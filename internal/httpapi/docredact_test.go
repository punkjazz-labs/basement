package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/docredact"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
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

// docredactFakeModel stands in for the serving runtime on its loopback port,
// answering chat/completions with scripted content. The last scripted reply
// is repeated once the script runs out, so a chunk's repair retry is
// answered too without the test having to count calls.
type docredactFakeModel struct {
	mu       sync.Mutex
	replies  []string
	requests []string
}

func (m *docredactFakeModel) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, string(body))
	reply := m.replies[len(m.replies)-1]
	if len(m.requests) <= len(m.replies) {
		reply = m.replies[len(m.requests)-1]
	}
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
	})
}

func (m *docredactFakeModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *docredactFakeModel) lastRequest(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		t.Fatal("the runtime was never reached")
	}
	return m.requests[len(m.requests)-1]
}

// docredactModelFixture is a paired manager with one installed, active and
// ready text model whose service port is the fake runtime's, which is how a
// model pass runs end to end with no hardware. It follows the same shape as
// newRoleFixtureWith in roles_test.go.
type docredactModelFixture struct {
	url     string
	cookies []*http.Cookie
	csrf    string
	model   *docredactFakeModel
	recipe  recipe.Recipe
	store   *store.Store
}

func newDocredactModelFixture(t *testing.T, replies ...string) *docredactModelFixture {
	t.Helper()
	fake := &docredactFakeModel{replies: replies}
	upstream := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(upstream.Close)
	port, err := strconv.Atoi(upstream.URL[strings.LastIndex(upstream.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	served := recipe.Recipe{
		ID: "redactor-model", Version: 1, DisplayName: "Redactor Model",
		Topology: recipe.Topology{SparkCount: 1},
		Runtime:  recipe.Runtime{Kind: "vllm"},
		Service:  recipe.Service{DefaultHostPort: port, ServedModelID: "publisher/redactor-model-nvfp4"},
	}
	// A second installed model that is not serving, so a test can point the
	// redactor role at something the pass must not stop the serving model for.
	idle := recipe.Recipe{
		ID: "idle-model", Version: 1, DisplayName: "Idle Model",
		Topology: recipe.Topology{SparkCount: 1},
		Runtime:  recipe.Runtime{Kind: "vllm"},
		Service:  recipe.Service{DefaultHostPort: port, ServedModelID: "publisher/idle-model-nvfp4"},
	}
	recipes := []recipe.Recipe{served, idle}

	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	authManager, err := auth.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	executor := &apiExecutor{done: map[string]bool{}, running: true}
	runner := engine.New(database, executor, recipes)
	manager := New("test-version", dataDir, authManager, database, readyInventory{}, executor, runner, recipes)
	server := httptest.NewServer(manager.Handler())
	t.Cleanup(server.Close)

	ctx := t.Context()
	for _, item := range recipes {
		if err := database.SetInstalled(ctx, store.InstalledModel{RecipeID: item.ID, RecipeVersion: item.Version, Status: "stopped", ArtifactPath: "/managed/" + item.ID, ContainerID: "container-" + item.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.ActivateExclusively(ctx, store.InstalledModel{RecipeID: served.ID, RecipeVersion: served.Version, Status: "ready", ArtifactPath: "/managed/" + served.ID, ContainerID: "container-" + served.ID}); err != nil {
		t.Fatal(err)
	}

	tokenBytes, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair",
		`{"token":"`+strings.TrimSpace(string(tokenBytes))+`"}`, nil, map[string]string{"Origin": server.URL})
	var result struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	cookies := paired.Cookies()
	paired.Body.Close()
	if len(cookies) == 0 || result.CSRF == "" {
		t.Fatal("pairing did not issue session and CSRF tokens")
	}
	return &docredactModelFixture{url: server.URL, cookies: cookies, csrf: result.CSRF, model: fake, recipe: served, store: database}
}

func (f *docredactModelFixture) assignRole(t *testing.T, role, recipeID string) {
	t.Helper()
	response := doRequest(t, http.MethodPost, f.url+"/api/v1/roles",
		`{"role":"`+role+`","recipe_id":"`+recipeID+`"}`, f.cookies,
		map[string]string{"Origin": f.url, "X-CSRF-Token": f.csrf, "Idempotency-Key": "docredact-test"})
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("assign %s status=%d body=%s", role, response.StatusCode, data)
	}
	response.Body.Close()
}

// docredactModelPassResponse mirrors the JSON the modelpass case writes: the
// whole finding list as it now stands, and the pass's own tally with the name
// of the model that answered it.
type docredactModelPassResponse struct {
	Findings  []*docredact.Finding `json:"findings"`
	ModelPass struct {
		docredact.ModelPassResult
		Model string `json:"model"`
	} `json:"model_pass"`
}

func docredactModelPass(t *testing.T, server, sessionID, csrf string, cookies []*http.Cookie, body string) *http.Response {
	t.Helper()
	return doRequest(t, http.MethodPost, server+"/api/v1/docredact/sessions/"+sessionID+"/modelpass",
		body, cookies, map[string]string{"Origin": server, "X-CSRF-Token": csrf})
}

func TestDocredactModelPassAddsWhatThePatternsMissed(t *testing.T) {
	fixture := newDocredactModelFixture(t, `[{"literal":"Jane","category":"person"}]`)
	session := docredactAnalyze(t, fixture.url, fixture.csrf, fixture.cookies, docredactSampleText, "letter.txt")

	response := docredactModelPass(t, fixture.url, session.SessionID, fixture.csrf, fixture.cookies, `{}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("model pass status=%d body=%s", response.StatusCode, data)
	}
	var result docredactModelPassResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if result.ModelPass.Accepted != 1 || result.ModelPass.Degraded || result.ModelPass.ChunksFailed != 0 {
		t.Fatalf("model pass = %+v, want one accepted literal and no degradation", result.ModelPass)
	}
	// The name of whichever model answered, so a fallback is never silent.
	if result.ModelPass.Model != "Redactor Model" {
		t.Errorf("model = %q, want the display name of the model that answered", result.ModelPass.Model)
	}
	byLiteral := map[string]*docredact.Finding{}
	for _, finding := range result.Findings {
		byLiteral[finding.Literal] = finding
	}
	person := byLiteral["Jane"]
	if person == nil {
		t.Fatalf("findings = %+v, want the literal the model named", result.Findings)
	}
	if person.Source != docredact.SourceModel {
		t.Errorf("source = %q, want %q", person.Source, docredact.SourceModel)
	}
	if byLiteral["jane.doe@example.com"] == nil {
		t.Errorf("findings = %+v, want the pattern finding kept alongside the model's", result.Findings)
	}

	// The runtime must be asked for the model id it actually serves.
	if forwarded := fixture.model.lastRequest(t); !strings.Contains(forwarded, `"model":"publisher/redactor-model-nvfp4"`) {
		t.Fatalf("forwarded body = %s, want the served model id", forwarded)
	}
}

// The redactor role decides which model a pass is sent to when its model is
// the one serving, and the answer names it.
func TestDocredactModelPassFollowsTheRedactorRole(t *testing.T) {
	fixture := newDocredactModelFixture(t, `[{"literal":"Jane","category":"person"}]`)
	fixture.assignRole(t, docredactRoleName, "redactor-model")
	session := docredactAnalyze(t, fixture.url, fixture.csrf, fixture.cookies, docredactSampleText, "letter.txt")

	response := docredactModelPass(t, fixture.url, session.SessionID, fixture.csrf, fixture.cookies, `{}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("model pass status=%d body=%s", response.StatusCode, data)
	}
	var result docredactModelPassResponse
	json.NewDecoder(response.Body).Decode(&result)
	response.Body.Close()
	if result.ModelPass.Model != "Redactor Model" || result.ModelPass.Accepted != 1 {
		t.Fatalf("model pass = %+v, want the role's model to have answered", result.ModelPass)
	}
}

// A redactor role pointing at a model that is not serving does not stop the
// one that is: the pass falls back to it, and says so by name.
func TestDocredactModelPassFallsBackVisiblyToTheServingModel(t *testing.T) {
	fixture := newDocredactModelFixture(t, `[{"literal":"Jane","category":"person"}]`)
	fixture.assignRole(t, docredactRoleName, "idle-model")
	session := docredactAnalyze(t, fixture.url, fixture.csrf, fixture.cookies, docredactSampleText, "letter.txt")

	response := docredactModelPass(t, fixture.url, session.SessionID, fixture.csrf, fixture.cookies, `{}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("model pass status=%d body=%s", response.StatusCode, data)
	}
	var result docredactModelPassResponse
	json.NewDecoder(response.Body).Decode(&result)
	response.Body.Close()
	if result.ModelPass.Model != "Redactor Model" {
		t.Fatalf("model = %q, want the model that was already serving", result.ModelPass.Model)
	}
	if forwarded := fixture.model.lastRequest(t); !strings.Contains(forwarded, `"model":"publisher/redactor-model-nvfp4"`) {
		t.Fatalf("forwarded body = %s, want the serving model id", forwarded)
	}
	// Nothing was started for the pass: the idle model is still idle.
	idle, err := fixture.store.Model(t.Context(), "idle-model")
	if err != nil {
		t.Fatal(err)
	}
	if idle.Active || idle.Status == "ready" {
		t.Fatalf("the pass activated the model the role names: %+v", idle)
	}
}

// A model that answers with nothing usable is data, not an error: the pass
// says it is degraded and every pattern finding stands exactly as it was.
func TestDocredactModelPassDegradedKeepsPatternFindings(t *testing.T) {
	fixture := newDocredactModelFixture(t, "not json at all", "still not json")
	session := docredactAnalyze(t, fixture.url, fixture.csrf, fixture.cookies, docredactSampleText, "letter.txt")

	response := docredactModelPass(t, fixture.url, session.SessionID, fixture.csrf, fixture.cookies, `{}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("model pass status=%d body=%s", response.StatusCode, data)
	}
	var result docredactModelPassResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if !result.ModelPass.Degraded {
		t.Fatalf("model pass = %+v, want degraded", result.ModelPass)
	}
	if result.ModelPass.Accepted != 0 || result.ModelPass.ChunksFailed != result.ModelPass.ChunksTotal {
		t.Errorf("model pass = %+v, want every chunk counted failed and nothing accepted", result.ModelPass)
	}
	if len(result.Findings) != 1 || result.Findings[0].Literal != "jane.doe@example.com" {
		t.Fatalf("findings = %+v, want the pattern finding untouched", result.Findings)
	}
	if result.Findings[0].Source != docredact.Source {
		t.Errorf("source = %q, want %q", result.Findings[0].Source, docredact.Source)
	}
	// The garbage reply was retried once for repair before the chunk was
	// given up on, which is what the two scripted replies are for.
	if fixture.model.count() < 2 {
		t.Errorf("runtime calls = %d, want the repair retry too", fixture.model.count())
	}
}

// Nothing serving means the pass cannot run, and the honest answer says so
// without pretending the pattern findings changed.
func TestDocredactModelPassNeedsATextModelServing(t *testing.T) {
	server, cookies, csrf := newPairedTestServer(t)
	session := docredactAnalyze(t, server.URL, csrf, cookies, docredactSampleText, "letter.txt")

	response := docredactModelPass(t, server.URL, session.SessionID, csrf, cookies, `{}`)
	if response.StatusCode != http.StatusServiceUnavailable {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("model pass status=%d body=%s", response.StatusCode, data)
	}
	var problem struct {
		Error string `json:"error"`
	}
	json.NewDecoder(response.Body).Decode(&problem)
	response.Body.Close()
	const want = "no text model is serving, so the model pass cannot run. The pattern findings are unchanged."
	if problem.Error != want {
		t.Fatalf("error = %q, want %q", problem.Error, want)
	}

	// The findings really are unchanged.
	listed := doRequest(t, http.MethodGet, server.URL+"/api/v1/docredact/sessions/"+session.SessionID+"/findings", "", cookies, nil)
	var body struct {
		Findings []*docredact.Finding `json:"findings"`
	}
	json.NewDecoder(listed.Body).Decode(&body)
	listed.Body.Close()
	if len(body.Findings) != 1 || body.Findings[0].Literal != "jane.doe@example.com" {
		t.Fatalf("findings = %+v, want the pattern finding untouched", body.Findings)
	}
}

func TestDocredactModelPassRequiresSessionAndCSRF(t *testing.T) {
	fixture := newDocredactModelFixture(t, `[]`)
	session := docredactAnalyze(t, fixture.url, fixture.csrf, fixture.cookies, docredactSampleText, "letter.txt")
	url := fixture.url + "/api/v1/docredact/sessions/" + session.SessionID + "/modelpass"

	anonymous := doRequest(t, http.MethodPost, url, `{}`, nil, map[string]string{"Origin": fixture.url})
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated model pass status=%d", anonymous.StatusCode)
	}
	anonymous.Body.Close()
	noCSRF := doRequest(t, http.MethodPost, url, `{}`, fixture.cookies, map[string]string{"Origin": fixture.url})
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", noCSRF.StatusCode)
	}
	noCSRF.Body.Close()
	if fixture.model.count() != 0 {
		t.Fatalf("the runtime was reached %d times by refused requests", fixture.model.count())
	}
}
