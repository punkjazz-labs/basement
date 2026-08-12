package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/docredact"
)

const docredactSampleText = "Contact Jane at jane.doe@example.com about the contract, and again at jane.doe@example.com tomorrow."

func TestDocredactAnalyzeRequiresSession(t *testing.T) {
	server, _, _ := newPairedTestServer(t)
	response := doRequest(t, http.MethodPost, server.URL+"/api/v1/docredact/analyze",
		`{"text":"hello"}`, nil, map[string]string{"Origin": server.URL})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated analyze status=%d", response.StatusCode)
	}
}

func TestDocredactAnalyzeRequiresCSRF(t *testing.T) {
	server, cookies, _ := newPairedTestServer(t)
	// Cookies but no X-CSRF-Token header: the mutation must still be
	// refused, exactly like every other console mutation.
	response := doRequest(t, http.MethodPost, server.URL+"/api/v1/docredact/analyze",
		`{"text":"hello"}`, cookies, map[string]string{"Origin": server.URL})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", response.StatusCode)
	}
}

func TestDocredactAnalyzeRejectsEmptyText(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	headers := map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf}
	response := doRequest(t, http.MethodPost, server.URL+"/api/v1/docredact/analyze",
		`{"text":"   "}`, cookies, headers)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty text status=%d", response.StatusCode)
	}
}

// docredactAnalyzeResponse mirrors the JSON shape docredactAnalyze writes.
type docredactAnalyzeResponse struct {
	SessionID string               `json:"session_id"`
	Findings  []*docredact.Finding `json:"findings"`
}

func docredactAnalyze(t *testing.T, server, csrf string, cookies []*http.Cookie, text, name string) docredactAnalyzeResponse {
	t.Helper()
	body, err := json.Marshal(map[string]string{"text": text, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	response := doRequest(t, http.MethodPost, server+"/api/v1/docredact/analyze",
		string(body), cookies, map[string]string{"Origin": server, "X-CSRF-Token": csrf})
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("analyze status=%d body=%s", response.StatusCode, data)
	}
	var result docredactAnalyzeResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return result
}

func TestDocredactAnalyzeGroupsFindingsByLiteral(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")

	if result.SessionID == "" {
		t.Fatal("expected a session id")
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one grouped finding", result.Findings)
	}
	finding := result.Findings[0]
	if finding.Literal != "jane.doe@example.com" {
		t.Errorf("literal = %q, want jane.doe@example.com", finding.Literal)
	}
	if finding.Occurrences != 2 {
		t.Errorf("occurrences = %d, want 2", finding.Occurrences)
	}
	if finding.Category != docredact.CategoryEmail {
		t.Errorf("category = %s, want email", finding.Category)
	}
	if !finding.Enabled {
		t.Error("expected the finding to be enabled by default")
	}

	// GET findings must report the same thing back.
	listResponse := doRequest(t, http.MethodGet, server.URL+"/api/v1/docredact/sessions/"+result.SessionID+"/findings",
		"", cookies, nil)
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list findings status=%d", listResponse.StatusCode)
	}
	var listed struct {
		Findings []*docredact.Finding `json:"findings"`
	}
	json.NewDecoder(listResponse.Body).Decode(&listed)
	listResponse.Body.Close()
	if len(listed.Findings) != 1 || listed.Findings[0].ID != finding.ID {
		t.Fatalf("listed findings = %+v, want to match analyze's response", listed.Findings)
	}
}

func TestDocredactUnknownSession(t *testing.T) {
	server, cookies, _ := newPairedTestServer(t)
	response := doRequest(t, http.MethodGet, server.URL+"/api/v1/docredact/sessions/does-not-exist/findings",
		"", cookies, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session status=%d", response.StatusCode)
	}
}

