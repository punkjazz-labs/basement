package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/fleet"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
	managerupdate "github.com/punkjazz-labs/basement/internal/update"
)

type staticReleaseSource struct {
	release  managerupdate.Release
	payloads map[string][]byte
}

type updateRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip updateRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func (source staticReleaseSource) Releases(context.Context) ([]managerupdate.Release, error) {
	return []managerupdate.Release{source.release}, nil
}

func (source staticReleaseSource) Fetch(_ context.Context, location string, _ int64) ([]byte, error) {
	payload, ok := source.payloads[location]
	if !ok {
		return nil, fmt.Errorf("missing fixture %s", location)
	}
	return append([]byte(nil), payload...), nil
}

func TestUpdateCheckUsesSemanticVersionOrdering(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		current   string
		latest    string
		available bool
	}{
		{name: "newer local", current: "v3.0.0", latest: "v2.9.9", available: false},
		{name: "equal", current: "v2.0.0", latest: "v2.0.0", available: false},
		{name: "older local", current: "v1.9.9", latest: "v2.0.0", available: true},
		{name: "development build", current: "dev", latest: "v2.0.0", available: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rollbackFrom := test.current
			if _, err := managerupdate.ParseVersion(rollbackFrom); err != nil {
				rollbackFrom = "v1.0.0"
			}
			source := signedHTTPReleaseFixture(t, privateKey, test.latest, rollbackFrom)
			stager := managerupdate.NewStager(t.TempDir(), managerupdate.KeyRing{"release-test": publicKey})
			server := &Server{
				version:        test.current,
				updateResolver: &managerupdate.Resolver{Source: source, Keys: managerupdate.KeyRing{"release-test": publicKey}},
				updateStager:   stager,
			}
			request := httptest.NewRequest(http.MethodGet, "http://localhost/api/v1/update", nil)
			response := httptest.NewRecorder()
			server.updateCheck(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if available, _ := body["update_available"].(bool); available != test.available {
				t.Fatalf("update_available = %v, want %v; body = %s", available, test.available, response.Body.String())
			}
		})
	}
}

func signedHTTPReleaseFixture(t *testing.T, privateKey ed25519.PrivateKey, version, rollbackFrom string) staticReleaseSource {
	t.Helper()
	digest := sha256.Sum256([]byte(version))
	manifest, err := managerupdate.MarshalManifest(managerupdate.Manifest{
		SchemaVersion: managerupdate.ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: version,
		OS: "linux", Arch: "arm64", AssetName: managerupdate.LinuxARM64AssetName,
		AssetSize: int64(len(version)), AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: managerupdate.UpdaterProtocol,
		RollbackFrom: []string{rollbackFrom},
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))), '\n')
	prefix := "https://github.com/example/releases/download/" + version + "/"
	release := managerupdate.Release{TagName: version, HTMLURL: "https://github.com/example/releases/tag/" + version, Assets: []managerupdate.ReleaseAsset{
		{Name: managerupdate.ManifestAssetName, URL: prefix + managerupdate.ManifestAssetName},
		{Name: managerupdate.SignatureAssetName, URL: prefix + managerupdate.SignatureAssetName},
		{Name: managerupdate.LinuxARM64AssetName, URL: prefix + managerupdate.LinuxARM64AssetName},
	}}
	return staticReleaseSource{release: release, payloads: map[string][]byte{
		release.Assets[0].URL: manifest,
		release.Assets[1].URL: signature,
	}}
}

