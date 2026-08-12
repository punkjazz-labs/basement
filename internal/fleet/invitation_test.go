package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/store"
)

// The two-click addition, end to end, with nobody reading a code: the owner
// presses Add on the controller, the owner presses Approve on the member, and
// the machines do the rest over the channel the invitation pinned.
func TestTwoClickAdditionBuildsAFleetWithoutAnyoneReadingACode(t *testing.T) {
	ctx := context.Background()
	controller, controllerStore := newTestManager(t, "controller", "192.168.99.10")
	member, memberStore := newTestManager(t, "spark-worker", "192.168.99.20")
	wireInvitations(t, controller, member)

	invited, err := controller.Invite(ctx, InviteRequest{
		ConsoleURL: "http://192.168.99.20:7070", NodeURL: "https://192.168.99.20:7071",
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if invited.State != InviteStateWaiting || invited.DisplayName != "spark-worker" {
		t.Fatalf("first click did not leave the addition waiting: %+v", invited)
	}

	pending := member.PendingInvitations()
	if len(pending) != 1 || pending[0].ControllerName != "controller" || pending[0].ControllerConsoleURL != "http://192.168.99.10:7070" {
		t.Fatalf("the member console has nothing to approve: %+v", pending)
	}

	// Waiting changes no membership on either side.
	waiting, err := controller.InviteStatus(ctx, "http://192.168.99.20:7070")
	if err != nil || waiting.State != InviteStateWaiting {
		t.Fatalf("unapproved invitation state=%+v err=%v", waiting, err)
	}
	config, err := memberStore.FleetConfig(ctx)
	if err != nil || config.Role != "standalone" {
		t.Fatalf("an unanswered invitation joined the node: config=%+v err=%v", config, err)
	}

	if err := member.ApproveInvitation(ctx, pending[0].ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done, err := controller.InviteStatus(ctx, "http://192.168.99.20:7070")
	if err != nil {
		t.Fatalf("collect and adopt: %v", err)
	}
	if done.State != InviteStateDone || done.Node == nil || done.Node.NodeID != member.identity.NodeID || done.Node.MembershipState != "active" {
		t.Fatalf("approval did not finish the addition: %+v", done)
	}

	config, err = memberStore.FleetConfig(ctx)
	if err != nil || config.Role != "member" || config.ControllerNodeID != controller.identity.NodeID {
		t.Fatalf("member did not pin its controller: config=%+v err=%v", config, err)
	}
	nodes, err := controllerStore.FleetNodes(ctx)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("controller nodes=%d err=%v: %+v", len(nodes), err, nodes)
	}
	if listed := member.PendingInvitations(); len(listed) != 0 {
		t.Fatalf("an answered invitation is still waiting: %+v", listed)
	}
	// The code was handed over once. A second collection has nothing left.
	if answer := member.collectInvitation(controller.identity.CertificateFingerprint); answer.State != invitationExpired || answer.JoinCode != "" {
		t.Fatalf("the join code was deliverable twice: %+v", answer)
	}
}

// The owner's answer is the only thing that can change membership, so a denial
// has to end the addition rather than leave the controller trying.
func TestDeniedInvitationEndsTheAddition(t *testing.T) {
	ctx := context.Background()
	controller, controllerStore := newTestManager(t, "controller", "192.168.99.10")
	member, _ := newTestManager(t, "spark-worker", "192.168.99.20")
	wireInvitations(t, controller, member)

	if _, err := controller.Invite(ctx, InviteRequest{ConsoleURL: "http://192.168.99.20:7070", NodeURL: "https://192.168.99.20:7071"}); err != nil {
		t.Fatal(err)
	}
	pending := member.PendingInvitations()
	if len(pending) != 1 {
		t.Fatalf("invitations=%+v", pending)
	}
	if err := member.DenyInvitation(pending[0].ID); err != nil {
		t.Fatal(err)
	}
	denied, err := controller.InviteStatus(ctx, "http://192.168.99.20:7070")
	if err != nil || denied.State != InviteStateDenied {
		t.Fatalf("denied invitation state=%+v err=%v", denied, err)
	}
	nodes, err := controllerStore.FleetNodes(ctx)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("a denial changed membership: nodes=%+v err=%v", nodes, err)
	}
	if listed := member.PendingInvitations(); len(listed) != 0 {
		t.Fatalf("a denied invitation is still waiting: %+v", listed)
	}
}

// An invitation nobody answers has to run out on both sides, so a machine that
// was asked once does not stay adoptable for the rest of its uptime.
func TestUnansweredInvitationExpiresOnBothSides(t *testing.T) {
	ctx := context.Background()
	controller, _ := newTestManager(t, "controller", "192.168.99.10")
	member, _ := newTestManager(t, "spark-worker", "192.168.99.20")
	wireInvitations(t, controller, member)
	// Held against this machine's clock, because the node certificates in the
	// exchange are valid from the moment the test created them.
	start := time.Now()
	controller.now = func() time.Time { return start }
	member.now = func() time.Time { return start }

	if _, err := controller.Invite(ctx, InviteRequest{ConsoleURL: "http://192.168.99.20:7070", NodeURL: "https://192.168.99.20:7071"}); err != nil {
		t.Fatal(err)
	}
	later := start.Add(invitationLifetime + time.Minute)
	member.now = func() time.Time { return later }
	if listed := member.PendingInvitations(); len(listed) != 0 {
		t.Fatalf("an expired invitation is still offered for approval: %+v", listed)
	}
	if err := member.ApproveInvitation(ctx, "inv_whatever"); err == nil {
		t.Fatal("an expired invitation was approvable")
	}
	controller.now = func() time.Time { return later }
	expired, err := controller.InviteStatus(ctx, "http://192.168.99.20:7070")
	if err != nil || expired.State != InviteStateExpired {
		t.Fatalf("expired invitation state=%+v err=%v", expired, err)
	}
}

// The approval has to come from the machine that was asked. If the code that
// comes back names a different identity, the addition stops here rather than
// pinning a stranger into the fleet.
func TestApprovalFromAnotherIdentityIsRefused(t *testing.T) {
	ctx := context.Background()
	controller, controllerStore := newTestManager(t, "controller", "192.168.99.10")
	member, _ := newTestManager(t, "spark-worker", "192.168.99.20")
	stranger, _ := newTestManager(t, "stranger", "192.168.99.30")
	// First contact reports the stranger's certificate while the machine that
	// answers is the member: the pin and the approval disagree.
	controller.newFirstContactClient = inMemoryFirstContactClients(t, controller,
		map[string]*Manager{member.nodeURL: member}, stranger.identity.CertificateFingerprint)
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{stranger.identity.CertificateFingerprint: member})

	if _, err := controller.Invite(ctx, InviteRequest{ConsoleURL: "http://192.168.99.20:7070", NodeURL: "https://192.168.99.20:7071"}); err != nil {
		t.Fatal(err)
	}
	pending := member.PendingInvitations()
	if len(pending) != 1 {
		t.Fatalf("invitations=%+v", pending)
	}
	if err := member.ApproveInvitation(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	refused, err := controller.InviteStatus(ctx, "http://192.168.99.20:7070")
	if err != nil {
		t.Fatal(err)
	}
	if refused.State != InviteStateFailed || refused.Reason != "the approving Spark is not the one that was asked" {
		t.Fatalf("a mismatched approval was accepted: %+v", refused)
	}
	nodes, err := controllerStore.FleetNodes(ctx)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("a mismatched approval changed membership: nodes=%+v err=%v", nodes, err)
	}
}

