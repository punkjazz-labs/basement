package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/store"
	managerupdate "github.com/punkjazz-labs/basement/internal/update"
)

type staticReleaseSource struct {
	release  managerupdate.Release
	payloads map[string][]byte
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
