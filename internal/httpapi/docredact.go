package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/punkjazz-labs/basement/internal/docredact"
)

// docredactSession is one analyzed document, kept in memory only. The
// document itself, and the file it may have come from, never touch this
// manager's own disk: the browser read the file (or the owner pasted
// text), and export writes straight back into an HTTP response as a
// download rather than anywhere on this machine (spec's "runs from
// basement directly" design).
type docredactSession struct {
	mu   sync.Mutex
	doc  *docredact.Document
	name string // display name for suggested download filenames, without extension
	ext  string // ".txt" or ".md"
}

// docredactSessions is the manager-wide set of active redaction sessions.
type docredactSessions struct {
	mu   sync.Mutex
	byID map[string]*docredactSession
}

func newDocredactSessions() *docredactSessions {
	return &docredactSessions{byID: make(map[string]*docredactSession)}
}

func (s *docredactSessions) put(session *docredactSession) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	id := hex.EncodeToString(raw)
	s.mu.Lock()
	s.byID[id] = session
	s.mu.Unlock()
	return id, nil
}

func (s *docredactSessions) get(id string) (*docredactSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byID[id]
	return session, ok
}

// docredactFilenameParts derives the display name and extension used for
// suggested download filenames from the client-supplied name, which is
// cosmetic only (nothing here reads a file by this name -- the browser
// already sent the text). Anything that is not recognizably .txt or .md
// falls back to .txt, matching the spec's v1 scope: plain text and
// markdown only.
func docredactFilenameParts(requested string) (name, ext string) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "document", ".txt"
	}
	ext = strings.ToLower(filepath.Ext(requested))
	if ext != ".md" {
		ext = ".txt"
	}
	name = strings.TrimSuffix(filepath.Base(requested), filepath.Ext(requested))
	if name == "" || name == "." {
		name = "document"
	}
	return name, ext
}

func mimeForDocredactExt(ext string) string {
	if ext == ".md" {
		return "text/markdown; charset=utf-8"
	}
	return "text/plain; charset=utf-8"
}

// docredactAnalyze runs the pattern pass over submitted text and starts a
// session. There is no model pass yet (spec step 3) and no path field: the
// manager runs on the Spark, not on the owner's laptop where the document
// lives, so the browser sends the text itself rather than a local path for
// this process to open.
func (s *Server) docredactAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var request struct {
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeError(w, http.StatusBadRequest, errors.New("text is required"))
		return
	}

	name, ext := docredactFilenameParts(request.Name)
	doc := docredact.Analyze(request.Text)
	session := &docredactSession{doc: doc, name: name, ext: ext}
	id, err := s.docredact.put(session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "findings": doc.Findings})
}

// docredactSessionAction dispatches every /api/v1/docredact/sessions/...
// route, following the same manual path-parsing shape as jobAction and
// modelAction elsewhere in this package.
func (s *Server) docredactSessionAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/docredact/sessions/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" || parts[0] == "." {
		http.NotFound(w, r)
		return
	}
	session, ok := s.docredact.get(parts[0])
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no such redaction session"))
		return
	}

	switch {
	case len(parts) == 2 && parts[1] == "findings" && r.Method == http.MethodGet:
		session.mu.Lock()
		defer session.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"findings": session.doc.Findings})

	// Adding a finding by hand: the owner selected text in the preview that
	// no detector claimed. The category is optional and unknown names are
	// not an error -- selected text is a phrase until the caller names a
	// category this build knows.
	case len(parts) == 2 && parts[1] == "findings" && r.Method == http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		var request struct {
			Literal  string `json:"literal"`
			Category string `json:"category"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		category, _ := docredact.ParseCategory(request.Category)
		session.mu.Lock()
		defer session.mu.Unlock()
		finding, err := session.doc.AddManual(request.Literal, category)
		if err != nil {
			writeError(w, docredactAddStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"finding": finding, "findings": session.doc.Findings})

	case len(parts) == 4 && parts[1] == "findings" && parts[3] == "toggle" && r.Method == http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		var request struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		session.mu.Lock()
		defer session.mu.Unlock()
		if !session.doc.Toggle(parts[2], request.Enabled) {
			writeError(w, http.StatusNotFound, errors.New("no such finding"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"findings": session.doc.Findings})

	case len(parts) == 2 && parts[1] == "preview" && r.Method == http.MethodGet:
		session.mu.Lock()
		defer session.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"text": session.doc.Redacted()})

	case len(parts) == 3 && parts[1] == "export" && parts[2] == "redacted" && r.Method == http.MethodGet:
		session.mu.Lock()
		defer session.mu.Unlock()
		filename := session.name + ".redacted" + session.ext
		writeDocredactDownload(w, filename, mimeForDocredactExt(session.ext), []byte(session.doc.Redacted()))

	case len(parts) == 3 && parts[1] == "export" && parts[2] == "mapping" && r.Method == http.MethodGet:
		session.mu.Lock()
		defer session.mu.Unlock()
		data, err := session.doc.MappingBytes()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// text/plain, not application/json: the mapping's first line is a
		// plain warning sentence, not JSON, by design (see
		// docredact.MappingWarning), so calling it JSON would be a false
		// promise to anything that tried to parse it as such.
		writeDocredactDownload(w, session.name+".mapping.json", "text/plain; charset=utf-8", data)

	default:
		methodNotAllowed(w)
	}
}

// docredactAddStatus separates a request that was wrong from one the
// document simply cannot honour. A literal that is already hidden, under its
// own name or inside a longer one, is a conflict with the document's current
// state rather than a malformed request.
func docredactAddStatus(err error) int {
	switch {
	case errors.Is(err, docredact.ErrLiteralKnown), errors.Is(err, docredact.ErrLiteralCovered):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// writeDocredactDownload writes body as a browser download. Nothing here
// touches this machine's own filesystem -- the response body is the whole
// export, exactly as spec'd for a manager that runs the redactor itself
// rather than shipping it as a separate laptop-side binary.
func writeDocredactDownload(w http.ResponseWriter, filename, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
