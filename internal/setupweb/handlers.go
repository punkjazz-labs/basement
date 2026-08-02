package setupweb

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// authorize is the line-by-line security gate every route runs through
// first:
//   - Host must be exactly the loopback address this server bound (rejects
//     DNS-rebinding: an attacker-controlled name that resolves to
//     127.0.0.1 would send a different Host header than the literal
//     address we told the browser to open).
//   - The URL token must match, constant-time, or the route does not exist
//     as far as the caller can tell.
//   - An Origin header, when present, must be this server's own origin.
//     Direct browser navigation sends no Origin; a foreign Origin means
//     some other page's script is probing localhost.
//
// Nothing here ever inspects or logs the request body, which is the only
// place a password can appear.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if r.Host != s.addr {
		http.NotFound(w, r)
		return false
	}
	supplied := r.PathValue("token")
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(s.token)) != 1 {
		http.NotFound(w, r)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+s.addr {
		http.Error(w, "cross-origin request denied", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	data, err := pageAsset.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	s.mu.Lock()
	payload, err := json.Marshal(s.state)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(payload)
}

// handleAnswer accepts the single question the flow is currently blocked
// on. The body is a JSON object tagged with the "kind" it answers
// (matched against the pending question, never trusted otherwise) plus
// whatever fields that kind needs — including, for the password and
// confirm kinds, the SSH/sudo secret itself. That secret exists only in
// this POST body: it is parsed straight into the waiting flow goroutine
// and is never written to a URL, a log, or a response.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Kind == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	pending := s.pending
	if pending == nil || pending.kind != envelope.Kind {
		s.mu.Unlock()
		http.Error(w, "no matching question is pending", http.StatusConflict)
		return
	}
	s.pending = nil
	s.mu.Unlock()

	pending.answer <- json.RawMessage(body)
	w.WriteHeader(http.StatusNoContent)
}

// logged records method and a token-redacted path for every request — never
// headers, never bodies, so a password can never reach it.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logger.Printf("%s %s", r.Method, redactPath(r.URL.Path))
		next.ServeHTTP(w, r)
	})
}

func redactPath(path string) string {
	const prefix = "/setup/"
	if !strings.HasPrefix(path, prefix) {
		return path
	}
	rest := path[len(prefix):]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return prefix + "<token>" + rest[slash:]
	}
	return prefix + "<token>"
}
