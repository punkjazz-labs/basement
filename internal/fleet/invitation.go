package fleet

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/punkjazz-labs/basement/internal/store"
)

// Adding a Spark to the fleet is two clicks: the owner presses Add on the
// controller console, and the owner presses Approve on the member console.
// Nobody reads an address, a fingerprint or a join code out of one screen and
// types it into another. See docs/decisions/0019-two-click-fleet-add.md.
//
// The controller opens the conversation with an invitation over the fleet
// transport, pinning whatever certificate the member presents at first
// contact. The member holds that invitation until an owner session approves
// it, and approval is what mints the join code. The controller then collects
// that code machine to machine, over the channel it pinned, and runs the
// existing adoption. The code is never rendered anywhere.

const (
	// invitationLifetime is how long an unanswered invitation is worth
	// showing. It matches the join code it can become, so nothing an owner
	// approves is already stale, and one more click re-sends it.
	invitationLifetime = 10 * time.Minute

	// maxPendingInvitations caps how many invitations one standalone node
	// holds at a time. A node with no fleet accepts these from certificates
	// it has never seen, so the queue an owner has to read through is
	// bounded whatever the network sends.
	maxPendingInvitations = 3

	// maxRemoteNameLength is what the store accepts for a node name, and
	// what a name another machine reported for itself is held to.
	maxRemoteNameLength = 64
)

// The states one invitation passes through on the member.
const (
	invitationPending  = "pending"
	invitationApproved = "approved"
	invitationDenied   = "denied"
	invitationExpired  = "expired"
)

// The states the controller surfaces to its console while it adds a node.
const (
	InviteStateWaiting  = "waiting"
	InviteStateAdopting = "adopting"
	InviteStateDone     = "done"
	InviteStateDenied   = "denied"
	InviteStateExpired  = "expired"
	InviteStateFailed   = "failed"
)

// invitation is one controller's request to adopt this node, waiting for an
// owner to answer it. It lives in memory on purpose: it is a prompt a person
// is looking at, not a record, and a manager restart during those ten minutes
// costs one more click on the controller rather than a stale approval that
// outlives the conversation that produced it.
type invitation struct {
	id                   string
	requester            string
	fleetID              string
	controllerName       string
	controllerConsoleURL string
	createdAt            time.Time
	expiresAt            time.Time
	state                string
	joinCode             string
}

// inviteAttempt is the controller's side of the same conversation: one node it
// is trying to add, the certificate it pinned when it asked, and how far the
// exchange has got. Also memory only, for the same reason.
type inviteAttempt struct {
	consoleURL  string
	nodeURL     string
	displayName string
	nodeID      string
	fingerprint string
	state       string
	reason      string
	expiresAt   time.Time
	node        *store.FleetNode
	polling     bool
}

// InvitationSummary is one waiting invitation as the member console shows it.
// It carries no code and no certificate: the owner is answering a question
// about a machine, not handling a secret.
type InvitationSummary struct {
	ID                   string `json:"id"`
	ControllerName       string `json:"controller_name"`
	ControllerConsoleURL string `json:"controller_console_url"`
	ExpiresAt            string `json:"expires_at"`
}

// InviteRequest is one node the controller was asked to add. The console
// derives NodeURL from the console URL it already knows.
type InviteRequest struct {
	ConsoleURL  string
	NodeURL     string
	DisplayName string
}

// InviteProgress is what the controller console polls while a node is being
// added. Node is filled once the adoption has really finished.
type InviteProgress struct {
	ConsoleURL  string           `json:"console_url"`
	NodeURL     string           `json:"node_url"`
	DisplayName string           `json:"display_name"`
	NodeID      string           `json:"node_id"`
	State       string           `json:"state"`
	Reason      string           `json:"reason"`
	ExpiresAt   string           `json:"expires_at"`
	Node        *store.FleetNode `json:"node"`
}