func TestDocredactToggleAndPreview(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	headers := map[string]string{"Origin": server.URL, "X-CSRF-Token": csrf}
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")
	findingID := result.Findings[0].ID

	// Default preview must not contain the literal.
	preview := docredactPreview(t, server.URL, result.SessionID, cookies)
	if strings.Contains(preview, "jane.doe@example.com") {
		t.Fatal("preview contains the literal before any toggle")
	}
	if !strings.Contains(preview, "[EMAIL_1]") {
		t.Fatalf("preview = %q, want it to contain the [EMAIL_1] token", preview)
	}

	// Toggling it off must restore the literal in the preview.
	toggleBody := `{"enabled":false}`
	toggleURL := server.URL + "/api/v1/docredact/sessions/" + result.SessionID + "/findings/" + findingID + "/toggle"
	toggleResponse := doRequest(t, http.MethodPost, toggleURL, toggleBody, cookies, headers)
	if toggleResponse.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(toggleResponse.Body)
		t.Fatalf("toggle status=%d body=%s", toggleResponse.StatusCode, data)
	}
	toggleResponse.Body.Close()

	preview = docredactPreview(t, server.URL, result.SessionID, cookies)
	if !strings.Contains(preview, "jane.doe@example.com") {
		t.Fatal("preview should contain the literal once the finding is disabled")
	}

	// Toggling an unknown finding must 404.
	badToggle := doRequest(t, http.MethodPost,
		server.URL+"/api/v1/docredact/sessions/"+result.SessionID+"/findings/NOPE_1/toggle",
		toggleBody, cookies, headers)
	if badToggle.StatusCode != http.StatusNotFound {
		t.Fatalf("toggle of unknown finding status=%d", badToggle.StatusCode)
	}
}

func docredactPreview(t *testing.T, server, sessionID string, cookies []*http.Cookie) string {
	t.Helper()
	response := doRequest(t, http.MethodGet, server+"/api/v1/docredact/sessions/"+sessionID+"/preview", "", cookies, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("preview status=%d", response.StatusCode)
	}
	var body struct {
		Text string `json:"text"`
	}
	json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	return body.Text
}

func TestDocredactExportDownloads(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")

	redactedResponse := doRequest(t, http.MethodGet,
		server.URL+"/api/v1/docredact/sessions/"+result.SessionID+"/export/redacted", "", cookies, nil)
	if redactedResponse.StatusCode != http.StatusOK {
		t.Fatalf("export redacted status=%d", redactedResponse.StatusCode)
	}
	if disposition := redactedResponse.Header.Get("Content-Disposition"); !strings.Contains(disposition, `filename="letter.redacted.txt"`) {
		t.Errorf("Content-Disposition = %q, want it to name letter.redacted.txt", disposition)
	}
	redactedBody, _ := io.ReadAll(redactedResponse.Body)
	redactedResponse.Body.Close()
	if strings.Contains(string(redactedBody), "jane.doe@example.com") {
		t.Error("downloaded redacted file still contains the literal")
	}

	mappingResponse := doRequest(t, http.MethodGet,
		server.URL+"/api/v1/docredact/sessions/"+result.SessionID+"/export/mapping", "", cookies, nil)
	if mappingResponse.StatusCode != http.StatusOK {
		t.Fatalf("export mapping status=%d", mappingResponse.StatusCode)
	}
	if disposition := mappingResponse.Header.Get("Content-Disposition"); !strings.Contains(disposition, `filename="letter.mapping.json"`) {
		t.Errorf("Content-Disposition = %q, want it to name letter.mapping.json", disposition)
	}
	mappingBody, _ := io.ReadAll(mappingResponse.Body)
	mappingResponse.Body.Close()

	warning, entries, err := docredact.ParseMapping(mappingBody)
	if err != nil {
		t.Fatalf("ParseMapping: %v", err)
	}
	if warning != docredact.MappingWarning {
		t.Errorf("warning = %q, want %q", warning, docredact.MappingWarning)
	}
	if len(entries) != 1 || entries[0].Literal != "jane.doe@example.com" || entries[0].Occurrences != 2 {
		t.Fatalf("mapping entries = %+v, want the one email entry with 2 occurrences", entries)
	}

	// The two downloads must never share bytes: the redacted copy must
	// never contain the mapping's warning line or any of its literals.
	if strings.Contains(string(redactedBody), docredact.MappingWarning) {
		t.Error("redacted download contains the mapping warning line")
	}
}
