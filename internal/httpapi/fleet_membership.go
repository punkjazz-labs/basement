package httpapi

import (
	"errors"
	"net/http"
	"path"
	"strings"

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

// Adding a Spark to the fleet is two clicks (ADR 0019). These four routes are
// the console halves of it: the two invitation routes answer for this Spark,
// and the two invite routes drive an addition from this Spark. All four are
// owner-session routes, and no join code crosses any of them: approval is an
// owner action here, and the code it mints travels machine to machine on the
// fleet transport.

// fleetInvitations lists the controllers asking to adopt this Spark.
func (s *Server) fleetInvitations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet membership is not initialized"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": s.fleetManager.PendingInvitations()})
}

// fleetInvitationAction answers one invitation. Approving is what mints a join
// code for the Spark that asked, so it is a mutation with the owner's session
// and CSRF behind it, exactly like every other membership change.
func (s *Server) fleetInvitationAction(w http.ResponseWriter, r *http.Request) {
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
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/fleet/invitations/")
	id, action, found := strings.Cut(trimmed, "/")
	if !found || id == "" {
		http.NotFound(w, r)
		return
	}
	var err error
	switch action {
	case "approve":
		err = s.fleetManager.ApproveInvitation(r.Context(), id)
	case "deny":
		err = s.fleetManager.DenyInvitation(id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "state": action + "d"})
}

// fleetInvite asks another Spark to join this fleet. The owner picks a console
// this manager already knows about; the fleet listener next to it is derived,
// never typed.
func (s *Server) fleetInvite(w http.ResponseWriter, r *http.Request) {
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
	var request struct {
		ConsoleURL  string `json:"console_url"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	nodeURL, err := adjacentFleetNodeURL(strings.TrimSpace(request.ConsoleURL))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	progress, err := s.fleetManager.Invite(r.Context(), fleet.InviteRequest{
		ConsoleURL: request.ConsoleURL, NodeURL: nodeURL, DisplayName: request.DisplayName,
	})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, progress)
}

// fleetInviteStatus advances and reports one addition. It is a read for the
// console, and the step it takes on this Spark's behalf is one the owner
// already authorized by sending the invitation.
func (s *Server) fleetInviteStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.fleetManager == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("fleet membership is not initialized"))
		return
	}
	consoleURL := strings.TrimSpace(r.URL.Query().Get("console_url"))
	if consoleURL == "" {
		writeError(w, http.StatusBadRequest, errors.New("name the console this Spark is adding"))
		return
	}
	progress, err := s.fleetManager.InviteStatus(r.Context(), consoleURL)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, progress)
}
