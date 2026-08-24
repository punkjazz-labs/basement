package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/store"
)

const maxFleetBody = 1 << 20

type joinPrepareRequest struct {
	Version               int    `json:"version"`
	FleetID               string `json:"fleet_id"`
	MembershipEpoch       int64  `json:"membership_epoch"`
	ControllerNodeID      string `json:"controller_node_id"`
	ControllerConsoleURL  string `json:"controller_console_url"`
	ControllerNodeURL     string `json:"controller_node_url"`
	ControllerCertificate []byte `json:"controller_certificate"`
	JoinSecret            string `json:"join_secret"`
}

type joinPrepareResponse struct {
	NodeID               string `json:"node_id"`
	Certificate          []byte `json:"certificate"`
	ManagerVersion       string `json:"manager_version"`
	ManagerBuildIdentity string `json:"manager_build_identity"`
	CatalogueDigest      string `json:"catalogue_digest"`
	PrepareToken         string `json:"prepare_token"`
}

type joinTokenRequest struct {
	PrepareToken string `json:"prepare_token"`
}

// TLSConfig authenticates manager traffic before HTTP sees it. A standalone
// node permits an unknown self-signed Ed25519 client certificate to complete
// TLS, because that is how a Spark that belongs to no fleet is asked to join
// one: /invite records the request for an owner to answer, and /join/prepare
// binds the certificate to a code an owner approved. Such a certificate can
// call no regular endpoint. Every other handler on this transport separately
// requires this node to be a member and the caller to be its adopted
// controller, so admission here is never authority there. Once enrolled, only
// pinned certificates complete another handshake.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.identity.TLSCertificate()},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("fleet TLS requires one client certificate")
			}
			return m.allowPeerCertificate(context.Background(), state.PeerCertificates[0])
		},
	}
}

func (m *Manager) allowPeerCertificate(ctx context.Context, certificate *x509.Certificate) error {
	if _, err := inspectCertificate(certificate); err != nil {
		return err
	}
	now := m.now()
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return errors.New("fleet certificate is outside its validity period")
	}
	pinned, err := m.database.PinnedFleetCertificates(ctx, now)
	if err != nil {
		return err
	}
	for _, encoded := range pinned {
		stored, _, err := ParseCertificatePEM(encoded)
		if err == nil && bytes.Equal(stored.Raw, certificate.Raw) {
			return nil
		}
	}
	config, err := m.database.FleetConfig(ctx)
	if err != nil {
		return err
	}
	if config.Role == "standalone" {
		return nil
	}
	return errors.New("fleet client certificate is not pinned")
}