func TestApplyUpdateRefusesActiveWork(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *store.Store)
		want    string
	}{
		{
			name: "running install job",
			prepare: func(t *testing.T, database *store.Store) {
				job, _, err := database.CreateJob(context.Background(), "install", "model-test", "busy-install", map[string]any{})
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateJobState(context.Background(), job.ID, "running", ""); err != nil {
					t.Fatal(err)
				}
			},
			want: "install job",
		},
		{
			name: "queued generation",
			prepare: func(t *testing.T, database *store.Store) {
				if _, err := database.CreateGeneration(context.Background(), store.Generation{RecipeID: "media-test", Mode: "text-to-video", Prompt: "test"}); err != nil {
					t.Fatal(err)
				}
			},
			want: "generation",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			database, err := store.Open(filepath.Join(root, "manager.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			test.prepare(t, database)
			authManager, err := auth.Open(filepath.Join(root, "auth"))
			if err != nil {
				t.Fatal(err)
			}
			token, err := os.ReadFile(authManager.PairingTokenPath())
			if err != nil {
				t.Fatal(err)
			}
			paired := httptest.NewRecorder()
			csrf, err := authManager.Pair(paired, strings.TrimSpace(string(token)))
			if err != nil {
				t.Fatal(err)
			}
			stager := managerupdate.NewStager(root, nil)
			stager.RootStatusPath = filepath.Join(root, "root-status.json")
			server := &Server{auth: authManager, store: database, updateStager: stager}
			request := httptest.NewRequest(http.MethodPost, "http://localhost/api/v1/update", strings.NewReader("{}"))
			request.Header.Set("Origin", "http://localhost")
			request.Header.Set("X-CSRF-Token", csrf)
			for _, cookie := range paired.Result().Cookies() {
				request.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			server.applyUpdate(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body["error"], test.want) || !strings.Contains(body["error"], "finish or cancel it first") {
				t.Fatalf("error = %q", body["error"])
			}
		})
	}
}

func TestSingleNodeLocalUpdatePathUnaffectedByFleetUpgrade(t *testing.T) {
	root := t.TempDir()
	database, err := store.Open(filepath.Join(root, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authManager, err := auth.Open(filepath.Join(root, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	executor := &apiExecutor{done: map[string]bool{}}
	server := New("v1.0.0", root, authManager, database, readyInventory{}, executor, engine.New(database, executor, recipes), recipes)
	defer server.Close()
	manager, err := fleet.NewManager(context.Background(), fleet.Options{DataDir: root, Database: database, Inventory: readyInventory{},
		Version: "v1.0.0", BuildIdentity: "v1-build", DisplayName: "spark-head",
		ConsoleURL: "http://192.168.99.10:7070", NodeURL: "https://192.168.99.10:7071", Recipes: recipes})
	if err != nil {
		t.Fatal(err)
	}
	server.SetFleetManager(manager)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	asset := make([]byte, 20)
	copy(asset[:4], []byte{0x7f, 'E', 'L', 'F'})
	asset[18], asset[19] = 183, 0
	digest := sha256.Sum256(asset)
	manifestBytes, err := managerupdate.MarshalManifest(managerupdate.Manifest{SchemaVersion: managerupdate.ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: managerupdate.LinuxARM64AssetName, AssetSize: int64(len(asset)), AssetSHA256: hex.EncodeToString(digest[:]),
		UpdaterProtocol: managerupdate.UpdaterProtocol, RollbackFrom: []string{"v1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifestBytes))), '\n')
	verified, err := managerupdate.VerifySignedManifest(manifestBytes, signature, managerupdate.KeyRing{"release-test": publicKey})
	if err != nil {
		t.Fatal(err)
	}
	server.updateCandidate = &managerupdate.Candidate{Release: managerupdate.Release{TagName: "v2.0.0"}, Manifest: verified,
		ManifestBytes: manifestBytes, Signature: signature, AssetURL: "https://github.com/example/releases/download/v2.0.0/" + managerupdate.LinuxARM64AssetName}
	server.updateStager = managerupdate.NewStager(root, managerupdate.KeyRing{"release-test": publicKey})
	server.updateStager.BootstrapCheck = func() error { return nil }
	server.updateStager.Client = &http.Client{Transport: updateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(asset)), Body: io.NopCloser(bytes.NewReader(asset)), Header: make(http.Header)}, nil
	})}
	cookie, csrf := pairMembershipConsole(t, server, authManager)
	request := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/update/apply", strings.NewReader("{}"))
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://console.test")
	request.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("standalone update status=%d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, found, err := server.updateStager.Status()
		if err != nil {
			t.Fatal(err)
		}
		if found && (status.State == "waiting_for_root" || status.State == "failed_before_handoff") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("standalone update did not finish staging: %+v", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	server.Close()
	partial, err := os.ReadDir(filepath.Join(root, "updates", "staging", "partial"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 {
		t.Fatalf("update staging writer remained after server close: %v", partial)
	}
}

func TestUpdateCheckReportsFleetScope(t *testing.T) {
	t.Run("controller", func(t *testing.T) {
		server, manager, _ := membershipTestServer(t)
		assertUpdateFleetScope(t, server, "standalone", 0)
		identity := manager.Identity()
		self := store.FleetNode{
			NodeID: identity.NodeID, DisplayName: "spark-head",
			ConsoleURL: "http://192.168.99.10:7070", NodeURL: "https://192.168.99.10:7071",
			Certificate: identity.CertificatePEM, ManagerVersion: "test", ManagerBuildIdentity: "test-build",
		}
		config, err := server.store.EnsureFleetController(context.Background(), self)
		if err != nil {
			t.Fatal(err)
		}
		member := store.FleetNode{
			NodeID: "node-worker", DisplayName: "spark-worker",
			ConsoleURL: "http://192.168.99.11:7070", NodeURL: "https://192.168.99.11:7071",
			Certificate: []byte("member-certificate"), ManagerVersion: "test", ManagerBuildIdentity: "test-build",
		}
		if _, _, err := server.store.PrepareFleetNode(context.Background(), self, member); err != nil {
			t.Fatal(err)
		}
		if err := server.store.CommitFleetNode(context.Background(), config.FleetID, member.NodeID); err != nil {
			t.Fatal(err)
		}
		assertUpdateFleetScope(t, server, "controller", 2)
	})

	t.Run("standalone", func(t *testing.T) {
		server, _, _ := membershipTestServer(t)
		assertUpdateFleetScope(t, server, "standalone", 0)
	})

	t.Run("standalone without fleet manager", func(t *testing.T) {
		server := &Server{version: "dev"}
		assertUpdateFleetScope(t, server, "standalone", 0)
	})
}

func assertUpdateFleetScope(t *testing.T, server *Server, role string, nodeCount int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/update", nil)
	response := httptest.NewRecorder()
	server.updateCheck(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		FleetRole      string `json:"fleet_role"`
		FleetNodeCount int    `json:"fleet_node_count"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.FleetRole != role || body.FleetNodeCount != nodeCount {
		t.Fatalf("fleet scope=%+v, want role=%s count=%d", body, role, nodeCount)
	}
}

func TestFleetUpgradeStatusAPIReportsPerNodeVersionsMidRollout(t *testing.T) {
	server, manager, authManager := membershipTestServer(t)
	run := store.FleetUpgradeRun{RunID: "api-mid-rollout", FleetID: "fleet-test", ControllerNodeID: manager.Identity().NodeID,
		ReleaseTag: "v2.0.0", TargetVersion: "v2.0.0", ManifestSHA256: strings.Repeat("b", 64),
		ManifestBytes: []byte("manifest"), SignatureBytes: []byte("signature"), AssetURL: "https://github.com/example/asset"}
	nodes := []store.FleetUpgradeNode{
		{NodeID: "node-worker", DisplayName: "spark-worker", Sequence: 0, Role: "idle", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"},
		{NodeID: manager.Identity().NodeID, DisplayName: "spark-head", Sequence: 1, Role: "controller", RunningVersion: "v1.0.0", TargetVersion: "v2.0.0"},
	}
	if _, _, err := server.store.CreateFleetUpgradeRun(context.Background(), run, nodes); err != nil {
		t.Fatal(err)
	}
	if err := server.store.UpdateFleetUpgradeNode(context.Background(), run.RunID, "node-worker", "succeeded", "v2.0.0", "attempt-worker", ""); err != nil {
		t.Fatal(err)
	}
	if err := server.store.UpdateFleetUpgradeRunState(context.Background(), run.RunID, "applying", ""); err != nil {
		t.Fatal(err)
	}
	cookie, _ := pairMembershipConsole(t, server, authManager)
	request := httptest.NewRequest(http.MethodGet, "http://console.test/api/v1/fleet/upgrade", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body store.FleetUpgradeRun
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.State != "applying" || len(body.Nodes) != 2 || body.Nodes[0].RunningVersion != "v2.0.0" || body.Nodes[1].RunningVersion != "v1.0.0" {
		t.Fatalf("mid-rollout response=%+v", body)
	}
}