// The invitation wire shapes. They travel on the fleet transport only, under
// mutual TLS, and never through the console API.
type inviteRequest struct {
	FleetID              string `json:"fleet_id"`
	ControllerName       string `json:"controller_name"`
	ControllerConsoleURL string `json:"controller_console_url"`
}

type inviteResponse struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name"`
	ExpiresAt   string `json:"expires_at"`
}

type inviteStatusResponse struct {
	State    string `json:"state"`
	JoinCode string `json:"join_code,omitempty"`
}

// inviteReceive records a controller's request to adopt this node. The caller
// is identified by its mutual TLS certificate and nothing else: no code, no
// token, no session. That certificate is unknown at this point, which is the
// whole point of the exchange, so this endpoint answers only while this node
// belongs to no fleet, holds at most a handful of these at a time, and changes
// no membership by itself.
func (m *Manager) inviteReceive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	fingerprint, err := peerFingerprint(r)
	if err != nil {
		writeFleetError(w, http.StatusUnauthorized, err)
		return
	}
	var request inviteRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	config, err := m.database.FleetConfig(r.Context())
	if err != nil {
		writeFleetError(w, http.StatusInternalServerError, err)
		return
	}
	if config.Role != "standalone" {
		writeFleetError(w, http.StatusConflict, errors.New("this Spark already belongs to a fleet"))
		return
	}
	controllerName := sanitizeRemoteName(request.ControllerName)
	if controllerName == "" {
		writeFleetError(w, http.StatusBadRequest, errors.New("the inviting Spark did not name itself"))
		return
	}
	controllerConsoleURL, err := normalizeOrigin(request.ControllerConsoleURL, false)
	if err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	expiresAt, err := m.recordInvitation(fingerprint, sanitizeRemoteName(request.FleetID), controllerName, controllerConsoleURL)
	if err != nil {
		writeFleetError(w, http.StatusTooManyRequests, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, inviteResponse{
		NodeID: m.identity.NodeID, DisplayName: m.displayName,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	})
}

// inviteStatus hands the answer back to the exact certificate that asked. An
// approved invitation carries its join code here and nowhere else: this is a
// mutual TLS channel to the one client the owner's approval was about, and the
// code is cleared as it is handed over, so it exists in one place at a time.
func (m *Manager) inviteStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fleetMethodNotAllowed(w)
		return
	}
	fingerprint, err := peerFingerprint(r)
	if err != nil {
		writeFleetError(w, http.StatusUnauthorized, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, m.collectInvitation(fingerprint))
}

// peerFingerprint is the identity of the mutual TLS client, which is the only
// authority these two endpoints recognise.
func peerFingerprint(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		return "", errors.New("fleet client certificate is required")
	}
	details, err := inspectCertificate(r.TLS.PeerCertificates[0])
	if err != nil {
		return "", err
	}
	return details.Fingerprint, nil
}

func (m *Manager) recordInvitation(fingerprint, fleetID, controllerName, controllerConsoleURL string) (time.Time, error) {
	m.invitationMu.Lock()
	defer m.invitationMu.Unlock()
	now := m.now()
	m.dropStaleInvitationsLocked(now)
	// One certificate holds one invitation. A controller whose owner clicked
	// Add twice replaces its own request rather than filling the queue.
	for id, held := range m.invitations {
		if held.requester == fingerprint {
			delete(m.invitations, id)
		}
	}
	if len(m.invitations) >= maxPendingInvitations {
		return time.Time{}, errors.New("this Spark is already holding three invitations, so answer one of those before sending another")
	}
	id, err := invitationID()
	if err != nil {
		return time.Time{}, err
	}
	expiresAt := now.Add(invitationLifetime)
	m.invitations[id] = &invitation{
		id: id, requester: fingerprint, fleetID: fleetID, controllerName: controllerName,
		controllerConsoleURL: controllerConsoleURL, createdAt: now, expiresAt: expiresAt,
		state: invitationPending,
	}
	return expiresAt, nil
}

