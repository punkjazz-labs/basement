package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	managerupdate "github.com/punkjazz-labs/basement/internal/update"
)

func (s *Server) updateAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.updateCheck(w, r)
		return
	}
	methodNotAllowed(w)
}

func (s *Server) updateApplyAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.applyUpdate(w, r)
		return
	}
	methodNotAllowed(w)
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if s.updateResult != nil && time.Since(s.updateFetched) < time.Hour {
		writeJSON(w, http.StatusOK, s.updateResult)
		return
	}
	result := map[string]any{
		"current_version": s.version, "checked": true, "update_available": false,
		"signed": false, "compatible": false, "installable": false,
	}
	if _, err := managerupdate.ParseVersion(s.version); err != nil {
		result["note"] = "development builds cannot use console updates"
		s.finishUpdateCheck(result, nil)
		writeJSON(w, http.StatusOK, result)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	resolution, err := s.updateResolver.Resolve(ctx, s.version)
	if err != nil {
		result["checked"] = false
		result["note"] = cleanUpdateError(err)
		s.finishUpdateCheck(result, nil)
		writeJSON(w, http.StatusOK, result)
		return
	}
	if resolution.NewestPublished != "" {
		result["latest_version"] = strings.TrimPrefix(resolution.NewestPublished, "v")
		result["release_url"] = resolution.NewestReleaseURL
		result["update_available"] = managerupdate.IsNewer(s.version, resolution.NewestPublished)
	}
	if resolution.Candidate != nil {
		candidate := cloneUpdateCandidate(*resolution.Candidate)
		result["latest_version"] = strings.TrimPrefix(candidate.Manifest.ReleaseVersion, "v")
		result["target_version"] = candidate.Manifest.ReleaseVersion
		result["release_url"] = candidate.Release.HTMLURL
		result["update_available"] = true
		result["signed"] = true
		result["compatible"] = true
		if err := s.updateStager.BootstrapReady(); err != nil {
			result["manual_bootstrap_required"] = true
			result["reason"] = err.Error()
		} else {
			result["installable"] = true
		}
		s.finishUpdateCheck(result, &candidate)
		writeJSON(w, http.StatusOK, result)
		return
	}
	if resolution.ManualUpgrade {
		result["manual_upgrade_required"] = true
		result["reason"] = "this machine needs a one-time manual upgrade before console updates can continue"
	} else if resolution.NewestPublished == "" {
		result["note"] = "no published releases yet"
	} else if managerupdate.IsNewer(s.version, resolution.NewestPublished) {
		result["reason"] = "the newer release could not be verified for this machine"
	}
	s.finishUpdateCheck(result, nil)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) finishUpdateCheck(result map[string]any, candidate *managerupdate.Candidate) {
	result["checked_at"] = time.Now().UTC().Format(time.RFC3339)
	s.updateResult = result
	s.updateCandidate = candidate
	s.updateFetched = time.Now()
}

func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if r.ContentLength != 0 {
		var request struct{}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	s.updateAdmissionMu.Lock()
	defer s.updateAdmissionMu.Unlock()
	if s.updateMaintenanceActiveLocked() {
		writeError(w, http.StatusConflict, errors.New("a manager update is already in progress"))
		return
	}
	blocker, blocked, err := s.store.ActiveUpdateBlocker(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if blocked {
		writeError(w, http.StatusConflict, errors.New("cannot update while "+blocker.Activity+" is "+blocker.State+"; finish or cancel it first"))
		return
	}
	s.updateMu.Lock()
	var candidate *managerupdate.Candidate
	if s.updateCandidate != nil {
		copy := cloneUpdateCandidate(*s.updateCandidate)
		candidate = &copy
	}
	s.updateMu.Unlock()
	if candidate == nil {
		writeError(w, http.StatusConflict, errors.New("check for updates again before starting the update"))
		return
	}
	if err := s.updateStager.BootstrapReady(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	status, err := s.updateStager.Prepare(*candidate, s.version)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.updateApplying = true
	go func(candidate managerupdate.Candidate) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_, err := s.updateStager.StagePrepared(ctx, candidate, status)
		if err != nil {
			s.updateAdmissionMu.Lock()
			s.updateApplying = false
			s.updateAdmissionMu.Unlock()
		}
	}(*candidate)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true, "attempt_id": status.AttemptID, "state": status.State,
	})
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	status, found, err := s.updateStager.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) updateMaintenanceActiveLocked() bool {
	if s.updateApplying {
		return true
	}
	status, found, err := s.updateStager.Status()
	if err != nil || !found {
		return false
	}
	switch status.State {
	case "succeeded", "rolled_back", "recovery_required", "failed_before_handoff":
		return false
	default:
		return true
	}
}

func (s *Server) refuseMutationDuringUpdate(w http.ResponseWriter) bool {
	if !s.updateMaintenanceActiveLocked() {
		return false
	}
	writeError(w, http.StatusConflict, errors.New("a manager update is in progress; wait for basement to reconnect before starting new work"))
	return true
}

func cloneUpdateCandidate(candidate managerupdate.Candidate) managerupdate.Candidate {
	candidate.ManifestBytes = append([]byte(nil), candidate.ManifestBytes...)
	candidate.Signature = append([]byte(nil), candidate.Signature...)
	candidate.Manifest.RollbackFrom = append([]string(nil), candidate.Manifest.RollbackFrom...)
	candidate.Release.Assets = append([]managerupdate.ReleaseAsset(nil), candidate.Release.Assets...)
	return candidate
}

func cleanUpdateError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}