func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/fleet/v1/join/prepare", m.joinPrepare)
	mux.HandleFunc("/internal/fleet/v1/join/commit", m.joinCommit)
	mux.HandleFunc("/internal/fleet/v1/join/abort", m.joinAbort)
	mux.HandleFunc("/internal/fleet/v1/invite", m.inviteReceive)
	mux.HandleFunc("/internal/fleet/v1/invite/status", m.inviteStatus)
	mux.HandleFunc("/internal/fleet/v1/heartbeat", m.heartbeat)
	mux.HandleFunc("/internal/fleet/v1/reservations/prepare", m.reservationPrepare)
	mux.HandleFunc("/internal/fleet/v1/reservations/commit", m.reservationCommit)
	mux.HandleFunc("/internal/fleet/v1/reservations/abort", m.reservationAbort)
	mux.HandleFunc("/internal/fleet/v1/deployments/independent", m.independentDeployment)
	mux.HandleFunc("/internal/fleet/v1/deployments/adopt", m.independentDeploymentAdopt)
	mux.HandleFunc("/internal/fleet/v1/jobs/", m.independentJob)
	mux.HandleFunc("/internal/fleet/v1/upgrade/stage", m.upgradeStage)
	mux.HandleFunc("/internal/fleet/v1/upgrade/apply", m.upgradeApply)
	mux.HandleFunc("/internal/fleet/v1/upgrade/status", m.upgradeStatus)
	mux.HandleFunc("/internal/fleet/v1/upgrade/finish", m.upgradeFinish)
	mux.HandleFunc("/internal/fleet/v1/upgrade/resolve", m.upgradeResolve)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cookies and public API keys are different authorities and are never
		// accepted on the manager transport. A compromised browser session or
		// leaked inference key therefore cannot become fleet membership. A
		// compromised enrolled member can complete TLS to its controller, but
		// placement handlers separately require the caller to be the adopted
		// controller. Membership alone never grants scheduling authority.
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			writeFleetError(w, http.StatusUnauthorized, errors.New("fleet transport accepts mutual TLS identity only"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (m *Manager) joinPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	var request joinPrepareRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if request.Version != ProtocolVersion || request.FleetID == "" || request.MembershipEpoch <= 0 {
		writeFleetError(w, http.StatusBadRequest, errors.New("fleet join protocol fields are invalid"))
		return
	}
	expectedFingerprint, secret, err := parseJoinCode("v1." + m.identity.CertificateFingerprint + "." + request.JoinSecret)
	if err != nil || expectedFingerprint != m.identity.CertificateFingerprint {
		writeFleetError(w, http.StatusForbidden, errors.New("the fleet join code is invalid, expired, or already used"))
		return
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		writeFleetError(w, http.StatusUnauthorized, errors.New("fleet client certificate is required"))
		return
	}
	controllerCertificate, controllerDetails, err := ParseCertificatePEM(request.ControllerCertificate)
	if err != nil || controllerDetails.NodeID != request.ControllerNodeID || !bytes.Equal(controllerCertificate.Raw, r.TLS.PeerCertificates[0].Raw) {
		writeFleetError(w, http.StatusForbidden, errors.New("the controller identity does not match the mutual TLS client"))
		return
	}
	controllerConsoleURL, err := normalizeOrigin(request.ControllerConsoleURL, false)
	if err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	controllerNodeURL, err := normalizeOrigin(request.ControllerNodeURL, true)
	if err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	prepareToken, err := randomSecret(24)
	if err != nil {
		writeFleetError(w, http.StatusInternalServerError, err)
		return
	}
	expires := m.now().Add(joinPreparationLifetime)
	pending := store.PendingFleetJoin{
		PrepareTokenHash: hashSecret(prepareToken), FleetID: request.FleetID,
		ControllerNodeID: request.ControllerNodeID, ControllerConsoleURL: controllerConsoleURL,
		ControllerNodeURL: controllerNodeURL, ControllerCertificate: request.ControllerCertificate,
		ControllerCertificateFingerprint: controllerDetails.Fingerprint, MembershipEpoch: request.MembershipEpoch,
		ExpiresAt: expires.UTC().Format(time.RFC3339Nano),
	}
	if err := m.database.PrepareMemberJoin(r.Context(), hashSecret(secret), pending, m.now()); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, joinPrepareResponse{
		NodeID: m.identity.NodeID, Certificate: m.identity.CertificatePEM,
		ManagerVersion: m.version, ManagerBuildIdentity: m.buildIdentity,
		CatalogueDigest: m.digest(), PrepareToken: prepareToken,
	})
}

func (m *Manager) joinCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	var request joinTokenRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if request.PrepareToken == "" {
		writeFleetError(w, http.StatusBadRequest, errors.New("membership preparation token is required"))
		return
	}
	if err := m.database.CommitMemberJoin(r.Context(), hashSecret(request.PrepareToken), m.now()); err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, struct{}{})
}

func (m *Manager) joinAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	var request joinTokenRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if err := m.database.AbortMemberJoin(r.Context(), hashSecret(request.PrepareToken)); err != nil {
		writeFleetError(w, http.StatusInternalServerError, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, struct{}{})
}

func (m *Manager) heartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fleetMethodNotAllowed(w)
		return
	}
	config, err := m.database.FleetConfig(r.Context())
	if err != nil {
		writeFleetError(w, http.StatusInternalServerError, err)
		return
	}
	if config.Role != "member" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		writeFleetError(w, http.StatusForbidden, errors.New("only this node's adopted controller can request a heartbeat"))
		return
	}
	_, details, err := inspectRawCertificate(r.TLS.PeerCertificates[0])
	if err != nil || details.NodeID != config.ControllerNodeID {
		writeFleetError(w, http.StatusForbidden, errors.New("the heartbeat caller is not this node's controller"))
		return
	}
	envelope, err := BuildHeartbeat(r.Context(), m.identity, m.database, m.inventory, config.FleetID, m.version, m.buildIdentity, m.digest(), m.now())
	if err != nil {
		writeFleetError(w, http.StatusInternalServerError, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, envelope)
}

func inspectRawCertificate(certificate *x509.Certificate) (*x509.Certificate, certificateDetails, error) {
	details, err := inspectCertificate(certificate)
	return certificate, details, err
}

// The three ways the other end of a fleet call can answer with an identity
// this manager will not talk to. They are sentinels, with the words they
// always had, so that a caller can tell such an answer from silence: the
// machine did answer, and its owner has to hear that rather than be sent to
// look for a broken cable.
var (
	errServerCertificateCount    = errors.New("fleet TLS requires one server certificate")
	errServerCertificatePin      = errors.New("fleet server certificate does not match its pin")
	errServerCertificateValidity = errors.New("fleet server certificate is outside its validity period")
)