// The invitation endpoint is the one door a Spark with no fleet opens to a
// certificate it has never seen. It has to close the moment that Spark belongs
// to a fleet, at the handshake and at the handler.
func TestInviteIsAcceptedFromAnUnknownCertificateOnlyWhileStandalone(t *testing.T) {
	ctx := context.Background()
	controller, _ := newTestManager(t, "controller", "192.168.99.10")
	member, memberStore := newTestManager(t, "spark-worker", "192.168.99.20")

	standalone := callInvite(t, member, controller, "controller", "http://192.168.99.10:7070")
	if standalone.Code != http.StatusOK {
		t.Fatalf("a standalone Spark refused an invitation: status=%d body=%s", standalone.Code, standalone.Body.String())
	}
	if err := member.allowPeerCertificate(ctx, controller.identity.Certificate); err != nil {
		t.Fatalf("a standalone Spark refused the handshake: %v", err)
	}

	joinMember(t, controller, member, memberStore)

	joined := callInvite(t, member, controller, "controller", "http://192.168.99.10:7070")
	if joined.Code != http.StatusConflict {
		t.Fatalf("a Spark in a fleet accepted an invitation: status=%d body=%s", joined.Code, joined.Body.String())
	}
	other, _ := newTestManager(t, "other", "192.168.99.30")
	if err := member.allowPeerCertificate(ctx, other.identity.Certificate); err == nil {
		t.Fatal("a Spark in a fleet completed a handshake with an unpinned certificate")
	}
}

