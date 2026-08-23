package fleet

import (
	"errors"
	"net/http"
	"path"
	"strings"
)

func (m *Manager) requireControllerCaller(r *http.Request) error {
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		return errors.New("the fleet controller certificate is required")
	}
	_, caller, err := inspectRawCertificate(r.TLS.PeerCertificates[0])
	if err != nil {
		return err
	}
	config, err := m.database.FleetConfig(r.Context())
	if err != nil {
		return err
	}
	if config.Role != "member" || caller.NodeID != config.ControllerNodeID {
		return errors.New("only this node's adopted controller can issue a placement")
	}
	return nil
}

func (m *Manager) reservationPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.requireControllerCaller(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request reservationPrepareRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	response, err := m.prepareIndependent(r.Context(), request)
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, response)
}

func (m *Manager) reservationCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.requireControllerCaller(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request reservationCommitRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if err := m.commitIndependent(r.Context(), request.PrepareToken, request.Grant); err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, struct{}{})
}

func (m *Manager) reservationAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.requireControllerCaller(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request reservationIDRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if err := m.allocator.Abort(r.Context(), request.ReservationID); err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, struct{}{})
}

func (m *Manager) independentDeployment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.requireControllerCaller(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request independentDeploymentRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	job, created, err := m.startIndependent(r.Context(), request.Grant, request.Intent)
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusAccepted, independentDeploymentResponse{Job: job, Created: created})
}

// independentDeploymentAdopt records an already-installed model as a fleet
// deployment. Only this node's adopted controller may ask for it, exactly as
// with every other placement handler here. The reply is 200, not 202: no work
// starts, because the carrier job is terminal the moment it exists.
func (m *Manager) independentDeploymentAdopt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.requireControllerCaller(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request adoptDeploymentRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	job, created, err := m.adoptIndependent(r.Context(), request)
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, adoptDeploymentResponse{Job: job, Created: created})
}

func (m *Manager) independentJob(w http.ResponseWriter, r *http.Request) {
	if err := m.requireControllerCaller(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/internal/fleet/v1/jobs/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" || parts[0] == "." {
		http.NotFound(w, r)
		return
	}
	runtime := m.independentRuntime()
	if runtime == nil {
		writeFleetError(w, http.StatusServiceUnavailable, errors.New("the target manager is not ready to report placement jobs"))
		return
	}
	owner, err := runtime.IndependentJob(r.Context(), parts[0])
	if err != nil {
		writeFleetError(w, http.StatusNotFound, errors.New("the deployment job was not found on its owner node"))
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		writeFleetJSON(w, http.StatusOK, owner)
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	var request remoteJobActionRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	if request.Action != parts[1] {
		writeFleetError(w, http.StatusBadRequest, errors.New("the deployment action path and body do not match"))
		return
	}
	job, err := runtime.IndependentAction(r.Context(), owner, request.Action, request.IdempotencyKey, request.Intent)
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusAccepted, job)
}
