package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/inventory"
	"github.com/punkjazz-labs/basement/internal/store"
)

type fixedInventory struct{ hostname string }

func (f fixedInventory) Inspect(context.Context) (inventory.System, error) {
	return inventory.System{Hostname: f.hostname, Architecture: "aarch64", Ready: true, ObservedAt: "2026-08-05T10:00:00Z"}, nil
}

func TestIdentitySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "manager.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewManager(ctx, testManagerOptions(directory, database, "192.168.99.10"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnsureFleetController(ctx, first.selfNode("")); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	second, err := NewManager(ctx, testManagerOptions(directory, database, "192.168.99.10"))
	if err != nil {
		t.Fatal(err)
	}
	if first.identity.NodeID != second.identity.NodeID || first.identity.CertificateFingerprint != second.identity.CertificateFingerprint || !first.identity.PublicKey.Equal(second.identity.PublicKey) {
		t.Fatalf("identity changed across restart: first=%s second=%s", first.identity.NodeID, second.identity.NodeID)
	}
}

func TestIdentitySurvivesAddressChange(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "manager.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewManager(ctx, testManagerOptions(directory, database, "192.168.99.10"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnsureFleetController(ctx, first.selfNode("")); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	second, err := NewManager(ctx, testManagerOptions(directory, database, "192.168.99.42"))
	if err != nil {
		t.Fatal(err)
	}
	if first.identity.NodeID != second.identity.NodeID || first.identity.CertificateFingerprint != second.identity.CertificateFingerprint {
		t.Fatalf("address change replaced identity: first=%s second=%s", first.identity.NodeID, second.identity.NodeID)
	}
	config, err := database.FleetConfig(ctx)
	if err != nil || config.ControllerNodeURL != "https://192.168.99.42:7071" {
		t.Fatalf("address changed identity instead of its route: config=%+v err=%v", config, err)
	}
}

func TestNodeCannotJoinWithoutOwnerAdoption(t *testing.T) {
	ctx := context.Background()
	controller, controllerStore := newTestManager(t, "controller", "192.168.99.10")
	attacker, _ := newTestManager(t, "unadopted", "192.168.99.30")
	config, err := controllerStore.EnsureFleetController(ctx, controller.selfNode(""))
	if err != nil {
		t.Fatal(err)
	}
	payload := testHeartbeatPayload(config.FleetID, attacker.identity.NodeID, 1)
	envelope, err := SignHeartbeat(attacker.identity, payload)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := envelope.signedBytes()
	err = controllerStore.AcceptHeartbeat(ctx, config.FleetID, attacker.identity.NodeID, 1, "2026-08-05T10:00:01Z", canonical, envelope.Signature, "test", "test-build", "catalogue")
	if err == nil || err.Error() != "the heartbeat node is not an adopted fleet member" {
		t.Fatalf("unadopted node heartbeat returned %v", err)
	}
	nodes, err := controllerStore.FleetNodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].NodeID != controller.identity.NodeID {
		t.Fatalf("unadopted identity changed membership: nodes=%+v err=%v", nodes, err)
	}
}

