package docredact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capturedRequest snapshots the one request an httptest handler saw, so a
// test can assert on it after Complete has already returned.
type capturedRequest struct {
	method string
	path   string
	header http.Header
	body   map[string]any
}

// newModelServer starts an httptest server that records the single request
// it receives into captured and answers with status/body. Tests that need
// to see what ModelClient sent read captured after calling Complete.
func newModelServer(t *testing.T, status int, body string, captured *capturedRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.header = r.Header.Clone()
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &captured.body); err != nil {
				t.Fatalf("request body is not JSON: %v (body=%s)", err, raw)
			}
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelClientRequestShapeUnstructured(t *testing.T) {
	var captured capturedRequest
	srv := newModelServer(t, http.StatusOK, `{"choices":[{"message":{"content":"[]"}}]}`, &captured)

	c := &ModelClient{BaseURL: srv.URL, Model: "the-served-model"}
	reply, err := c.Complete(context.Background(), "system prompt", "user prompt", false)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reply != "[]" {
		t.Errorf("reply = %q, want %q", reply, "[]")
	}

	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", captured.path)
	}
	if got := captured.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want none (no APIKey set)", got)
	}

	if got := captured.body["model"]; got != "the-served-model" {
		t.Errorf("model = %v, want %q", got, "the-served-model")
	}
	if got := captured.body["temperature"]; got != 0.0 {
		t.Errorf("temperature = %v, want 0", got)
	}
	if _, ok := captured.body["response_format"]; ok {
		t.Errorf("response_format present, want omitted when structured=false")
	}

	messages, ok := captured.body["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %v, want a 2-element array", captured.body["messages"])
	}
	sys := messages[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "system prompt" {
		t.Errorf("messages[0] = %v, want system/%q", sys, "system prompt")
	}
	user := messages[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "user prompt" {
		t.Errorf("messages[1] = %v, want user/%q", user, "user prompt")
	}
}

func TestModelClientStructuredAddsJSONSchema(t *testing.T) {
	var captured capturedRequest
	srv := newModelServer(t, http.StatusOK, `{"choices":[{"message":{"content":"[]"}}]}`, &captured)

	c := &ModelClient{BaseURL: srv.URL, Model: "m"}
	if _, err := c.Complete(context.Background(), "s", "u", true); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	rf, ok := captured.body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format = %v, want an object", captured.body["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("response_format.json_schema = %v, want an object", rf["json_schema"])
	}
	if js["name"] != "redaction_findings" {
		t.Errorf("json_schema.name = %v, want redaction_findings", js["name"])
	}
	schema, ok := js["schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema.schema = %v, want an object", js["schema"])
	}
	if schema["type"] != "array" {
		t.Errorf("schema.type = %v, want array", schema["type"])
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		t.Fatalf("schema.items = %v, want an object", schema["items"])
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.items.properties = %v, want an object", items["properties"])
	}
	if _, ok := props["literal"]; !ok {
		t.Errorf("properties missing literal: %v", props)
	}
	if _, ok := props["category"]; !ok {
		t.Errorf("properties missing category: %v", props)
	}
	required, ok := items["required"].([]any)
	if !ok || len(required) != 2 {
		t.Fatalf("schema.items.required = %v, want [literal category]", items["required"])
	}
}

func TestModelClientAuthorizationHeader(t *testing.T) {
	cases := []struct {
		name    string
		apiKey  string
		wantSet bool
	}{
		{"no api key", "", false},
		{"api key set", "sk-secret", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured capturedRequest
			srv := newModelServer(t, http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`, &captured)

			c := &ModelClient{BaseURL: srv.URL, Model: "m", APIKey: tc.apiKey}
			if _, err := c.Complete(context.Background(), "s", "u", false); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			got := captured.header.Get("Authorization")
			if tc.wantSet && got != "Bearer "+tc.apiKey {
				t.Errorf("Authorization = %q, want %q", got, "Bearer "+tc.apiKey)
			}
			if !tc.wantSet && got != "" {
				t.Errorf("Authorization = %q, want none", got)
			}
		})
	}
}

func TestModelClient400MentioningResponseFormatIsStructuredUnsupported(t *testing.T) {
	var captured capturedRequest
	srv := newModelServer(t, http.StatusBadRequest, `{"error":"unknown field response_format"}`, &captured)

	c := &ModelClient{BaseURL: srv.URL, Model: "m"}
	_, err := c.Complete(context.Background(), "s", "u", false)
	if !errors.Is(err, ErrStructuredUnsupported) {
		t.Fatalf("err = %v, want ErrStructuredUnsupported", err)
	}
}

func TestModelClientAny400WhileStructuredIsStructuredUnsupported(t *testing.T) {
	var captured capturedRequest
	srv := newModelServer(t, http.StatusBadRequest, `{"error":"malformed request"}`, &captured)

	c := &ModelClient{BaseURL: srv.URL, Model: "m"}
	_, err := c.Complete(context.Background(), "s", "u", true)
	if !errors.Is(err, ErrStructuredUnsupported) {
		t.Fatalf("err = %v, want ErrStructuredUnsupported", err)
	}
}

func TestModelClient400UnstructuredUnrelatedIsPlainError(t *testing.T) {
	var captured capturedRequest
	srv := newModelServer(t, http.StatusBadRequest, `{"error":"missing required field prompt"}`, &captured)

	c := &ModelClient{BaseURL: srv.URL, Model: "m"}
	_, err := c.Complete(context.Background(), "s", "u", false)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if errors.Is(err, ErrStructuredUnsupported) {
		t.Fatalf("err = %v, want a plain error, not ErrStructuredUnsupported", err)
	}
}

func TestModelClientOtherNon200SurfacesStatusAndBody(t *testing.T) {
	var captured capturedRequest
	srv := newModelServer(t, http.StatusInternalServerError, `internal explosion detail xyz`, &captured)

	c := &ModelClient{BaseURL: srv.URL, Model: "m"}
	_, err := c.Complete(context.Background(), "s", "u", false)
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if errors.Is(err, ErrStructuredUnsupported) {
		t.Fatalf("err = %v, want a plain error, not ErrStructuredUnsupported", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("error %q does not mention status 500", msg)
	}
	if !strings.Contains(msg, "internal explosion detail xyz") {
		t.Errorf("error %q does not include the body excerpt", msg)
	}
}

func TestModelClientReplyReadFromContentOnly(t *testing.T) {
	var captured capturedRequest
	srv := newModelServer(t, http.StatusOK, `{"choices":[{"message":{"content":"the actual reply","reasoning":"<think>ignore me</think>"}}],"extra":"ignore me too"}`, &captured)

	c := &ModelClient{BaseURL: srv.URL, Model: "m"}
	reply, err := c.Complete(context.Background(), "s", "u", false)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reply != "the actual reply" {
		t.Errorf("reply = %q, want %q", reply, "the actual reply")
	}
}

func TestModelClientTransportErrorIsPlainError(t *testing.T) {
	// Nothing listens here; the request never reaches a server.
	c := &ModelClient{BaseURL: "http://127.0.0.1:1", Model: "m"}
	_, err := c.Complete(context.Background(), "s", "u", false)
	if err == nil {
		t.Fatal("err = nil, want a transport error")
	}
	if errors.Is(err, ErrStructuredUnsupported) {
		t.Fatalf("err = %v, want a plain error, not ErrStructuredUnsupported", err)
	}
}
