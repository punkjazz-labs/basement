package httpapi

import (
	"errors"
	"net/http"

	"github.com/punkjazz-labs/basement/internal/fleet"
)

func (s *Server) fleetMembershipSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet membership is not initialized"))
		return
	}
	summary, err := s.fleetManager.Summary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) fleetJoinCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet membership is not initialized"))
		return
	}
	code, err := s.fleetManager.CreateJoinCode(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, code)
}

func (s *Server) fleetJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet membership is not initialized"))
		return
	}
	var request fleet.AdoptRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.fleetManager.Adopt(r.Context(), request)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