func TestHeartbeatWithBadSignatureIsRejected(t *testing.T) {
	controller, _ := newTestManager(t, "controller", "192.168.99.10")
	member, _ := newTestManager(t, "member", "192.168.99.20")
	payload := testHeartbeatPayload("fleet_test", member.identity.NodeID, 1)
	envelope, err := SignHeartbeat(member.identity, payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Signature[0] ^= 0xff
	if err := VerifyHeartbeat(envelope, member.identity.PublicKey, "fleet_test", member.identity.NodeID); err == nil || err.Error() != "heartbeat signature is invalid" {
		t.Fatalf("bad signature returned %v", err)
	}
	if err := VerifyHeartbeat(envelope, controller.identity.PublicKey, "fleet_test", member.identity.NodeID); err == nil {
		t.Fatal("another node's public key accepted the heartbeat")
	}
}

func TestHeartbeatReplayFromEarlierMomentIsRejected(t *testing.T) {
	ctx := context.Background()
	controller, controllerStore := newTestManager(t, "controller", "192.168.99.10")
	member, _ := newTestManager(t, "member", "192.168.99.20")
	config, err := controllerStore.EnsureFleetController(ctx, controller.selfNode(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controllerStore.PrepareFleetNode(ctx, controller.selfNode(config.FleetID), member.selfNode("")); err != nil {
		t.Fatal(err)
	}
	if err := controllerStore.CommitFleetNode(ctx, config.FleetID, member.identity.NodeID); err != nil {
		t.Fatal(err)
	}
	envelope, err := SignHeartbeat(member.identity, testHeartbeatPayload(config.FleetID, member.identity.NodeID, 7))
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := envelope.signedBytes()
	if err := VerifyHeartbeat(envelope, member.identity.PublicKey, config.FleetID, member.identity.NodeID); err != nil {
		t.Fatal(err)
	}
	if err := controllerStore.AcceptHeartbeat(ctx, config.FleetID, member.identity.NodeID, 7, "2026-08-05T10:00:01Z", canonical, envelope.Signature, "test", "test-build", "catalogue"); err != nil {
		t.Fatal(err)
	}
	if err := controllerStore.AcceptHeartbeat(ctx, config.FleetID, member.identity.NodeID, 7, "2026-08-05T10:00:02Z", canonical, envelope.Signature, "test", "test-build", "catalogue"); !errors.Is(err, store.ErrHeartbeatReplay) {
		t.Fatalf("replayed heartbeat returned %v", err)
	}
	nodes, _ := controllerStore.FleetNodes(ctx)
	for _, node := range nodes {
		if node.NodeID == member.identity.NodeID && node.HeartbeatReceivedAt != "2026-08-05T10:00:01Z" {
			t.Fatalf("replay replaced receipt time: %+v", node)
		}
	}
}

func TestMutualTLSHandshakeWithWrongCertificateFailsClosed(t *testing.T) {
	ctx := context.Background()
	controller, _ := newTestManager(t, "controller", "192.168.99.10")
	member, memberStore := newTestManager(t, "member", "192.168.99.20")
	wrong, _ := newTestManager(t, "wrong", "192.168.99.30")
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := parseJoinCode(code.Code)
	if err != nil {
		t.Fatal(err)
	}
	pending := store.PendingFleetJoin{
		PrepareTokenHash: "prepare-hash", FleetID: "fleet_test", ControllerNodeID: controller.identity.NodeID,
		ControllerConsoleURL: controller.consoleURL, ControllerNodeURL: controller.nodeURL,
		ControllerCertificate: controller.identity.CertificatePEM, ControllerCertificateFingerprint: controller.identity.CertificateFingerprint,
		MembershipEpoch: 1, ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}
	if err := memberStore.PrepareMemberJoin(ctx, hashSecret(secret), pending, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := memberStore.CommitMemberJoin(ctx, pending.PrepareTokenHash, time.Now()); err != nil {
		t.Fatal(err)
	}
	wrongClient := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{wrong.identity.TLSCertificate()}, InsecureSkipVerify: true}
	serverErr, clientErr := handshakePair(member.TLSConfig(), wrongClient)
	if serverErr == nil {
		t.Fatalf("wrong certificate handshake did not fail closed: server=%v client=%v", serverErr, clientErr)
	}
	correctClient := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{controller.identity.TLSCertificate()}, InsecureSkipVerify: true}
	serverErr, clientErr = handshakePair(member.TLSConfig(), correctClient)
	if serverErr != nil || clientErr != nil {
		t.Fatalf("pinned certificate handshake failed: server=%v client=%v", serverErr, clientErr)
	}
}

func TestHeartbeatRejectsWrongFleetAndAlteredPayload(t *testing.T) {
	member, _ := newTestManager(t, "member", "192.168.99.20")
	envelope, err := SignHeartbeat(member.identity, testHeartbeatPayload("fleet_one", member.identity.NodeID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyHeartbeat(envelope, member.identity.PublicKey, "fleet_two", member.identity.NodeID); err == nil || err.Error() != "heartbeat names the wrong fleet" {
		t.Fatalf("wrong fleet returned %v", err)
	}
	envelope.Payload.ManagerVersion = "altered"
	if err := VerifyHeartbeat(envelope, member.identity.PublicKey, "fleet_one", member.identity.NodeID); err == nil || err.Error() != "heartbeat signature is invalid" {
		t.Fatalf("altered payload returned %v", err)
	}
}

func TestFleetJoinCodeIsOneUseAndDoesNotJoinAtPrepare(t *testing.T) {
	ctx := context.Background()
	controller, _ := newTestManager(t, "controller", "192.168.99.10")
	member, memberStore := newTestManager(t, "member", "192.168.99.20")
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := parseJoinCode(code.Code)
	if err != nil {
		t.Fatal(err)
	}
	requestBody := joinPrepareRequest{
		Version: ProtocolVersion, FleetID: "fleet_test", MembershipEpoch: 1,
		ControllerNodeID: controller.identity.NodeID, ControllerConsoleURL: controller.consoleURL,
		ControllerNodeURL: controller.nodeURL, ControllerCertificate: controller.identity.CertificatePEM, JoinSecret: secret,
	}
	payload, _ := json.Marshal(requestBody)
	callPrepare := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "https://member.test/internal/fleet/v1/join/prepare", bytes.NewReader(payload))
		request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{controller.identity.Certificate}}
		response := httptest.NewRecorder()
		member.Handler().ServeHTTP(response, request)
		return response
	}
	first := callPrepare()
	if first.Code != http.StatusOK {
		t.Fatalf("first prepare status=%d body=%s", first.Code, first.Body.String())
	}
	config, err := memberStore.FleetConfig(ctx)
	if err != nil || config.Role != "standalone" {
		t.Fatalf("prepare joined the node without controller commit: config=%+v err=%v", config, err)
	}
	second := callPrepare()
	if second.Code != http.StatusForbidden {
		t.Fatalf("join code was reusable: status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestFleetTransportRejectsCookiesBearerKeysAndPublicRoutes(t *testing.T) {
	manager, _ := newTestManager(t, "member", "192.168.99.20")
	other, _ := newTestManager(t, "controller", "192.168.99.10")
	invite, _ := json.Marshal(map[string]string{
		"fleet_id":               "fleet-under-test",
		"controller_name":        "controller",
		"controller_console_url": "http://192.168.99.10:7070",
	})
	callInvite := func(headerName, headerValue string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "https://member.test/internal/fleet/v1/invite", bytes.NewReader(invite))
		request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{other.identity.Certificate}}
		if headerName != "" {
			request.Header.Set(headerName, headerValue)
		}
		response := httptest.NewRecorder()
		manager.Handler().ServeHTTP(response, request)
		return response
	}
	// The control carries a valid client certificate and an acceptable body,
	// so the rejections below are attributable to the header alone. Without
	// it, this test would pass even if the header check did not exist, since
	// a certificate-less request is unauthorized for its own reason.
	if control := callInvite("", ""); control.Code != http.StatusOK {
		t.Fatalf("control invite refused: status=%d body=%s", control.Code, control.Body.String())
	}
	for _, header := range []struct {
		name  string
		value string
	}{{"Cookie", "basement_session=value"}, {"Authorization", "Bearer public-key"}} {
		if response := callInvite(header.name, header.value); response.Code != http.StatusUnauthorized {
			t.Errorf("%s authority reached fleet transport despite valid mutual TLS: status=%d", header.name, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://member.test/api/v1/keys", nil)
	response := httptest.NewRecorder()
	manager.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("fleet mux exposed a public route: status=%d", response.Code)
	}
}

func TestOwnerInitiatedAdoptionBuildsFourNodeFleet(t *testing.T) {
	ctx := context.Background()
	controller, controllerStore := newTestManager(t, "controller", "192.168.99.10")
	type target struct {
		manager  *Manager
		database *store.Store
		address  string
		name     string
	}
	var targets []target
	byFingerprint := map[string]*Manager{}
	for _, item := range []struct {
		name    string
		address string
	}{{"spark-worker", "192.168.99.20"}, {"edgexpert-alpha", "192.168.99.21"}, {"edgexpert-beta", "192.168.99.22"}} {
		manager, database := newTestManager(t, item.name, item.address)
		targets = append(targets, target{manager: manager, database: database, address: item.address, name: item.name})
		byFingerprint[manager.identity.CertificateFingerprint] = manager
	}
	controller.newClient = inMemoryFleetClients(t, controller, byFingerprint)
	for _, target := range targets {
		code, err := target.manager.CreateJoinCode(ctx)
		if err != nil {
			t.Fatal(err)
		}
		result, err := controller.Adopt(ctx, AdoptRequest{
			DisplayName: target.name, ConsoleURL: "http://" + target.address + ":7070",
			NodeURL: "https://" + target.address + ":7071", JoinCode: code.Code,
		})
		if err != nil {
			t.Fatalf("adopt %s: %v", target.name, err)
		}
		if result.Node.NodeID != target.manager.identity.NodeID || result.Node.MembershipState != "active" {
			t.Fatalf("unexpected adopted node: %+v", result.Node)
		}
		config, err := target.database.FleetConfig(ctx)
		if err != nil || config.Role != "member" || config.ControllerNodeID != controller.identity.NodeID {
			t.Fatalf("member did not pin controller: config=%+v err=%v", config, err)
		}
	}
	nodes, err := controllerStore.FleetNodes(ctx)
	if err != nil || len(nodes) != store.MaxFleetNodes {
		t.Fatalf("controller nodes=%d err=%v: %+v", len(nodes), err, nodes)
	}
}

// Once the legacy peer has really joined, the owner must stop being told that
// a migration is still pending, because the summary is what the console shows.
func TestAdoptingTheLegacyPeerClearsThePendingMigrationSummary(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	controllerStore, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controllerStore.Close() })
	member, _ := newTestManager(t, "spark-worker", "192.168.99.20")
	if _, err := controllerStore.CreatePeer(ctx, "spark-worker", "http://192.168.99.20:7070", "legacy-secret"); err != nil {
		t.Fatal(err)
	}
	options := testManagerOptions(directory, controllerStore, "192.168.99.10")
	options.DisplayName = "controller"
	controller, err := NewManager(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := controller.Summary(ctx)
	if err != nil || pending.MigrationState != "legacy-pending" {
		t.Fatalf("legacy peer was not reported as pending: %+v err=%v", pending, err)
	}
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Adopt(ctx, AdoptRequest{
		DisplayName: "spark-worker", ConsoleURL: "http://192.168.99.20:7070",
		NodeURL: "https://192.168.99.20:7071", JoinCode: code.Code,
	})
	if err != nil {
		t.Fatalf("adopt the legacy peer: %v", err)
	}
	if result.Node.NodeID != member.identity.NodeID || result.Node.MembershipState != "active" {
		t.Fatalf("unexpected adopted node: %+v", result.Node)
	}
	summary, err := controller.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.MigrationState != "ready" {
		t.Fatalf("the migration stayed pending after the peer joined: %+v", summary)
	}
	if len(summary.Nodes) != 2 {
		t.Fatalf("the legacy placeholder is still listed: %+v", summary.Nodes)
	}
}

func TestFleetSummaryUsesReceiptFreshnessAndReportsClockSkew(t *testing.T) {
	ctx := context.Background()
	controller, controllerStore := newTestManager(t, "controller", "192.168.99.10")
	member, _ := newTestManager(t, "member", "192.168.99.20")
	config, err := controllerStore.EnsureFleetController(ctx, controller.selfNode(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controllerStore.PrepareFleetNode(ctx, controller.selfNode(config.FleetID), member.selfNode("")); err != nil {
		t.Fatal(err)
	}
	if err := controllerStore.CommitFleetNode(ctx, config.FleetID, member.identity.NodeID); err != nil {
		t.Fatal(err)
	}
	payload := testHeartbeatPayload(config.FleetID, member.identity.NodeID, 1)
	payload.LocalTime = "2026-08-05T09:55:00Z"
	envelope, err := SignHeartbeat(member.identity, payload)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := envelope.signedBytes()
	if err := controllerStore.AcceptHeartbeat(ctx, config.FleetID, member.identity.NodeID, 1, "2026-08-05T10:00:00Z", canonical, envelope.Signature, "test", "test-build", "catalogue"); err != nil {
		t.Fatal(err)
	}
	controller.now = func() time.Time { return time.Date(2026, 8, 5, 10, 0, 20, 0, time.UTC) }
	summary, err := controller.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	memberSummary := findNodeSummary(t, summary, member.identity.NodeID)
	if memberSummary.Status != "fresh" || !memberSummary.ClockSkew {
		t.Fatalf("fresh skewed member summary=%+v", memberSummary)
	}
	controller.now = func() time.Time { return time.Date(2026, 8, 5, 10, 1, 0, 0, time.UTC) }
	summary, err = controller.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	memberSummary = findNodeSummary(t, summary, member.identity.NodeID)
	if memberSummary.Status != "stale" {
		t.Fatalf("old controller receipt was not stale: %+v", memberSummary)
	}
}

func newTestManager(t *testing.T, name, address string) (*Manager, *store.Store) {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	options := testManagerOptions(directory, database, address)
	options.DisplayName = name
	manager, err := NewManager(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return manager, database
}

func testManagerOptions(directory string, database *store.Store, address string) Options {
	return Options{
		DataDir: directory, Database: database, Inventory: fixedInventory{hostname: "spark-head"},
		Version: "test", BuildIdentity: "test-build", DisplayName: "spark-head",
		ConsoleURL: "http://" + address + ":7070", NodeURL: "https://" + address + ":7071",
	}
}

func testHeartbeatPayload(fleetID, nodeID string, sequence int64) HeartbeatPayload {
	return HeartbeatPayload{
		Version: ProtocolVersion, FleetID: fleetID, NodeID: nodeID, ManagerVersion: "test",
		ManagerBuildIdentity: "test-build", Sequence: sequence, LocalTime: "2026-08-05T10:00:00Z",
		Inventory: inventory.System{Hostname: "spark-worker", Ready: true}, InstalledModels: []ModelSnapshot{},
		Allocations: []AllocationSnapshot{}, CatalogueDigest: "catalogue",
	}
}

func findNodeSummary(t *testing.T, summary Summary, nodeID string) FleetNodeSummary {
	t.Helper()
	for _, node := range summary.Nodes {
		if node.NodeID == nodeID {
			return node
		}
	}
	t.Fatalf("summary has no node %s: %+v", nodeID, summary)
	return FleetNodeSummary{}
}

func handshakePair(serverConfig, clientConfig *tls.Config) (error, error) {
	serverSide, clientSide := net.Pipe()
	deadline := time.Now().Add(time.Second)
	_ = serverSide.SetDeadline(deadline)
	_ = clientSide.SetDeadline(deadline)
	serverResult := make(chan error, 1)
	clientResult := make(chan error, 1)
	go func() {
		connection := tls.Server(serverSide, serverConfig)
		serverResult <- connection.Handshake()
		connection.Close()
	}()
	go func() {
		connection := tls.Client(clientSide, clientConfig)
		clientResult <- connection.Handshake()
		connection.Close()
	}()
	return <-serverResult, <-clientResult
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func inMemoryFleetClients(t *testing.T, caller *Manager, targets map[string]*Manager) func(string) *http.Client {
	t.Helper()
	return func(expectedFingerprint string) *http.Client {
		target := targets[expectedFingerprint]
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if target == nil {
				return nil, errors.New("no in-memory target has that certificate")
			}
			if err := target.allowPeerCertificate(request.Context(), caller.identity.Certificate); err != nil {
				return nil, err
			}
			request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{caller.identity.Certificate}}
			recorder := httptest.NewRecorder()
			target.Handler().ServeHTTP(recorder, request)
			response := recorder.Result()
			response.Request = request
			return response, nil
		})}
	}
}
