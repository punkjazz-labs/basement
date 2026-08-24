package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/punkjazz-labs/basement/internal/store"
)

// The GPU power mode, from the console (task #22). There are two doors and
// they are deliberately different: /api/v1/system/power-mode is this Spark
// speaking about itself, and /api/v1/fleet/power-mode is a controller speaking
// about any Spark in its fleet, this one included. The console uses the second
// wherever a fleet exists, so one dashboard keeps working the way it already
// does for models.
//
// Neither door touches recipes, deployments or serving. Changing the mode
// while a model streams is allowed, because the cap applies to that stream
// live and that is the whole point of the setting.

// powerModeNodeResponse is the fleet answer: the same four fields the local
// answer has, with the Spark they belong to named first.
type powerModeNodeResponse struct {
	NodeID string `json:"node_id"`
	store.PowerMode
}

// requirePowerMutation is the gate both doors pass. It is the ordinary console
// mutation gate plus the managed-member door, in that order, so a member gets
// exactly the sentence it gives for a model change: one console owns this
// machine, and on a member that console is the controller's.
func (s *Server) requirePowerMutation(w http.ResponseWriter, r *http.Request) bool {
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return false
	}
	if s.refuseManagedMemberMutation(w, r) {
		return false
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, errors.New("a valid Idempotency-Key header is required"))
		return false
	}
	return true
}

// powerMode reads and sets the mode of the Spark the console is open on.
func (s *Server) powerMode(w http.ResponseWriter, r *http.Request) {
	if s.power == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this manager cannot read or change the GPU power mode"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		state, err := s.power.PowerMode(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	case http.MethodPost:
		if !s.requirePowerMutation(w, r) {
			return
		}
		var request struct {
			Mode string `json:"mode"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		state, err := s.power.SetPowerMode(r.Context(), request.Mode)
		if errors.Is(err, store.ErrPowerMode) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// 200 and the whole new state, never 202: the setting is written and
		// the GPU has already been asked by the time this answers. A machine
		// that refused the cap says so in the failure sentence rather than by
		// leaving the console to poll for an outcome that never changes.
		writeJSON(w, http.StatusOK, state)
	default:
		methodNotAllowed(w)
	}
}

// fleetPowerMode sets the mode of any Spark in this fleet. The controller
// answers for its own node from its own runtime and for every other node over
// the mutual TLS fleet plane, so the console makes one call whichever Spark
// the owner picked.
//
// There is no GET here on purpose. Every node reports its mode in the
// heartbeat this controller already collects, so the fleet summary the
// dashboard reads already carries it.
func (s *Server) fleetPowerMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet control is unavailable"))
		return
	}
	if !s.requirePowerMutation(w, r) {
		return
	}
	var request struct {
		NodeID string `json:"node_id"`
		Mode   string `json:"mode"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.fleetManager.SetNodePowerMode(r.Context(), request.NodeID, request.Mode)
	if errors.Is(err, store.ErrPowerMode) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, powerModeNodeResponse{NodeID: request.NodeID, PowerMode: state})
}
