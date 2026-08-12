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

func TestDocredactAddManualFindingRequiresSessionAndCSRF(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")
	url := server.URL + "/api/v1/docredact/sessions/" + result.SessionID + "/findings"
	body := `{"literal":"the contract"}`

	anonymous := doRequest(t, http.MethodPost, url, body, nil, map[string]string{"Origin": server.URL})
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated add status=%d", anonymous.StatusCode)
	}
	noCSRF := doRequest(t, http.MethodPost, url, body, cookies, map[string]string{"Origin": server.URL})
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", noCSRF.StatusCode)
	}
}

// docredactAddManual posts one hand-picked literal and returns the answer.
func docredactAddManual(t *testing.T, server, sessionID, csrf string, cookies []*http.Cookie, body string) *http.Response {
	t.Helper()
	return doRequest(t, http.MethodPost, server+"/api/v1/docredact/sessions/"+sessionID+"/findings",
		body, cookies, map[string]string{"Origin": server, "X-CSRF-Token": csrf})
}

func TestDocredactAddManualFinding(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")

	response := docredactAddManual(t, server.URL, result.SessionID, csrf, cookies, `{"literal":"Jane"}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("add status=%d body=%s", response.StatusCode, data)
	}
	var added struct {
		Finding  *docredact.Finding   `json:"finding"`
		Findings []*docredact.Finding `json:"findings"`
	}
	if err := json.NewDecoder(response.Body).Decode(&added); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if added.Finding == nil || added.Finding.Literal != "Jane" {
		t.Fatalf("finding = %+v, want the literal that was sent", added.Finding)
	}
	if added.Finding.Source != docredact.SourceManual {
		t.Errorf("source = %q, want manual", added.Finding.Source)
	}
	// No category was named, so it is a phrase.
	if added.Finding.Category != docredact.CategoryPhrase || added.Finding.Token != "[PHRASE_1]" {
		t.Errorf("finding = %+v, want a phrase carrying [PHRASE_1]", added.Finding)
	}
	if added.Finding.Occurrences != 1 || !added.Finding.Enabled {
		t.Errorf("finding = %+v, want one occurrence, enabled", added.Finding)
	}
	if len(added.Findings) != 2 {
		t.Fatalf("findings = %+v, want the email and the phrase", added.Findings)
	}

	preview := docredactPreview(t, server.URL, result.SessionID, cookies)
	if strings.Contains(preview, "Jane") || !strings.Contains(preview, "[PHRASE_1]") {
		t.Fatalf("preview = %q, want Jane replaced by its pseudonym", preview)
	}
}

func TestDocredactAddManualFindingUsesAKnownCategory(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")

	response := docredactAddManual(t, server.URL, result.SessionID, csrf, cookies,
		`{"literal":"the contract","category":"phone"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("add with category status=%d", response.StatusCode)
	}
	var added struct {
		Finding *docredact.Finding `json:"finding"`
	}
	json.NewDecoder(response.Body).Decode(&added)
	response.Body.Close()
	if added.Finding.Category != docredact.CategoryPhone || added.Finding.Token != "[PHONE_1]" {
		t.Fatalf("finding = %+v, want the named category and its prefix", added.Finding)
	}

	// A category this build does not know is not an error: the text is a
	// phrase until something says otherwise.
	unknown := docredactAddManual(t, server.URL, result.SessionID, csrf, cookies,
		`{"literal":"tomorrow","category":"role"}`)
	if unknown.StatusCode != http.StatusOK {
		t.Fatalf("add with unknown category status=%d", unknown.StatusCode)
	}
	json.NewDecoder(unknown.Body).Decode(&added)
	unknown.Body.Close()
	if added.Finding.Category != docredact.CategoryPhrase {
		t.Fatalf("category = %q, want phrase", added.Finding.Category)
	}
}

func TestDocredactAddManualFindingRefusals(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")

	for _, testCase := range []struct {
		name string
		body string
		want int
	}{
		{"empty literal", `{"literal":"  "}`, http.StatusBadRequest},
		{"absent from the document", `{"literal":"Zurich"}`, http.StatusBadRequest},
		{"already a finding", `{"literal":"jane.doe@example.com"}`, http.StatusConflict},
		{"inside a longer finding", `{"literal":"example.com"}`, http.StatusConflict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := docredactAddManual(t, server.URL, result.SessionID, csrf, cookies, testCase.body)
			if response.StatusCode != testCase.want {
				data, _ := io.ReadAll(response.Body)
				t.Fatalf("status=%d want=%d body=%s", response.StatusCode, testCase.want, data)
			}
			response.Body.Close()
		})
	}

	// Every refusal left the document exactly as analyze built it.
	listed := doRequest(t, http.MethodGet,
		server.URL+"/api/v1/docredact/sessions/"+result.SessionID+"/findings", "", cookies, nil)
	var body struct {
		Findings []*docredact.Finding `json:"findings"`
	}
	json.NewDecoder(listed.Body).Decode(&body)
	listed.Body.Close()
	if len(body.Findings) != 1 || body.Findings[0].Literal != "jane.doe@example.com" {
		t.Fatalf("findings = %+v, want the one detected email", body.Findings)
	}
}

// A hand-picked phrase that contains a detected literal wins over it, and the
// export follows: one pseudonym for the whole phrase, one mapping row.
func TestDocredactAddManualFindingCoversADetectedLiteral(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	result := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")

	response := docredactAddManual(t, server.URL, result.SessionID, csrf, cookies,
		`{"literal":"Jane at jane.doe@example.com"}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("add status=%d body=%s", response.StatusCode, data)
	}
	var added struct {
		Findings []*docredact.Finding `json:"findings"`
	}
	json.NewDecoder(response.Body).Decode(&added)
	response.Body.Close()

	// The email still appears once on its own further down the sample, so it
	// keeps a finding; the longer phrase takes the occurrence they share.
	if len(added.Findings) != 2 {
		t.Fatalf("findings = %+v, want the phrase and the remaining email", added.Findings)
	}
	byLiteral := map[string]*docredact.Finding{}
	for _, finding := range added.Findings {
		byLiteral[finding.Literal] = finding
	}
	phrase := byLiteral["Jane at jane.doe@example.com"]
	email := byLiteral["jane.doe@example.com"]
	if phrase == nil || email == nil {
		t.Fatalf("findings = %+v, want both literals present", added.Findings)
	}
	if phrase.Occurrences != 1 {
		t.Errorf("phrase occurrences = %d, want 1", phrase.Occurrences)
	}
	if email.Occurrences != 1 {
		t.Errorf("email occurrences = %d, want 1 once the phrase took the other", email.Occurrences)
	}

	preview := docredactPreview(t, server.URL, result.SessionID, cookies)
	if strings.Contains(preview, "jane.doe@example.com") {
		t.Fatalf("preview = %q, want no literal left", preview)
	}

	mappingResponse := doRequest(t, http.MethodGet,
		server.URL+"/api/v1/docredact/sessions/"+result.SessionID+"/export/mapping", "", cookies, nil)
	mappingBody, _ := io.ReadAll(mappingResponse.Body)
	mappingResponse.Body.Close()
	_, entries, err := docredact.ParseMapping(mappingBody)
	if err != nil {
		t.Fatalf("ParseMapping: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("mapping entries = %+v, want one per finding", entries)
	}
	for _, entry := range entries {
		if entry.Literal == "Jane at jane.doe@example.com" && entry.Source != docredact.SourceManual {
			t.Errorf("phrase entry = %+v, want it to say manual", entry)
		}
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