func (m *Manager) clientForFingerprint(expectedFingerprint string) *http.Client {
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{m.identity.TLSCertificate()},
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errServerCertificateCount
			}
			certificate := state.PeerCertificates[0]
			details, err := inspectCertificate(certificate)
			if err != nil {
				return err
			}
			if details.Fingerprint != expectedFingerprint {
				return errServerCertificatePin
			}
			now := m.now()
			if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
				return errServerCertificateValidity
			}
			return nil
		},
	}}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	_ = m.PollOnce(ctx)
	m.startUpgradeDriver(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.PollOnce(ctx)
			m.startUpgradeDriver(ctx)
		}
	}
}

func (m *Manager) PollOnce(ctx context.Context) error {
	config, err := m.database.FleetConfig(ctx)
	if err != nil || config.Role != "controller" {
		return err
	}
	envelope, err := BuildHeartbeat(ctx, m.identity, m.database, m.inventory, config.FleetID, m.version, m.buildIdentity, m.digest(), m.now())
	if err != nil {
		return err
	}
	payload, err := envelope.signedBytes()
	if err != nil {
		return err
	}
	receivedAt := m.now().UTC().Format(time.RFC3339Nano)
	if err := m.database.UpdateLocalFleetNode(ctx, m.selfNode(config.FleetID), payload, envelope.Signature, envelope.Payload.Sequence, receivedAt); err != nil {
		return err
	}
	nodes, err := m.database.FleetNodes(ctx)
	if err != nil {
		return err
	}
	var wait sync.WaitGroup
	errorsByNode := make(chan error, len(nodes))
	for _, node := range nodes {
		if node.NodeID == m.identity.NodeID || node.MembershipState != "active" || node.NodeURL == "" || len(node.Certificate) == 0 {
			continue
		}
		wait.Add(1)
		go func(node store.FleetNode) {
			defer wait.Done()
			if err := m.pollNode(ctx, config.FleetID, node); err != nil {
				errorsByNode <- fmt.Errorf("poll %s: %w", node.NodeID, err)
			}
		}(node)
	}
	wait.Wait()
	close(errorsByNode)
	var joined error
	for err := range errorsByNode {
		joined = errors.Join(joined, err)
	}
	return joined
}

func (m *Manager) pollNode(ctx context.Context, fleetID string, node store.FleetNode) error {
	_, details, err := ParseCertificatePEM(node.Certificate)
	if err != nil {
		return err
	}
	if details.NodeID != node.NodeID {
		return errors.New("stored node id does not match its certificate")
	}
	var envelope HeartbeatEnvelope
	if err := callFleetJSON(ctx, m.newClient(details.Fingerprint), http.MethodGet, node.NodeURL+"/internal/fleet/v1/heartbeat", nil, &envelope); err != nil {
		return err
	}
	if err := VerifyHeartbeat(envelope, details.PublicKey, fleetID, node.NodeID); err != nil {
		return err
	}
	payload, err := envelope.signedBytes()
	if err != nil {
		return err
	}
	return m.database.AcceptHeartbeat(ctx, fleetID, node.NodeID, envelope.Payload.Sequence, m.now().UTC().Format(time.RFC3339Nano),
		payload, envelope.Signature, envelope.Payload.ManagerVersion, envelope.Payload.ManagerBuildIdentity, envelope.Payload.CatalogueDigest)
}

func callFleetJSON(ctx context.Context, client *http.Client, method, endpoint string, requestBody, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		// Marked, not reworded: the text still reads as it always did for the
		// logs, and the mark is what lets a console sentence say plainly that
		// a Spark does not answer (see failures.go).
		return nodeUnreachable{err: err}
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxFleetBody+1))
	if err != nil {
		return err
	}
	if len(payload) > maxFleetBody {
		return errors.New("fleet response exceeds the size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		// Same words as before, carried by a type, so that a missing endpoint
		// can be read as the release gap it is (see failures.go).
		return nodeStatus{status: response.StatusCode}
	}
	if responseBody == nil {
		return nil
	}
	return strictJSON(payload, responseBody)
}

func decodeFleetBody(r *http.Request, target any) error {
	defer r.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxFleetBody+1))
	if err != nil {
		return err
	}
	if len(payload) > maxFleetBody {
		return errors.New("fleet request exceeds the size limit")
	}
	if err := strictJSON(payload, target); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	return nil
}

func writeFleetJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeFleetError(w http.ResponseWriter, status int, err error) {
	writeFleetJSON(w, status, map[string]string{"error": err.Error()})
}

func fleetMethodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, POST")
	writeFleetError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
}