// The answer belongs to the certificate that asked the question. Another
// machine on the same network must not be able to collect it.
func TestOnlyTheInvitingCertificateCollectsTheJoinCode(t *testing.T) {
	ctx := context.Background()
	controller, _ := newTestManager(t, "controller", "192.168.99.10")
	eavesdropper, _ := newTestManager(t, "eavesdropper", "192.168.99.30")
	member, _ := newTestManager(t, "spark-worker", "192.168.99.20")

	if response := callInvite(t, member, controller, "controller", "http://192.168.99.10:7070"); response.Code != http.StatusOK {
		t.Fatalf("invite status=%d body=%s", response.Code, response.Body.String())
	}
	pending := member.PendingInvitations()
	if len(pending) != 1 {
		t.Fatalf("invitations=%+v", pending)
	}
	if err := member.ApproveInvitation(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}

	stolen := decodeInviteStatus(t, callInviteStatus(t, member, eavesdropper))
	if stolen.State != invitationExpired || stolen.JoinCode != "" {
		t.Fatalf("another certificate collected the join code: %+v", stolen)
	}
	collected := decodeInviteStatus(t, callInviteStatus(t, member, controller))
	if collected.State != invitationApproved || collected.JoinCode == "" {
		t.Fatalf("the inviting certificate could not collect its code: %+v", collected)
	}
	if fingerprint, _, err := parseJoinCode(collected.JoinCode); err != nil || fingerprint != member.identity.CertificateFingerprint {
		t.Fatalf("the collected code does not name the approving Spark: %v", err)
	}
}

// Session cookies and published API keys are different authorities and never
// reach this transport, invitations included.
func TestInvitationRoutesRejectCookiesAndBearerKeys(t *testing.T) {
	member, _ := newTestManager(t, "spark-worker", "192.168.99.20")
	for _, endpoint := range []string{"/internal/fleet/v1/invite", "/internal/fleet/v1/invite/status"} {
		for _, header := range []struct{ name, value string }{
			{"Cookie", "basement_session=value"}, {"Authorization", "Bearer public-key"},
		} {
			request := httptest.NewRequest(http.MethodGet, "https://member.test"+endpoint, nil)
			request.Header.Set(header.name, header.value)
			response := httptest.NewRecorder()
			member.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Errorf("%s reached %s: status=%d", header.name, endpoint, response.Code)
			}
		}
	}
}