func (m *Manager) collectInvitation(fingerprint string) inviteStatusResponse {
	m.invitationMu.Lock()
	defer m.invitationMu.Unlock()
	m.dropStaleInvitationsLocked(m.now())
	for id, held := range m.invitations {
		if held.requester != fingerprint {
			continue
		}
		if held.state != invitationApproved {
			return inviteStatusResponse{State: held.state}
		}
		code := held.joinCode
		// The code is delivered once. Whatever happens next, this
		// invitation has done its work and stops being an answer.
		delete(m.invitations, id)
		return inviteStatusResponse{State: invitationApproved, JoinCode: code}
	}
	// An invitation this node never held, already answered, or let expire is
	// the same thing to the caller: there is nothing waiting for it here.
	return inviteStatusResponse{State: invitationExpired}
}

func (m *Manager) dropStaleInvitationsLocked(now time.Time) {
	for id, held := range m.invitations {
		if now.After(held.expiresAt) {
			delete(m.invitations, id)
		}
	}
}

// PendingInvitations is what the member console lists for the owner: the
// controllers asking to adopt this Spark, oldest first.
func (m *Manager) PendingInvitations() []InvitationSummary {
	m.invitationMu.Lock()
	defer m.invitationMu.Unlock()
	m.dropStaleInvitationsLocked(m.now())
	summaries := make([]InvitationSummary, 0, len(m.invitations))
	ordered := make([]*invitation, 0, len(m.invitations))
	for _, held := range m.invitations {
		if held.state == invitationPending {
			ordered = append(ordered, held)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].createdAt.Before(ordered[j].createdAt) })
	for _, held := range ordered {
		summaries = append(summaries, InvitationSummary{
			ID: held.id, ControllerName: held.controllerName, ControllerConsoleURL: held.controllerConsoleURL,
			ExpiresAt: held.expiresAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return summaries
}

// ApproveInvitation is the owner's half of the exchange, and the only place a
// join code is ever minted for it. Approving changes no membership: it makes
// this node adoptable by one named controller for the rest of the invitation's
// life, and that controller still has to complete the join it already had to
// complete before.
func (m *Manager) ApproveInvitation(ctx context.Context, id string) error {
	m.invitationMu.Lock()
	defer m.invitationMu.Unlock()
	m.dropStaleInvitationsLocked(m.now())
	held, ok := m.invitations[id]
	if !ok || held.state != invitationPending {
		return errors.New("that invitation is no longer waiting for an answer")
	}
	code, err := m.CreateJoinCode(ctx)
	if err != nil {
		return err
	}
	held.state, held.joinCode = invitationApproved, code.Code
	return nil
}

// DenyInvitation discards one. The controller is told it was denied rather
// than left waiting for a timeout it cannot read anything into.
func (m *Manager) DenyInvitation(id string) error {
	m.invitationMu.Lock()
	defer m.invitationMu.Unlock()
	m.dropStaleInvitationsLocked(m.now())
	held, ok := m.invitations[id]
	if !ok || held.state != invitationPending {
		return errors.New("that invitation is no longer waiting for an answer")
	}
	held.state, held.joinCode = invitationDenied, ""
	return nil
}

// Invite asks another Spark to join this fleet. It is first contact: there is
// no pinned certificate for that address yet, so this pins whatever the fleet
// listener there presents and records it as the only identity this attempt
// will ever accept an approval from.
func (m *Manager) Invite(ctx context.Context, request InviteRequest) (InviteProgress, error) {
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return InviteProgress{}, err
	}
	consoleURL, err := normalizeOrigin(request.ConsoleURL, false)
	if err != nil {
		return InviteProgress{}, fmt.Errorf("console URL: %w", err)
	}
	nodeURL, err := normalizeOrigin(request.NodeURL, true)
	if err != nil {
		return InviteProgress{}, fmt.Errorf("node URL: %w", err)
	}
	if consoleURL == m.consoleURL || nodeURL == m.nodeURL {
		return InviteProgress{}, errors.New("that is the Spark you are using, so it is already in this fleet")
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return InviteProgress{}, err
	}
	if config.Role == "member" {
		return InviteProgress{}, errors.New("this Spark is a fleet member, so add nodes from the controller console instead")
	}
	var observed string
	client := m.newFirstContactClient(func(fingerprint string) { observed = fingerprint })
	var answer inviteResponse
	body := inviteRequest{FleetID: config.FleetID, ControllerName: m.displayName, ControllerConsoleURL: m.consoleURL}
	if err := callFleetJSON(ctx, client, http.MethodPost, nodeURL+"/internal/fleet/v1/invite", body, &answer); err != nil {
		return InviteProgress{}, fmt.Errorf("ask that Spark to join: %w", err)
	}
	if observed == "" {
		return InviteProgress{}, errors.New("that Spark did not present a fleet certificate to pin")
	}
	displayName := sanitizeRemoteName(request.DisplayName)
	if displayName == "" {
		displayName = sanitizeRemoteName(answer.DisplayName)
	}
	if displayName == "" {
		displayName = consoleHostName(consoleURL)
	}
	attempt := &inviteAttempt{
		// The console URL is kept exactly as it was asked for. A legacy peer
		// row is merged by its stored base URL, so rewriting it here would
		// leave the old row behind next to the new member.
		consoleURL: consoleURL, nodeURL: nodeURL, displayName: displayName,
		nodeID: sanitizeRemoteName(answer.NodeID), fingerprint: observed,
		state: InviteStateWaiting, expiresAt: m.now().Add(invitationLifetime),
	}
	m.attemptMu.Lock()
	m.dropFinishedAttemptsLocked()
	m.attempts[consoleURL] = attempt
	progress := attempt.progress()
	m.attemptMu.Unlock()
	return progress, nil
}

// InviteStatus advances one attempt. It is called by the controller console
// while the owner watches, so it does the one step that is possible right now
// and returns: ask the member whether it has been answered, and, once it has,
// run the adoption the join code makes possible.
func (m *Manager) InviteStatus(ctx context.Context, consoleURL string) (InviteProgress, error) {
	normalized, err := normalizeOrigin(consoleURL, false)
	if err != nil {
		return InviteProgress{}, fmt.Errorf("console URL: %w", err)
	}
	m.attemptMu.Lock()
	attempt, ok := m.attempts[normalized]
	if !ok {
		m.attemptMu.Unlock()
		return InviteProgress{}, errors.New("this Spark is not adding that machine")
	}
	if attempt.polling || finishedInviteState(attempt.state) {
		progress := attempt.progress()
		m.attemptMu.Unlock()
		return progress, nil
	}
	if m.now().After(attempt.expiresAt) {
		attempt.state, attempt.reason = InviteStateExpired, "nobody approved it on that Spark within ten minutes"
		progress := attempt.progress()
		m.attemptMu.Unlock()
		return progress, nil
	}
	attempt.polling = true
	nodeURL, fingerprint, displayName := attempt.nodeURL, attempt.fingerprint, attempt.displayName
	m.attemptMu.Unlock()

	state, reason, node := m.advanceInvite(ctx, normalized, nodeURL, fingerprint, displayName)

	m.attemptMu.Lock()
	defer m.attemptMu.Unlock()
	attempt.polling = false
	attempt.state, attempt.reason = state, reason
	if node != nil {
		attempt.node, attempt.nodeID = node, node.NodeID
	}
	return attempt.progress(), nil
}

// advanceInvite runs the network half of one poll outside the attempt lock.
func (m *Manager) advanceInvite(ctx context.Context, consoleURL, nodeURL, fingerprint, displayName string) (string, string, *store.FleetNode) {
	var answer inviteStatusResponse
	client := m.newClient(fingerprint)
	if err := callFleetJSON(ctx, client, http.MethodGet, nodeURL+"/internal/fleet/v1/invite/status", nil, &answer); err != nil {
		// A single unanswered poll is a network blip, not a decision. The
		// attempt keeps waiting and the console can say why it is quiet.
		return InviteStateWaiting, err.Error(), nil
	}
	switch answer.State {
	case invitationPending:
		return InviteStateWaiting, "", nil
	case invitationDenied:
		return InviteStateDenied, "the owner of that Spark declined", nil
	case invitationApproved:
		codeFingerprint, _, err := parseJoinCode(answer.JoinCode)
		if err != nil {
			return InviteStateFailed, "that Spark returned an unreadable join code", nil
		}
		if codeFingerprint != fingerprint {
			// The approval came back over the pinned channel but names a
			// different identity. That is the machine-in-the-middle case, and
			// it stops here rather than at the far end of an adoption.
			return InviteStateFailed, "the approving Spark is not the one that was asked", nil
		}
		result, err := m.Adopt(ctx, AdoptRequest{
			DisplayName: displayName, ConsoleURL: consoleURL, NodeURL: nodeURL, JoinCode: answer.JoinCode,
		})
		if err != nil {
			return InviteStateFailed, err.Error(), nil
		}
		node := result.Node
		return InviteStateDone, "", &node
	default:
		return InviteStateExpired, "that Spark is no longer holding the invitation", nil
	}
}

func finishedInviteState(state string) bool {
	switch state {
	case InviteStateDone, InviteStateDenied, InviteStateExpired, InviteStateFailed:
		return true
	}
	return false
}

// dropFinishedAttemptsLocked keeps the controller's memory of attempts to the
// ones a console could still be watching.
func (m *Manager) dropFinishedAttemptsLocked() {
	now := m.now()
	for key, attempt := range m.attempts {
		if attempt.polling {
			continue
		}
		if finishedInviteState(attempt.state) && now.After(attempt.expiresAt) {
			delete(m.attempts, key)
		}
	}
}

func (attempt *inviteAttempt) progress() InviteProgress {
	return InviteProgress{
		ConsoleURL: attempt.consoleURL, NodeURL: attempt.nodeURL, DisplayName: attempt.displayName,
		NodeID: attempt.nodeID, State: attempt.state, Reason: attempt.reason,
		ExpiresAt: attempt.expiresAt.UTC().Format(time.RFC3339Nano), Node: attempt.node,
	}
}

// firstContactClient trusts the fleet certificate an address presents and
// reports its fingerprint to the caller. Nothing is pinned before an invitation
// is sent, so this is the one call that learns a pin instead of proving one;
// every later call in the exchange goes through the pinned client, and the
// join code carries its own fingerprint, which has to be this same one.
func (m *Manager) firstContactClient(observe func(string)) *http.Client {
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{m.identity.TLSCertificate()},
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("fleet TLS requires one server certificate")
			}
			certificate := state.PeerCertificates[0]
			details, err := inspectCertificate(certificate)
			if err != nil {
				return err
			}
			now := m.now()
			if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
				return errors.New("fleet server certificate is outside its validity period")
			}
			observe(details.Fingerprint)
			return nil
		},
	}}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func invitationID() (string, error) {
	payload := make([]byte, 8)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return "inv_" + hex.EncodeToString(payload), nil
}

func consoleHostName(consoleURL string) string {
	parsed, err := url.Parse(consoleURL)
	if err != nil || parsed.Hostname() == "" {
		return "new Spark"
	}
	return sanitizeRemoteName(parsed.Hostname())
}

// sanitizeRemoteName holds text another machine chose to something this
// manager can store and show: control characters and invalid UTF-8 removed,
// length capped on a rune boundary to what the store accepts.
func sanitizeRemoteName(raw string) string {
	var builder strings.Builder
	for _, symbol := range strings.TrimSpace(raw) {
		if unicode.IsControl(symbol) || symbol == utf8.RuneError {
			continue
		}
		if builder.Len()+utf8.RuneLen(symbol) > maxRemoteNameLength {
			break
		}
		builder.WriteRune(symbol)
	}
	return strings.TrimSpace(builder.String())
}