// A Spark with no fleet answers certificates it has never seen, so the queue
// an owner has to read through is bounded whatever the network sends, and one
// machine asking twice replaces its own request rather than filling it.
func TestInvitationsAreBoundedAndOneMachineHoldsOne(t *testing.T) {
	member, _ := newTestManager(t, "spark-worker", "192.168.99.20")
	first, _ := newTestManager(t, "first", "192.168.99.10")
	for round := 0; round < 3; round++ {
		if response := callInvite(t, member, first, "first", "http://192.168.99.10:7070"); response.Code != http.StatusOK {
			t.Fatalf("repeat invite status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if listed := member.PendingInvitations(); len(listed) != 1 {
		t.Fatalf("one machine filled the queue: %+v", listed)
	}
	second, _ := newTestManager(t, "second", "192.168.99.30")
	third, _ := newTestManager(t, "third", "192.168.99.40")
	fourth, _ := newTestManager(t, "fourth", "192.168.99.50")
	if response := callInvite(t, member, second, "second", "http://192.168.99.30:7070"); response.Code != http.StatusOK {
		t.Fatalf("second invite status=%d", response.Code)
	}
	if response := callInvite(t, member, third, "third", "http://192.168.99.40:7070"); response.Code != http.StatusOK {
		t.Fatalf("third invite status=%d", response.Code)
	}
	full := callInvite(t, member, fourth, "fourth", "http://192.168.99.50:7070")
	if full.Code != http.StatusTooManyRequests {
		t.Fatalf("a fourth invitation was accepted: status=%d body=%s", full.Code, full.Body.String())
	}
	if listed := member.PendingInvitations(); len(listed) != maxPendingInvitations {
		t.Fatalf("pending invitations=%+v", listed)
	}
}

// callInvite sends one invitation over the transport as caller, with caller's
// certificate as the mutual TLS client.
func callInvite(t *testing.T, target, caller *Manager, name, consoleURL string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(inviteRequest{FleetID: "", ControllerName: name, ControllerConsoleURL: consoleURL})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://member.test/internal/fleet/v1/invite", bytes.NewReader(payload))
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{caller.identity.Certificate}}
	response := httptest.NewRecorder()
	target.Handler().ServeHTTP(response, request)
	return response
}

func callInviteStatus(t *testing.T, target, caller *Manager) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://member.test/internal/fleet/v1/invite/status", nil)
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{caller.identity.Certificate}}
	response := httptest.NewRecorder()
	target.Handler().ServeHTTP(response, request)
	return response
}

func decodeInviteStatus(t *testing.T, response *httptest.ResponseRecorder) inviteStatusResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("invite status=%d body=%s", response.Code, response.Body.String())
	}
	var answer inviteStatusResponse
	if err := json.NewDecoder(strings.NewReader(response.Body.String())).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// joinMember puts the member in a fleet under the controller, without going
// through an adoption, so a test can assert what changes once it belongs to one.
func joinMember(t *testing.T, controller, member *Manager, memberStore *store.Store) {
	t.Helper()
	ctx := context.Background()
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
}

// wireInvitations stands the whole exchange up in memory: first contact finds
// the machine at the node URL and learns its real certificate, and every later
// call is pinned to that certificate.
func wireInvitations(t *testing.T, controller *Manager, members ...*Manager) {
	t.Helper()
	byFingerprint := map[string]*Manager{}
	byNodeURL := map[string]*Manager{}
	for _, member := range members {
		byFingerprint[member.identity.CertificateFingerprint] = member
		byNodeURL[member.nodeURL] = member
	}
	controller.newClient = inMemoryFleetClients(t, controller, byFingerprint)
	controller.newFirstContactClient = inMemoryFirstContactClients(t, controller, byNodeURL)
}

// inMemoryFirstContactClients is the unpinned half of the transport seam: it
// routes by address, because that is all a first contact has. An optional
// fingerprint stands in for what the address presented, which is how a test
// makes the pin and the answering machine disagree.
func inMemoryFirstContactClients(t *testing.T, caller *Manager, targets map[string]*Manager, observed ...string) func(func(string)) *http.Client {
	t.Helper()
	return func(observe func(string)) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			target := targets["https://"+request.URL.Host]
			if target == nil {
				return nil, errors.New("no in-memory machine listens there")
			}
			if len(observed) > 0 {
				observe(observed[0])
			} else {
				observe(target.identity.CertificateFingerprint)
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
