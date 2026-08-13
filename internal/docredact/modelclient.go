package docredact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ModelClient implements Completer against an OpenAI-compatible /v1: any
// backend that accepts a chat/completions POST and answers with the same
// choices[0].message.content shape, whether that is vLLM, llama.cpp, or
// LiteLLM in front of either.
type ModelClient struct {
	BaseURL string // e.g. "http://127.0.0.1:8000" -- no trailing /v1
	Model   string // served model id passed through in the request body
	APIKey  string // optional; sent as Bearer when non-empty (LiteLLM wants one)
	HTTP    *http.Client
}

var _ Completer = (*ModelClient)(nil)

// redactionFindingsSchema is the vLLM-style json_schema for structured
// output: an array of {literal, category} objects, matching ModelItem so a
// structured reply parses the same way as an unstructured one.
var redactionFindingsSchema = map[string]any{
	"type": "array",
	"items": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"literal":  map[string]any{"type": "string"},
			"category": map[string]any{"type": "string"},
		},
		"required": []string{"literal", "category"},
	},
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string `json:"name"`
	Schema any    `json:"schema"`
}

// chatResponse only names the field ApplyModelPass reads. A "reasoning"
// field, think tags inside content, or any other field the backend adds are
// left for json.Unmarshal to discard rather than modeled here.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// excerptLimit bounds how much of a non-200 body an error string quotes,
// so a backend that echoes a large request back in its error page does not
// blow up a log line.
const excerptLimit = 300

// Complete sends one chat/completions round to c.BaseURL and returns the
// reply text. structured asks the backend for a JSON-schema-constrained
// answer; a backend that cannot honor that (vLLM's own error shape, or any
// 400 while structured was requested -- most OpenAI-compatible backends
// reject an unsupported response_format with 400) reports
// ErrStructuredUnsupported so the caller can retry unstructured instead of
// treating the chunk as failed.
func (c *ModelClient) Complete(ctx context.Context, system, user string, structured bool) (string, error) {
	reqBody := chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
	}
	if structured {
		reqBody.ResponseFormat = &responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchema{
				Name:   "redaction_findings",
				Schema: redactionFindingsSchema,
			},
		}
	}

	encoded, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("model client: encode request: %w", err)
	}

	url := strings.TrimSuffix(c.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("model client: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("model client: request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("model client: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		mentionsResponseFormat := bytes.Contains(respBody, []byte("response_format"))
		is4xx := resp.StatusCode >= 400 && resp.StatusCode < 500
		if (is4xx && mentionsResponseFormat) || (resp.StatusCode == http.StatusBadRequest && structured) {
			return "", ErrStructuredUnsupported
		}
		return "", fmt.Errorf("model client: %s returned status %d: %s", url, resp.StatusCode, excerpt(respBody))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("model client: decode response body: %w (body: %s)", err, excerpt(respBody))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("model client: response has no choices (body: %s)", excerpt(respBody))
	}

	return parsed.Choices[0].Message.Content, nil
}

// excerpt collapses a response body to one trimmed, whitespace-flattened
// line capped at excerptLimit bytes, so an error string stays actionable
// and short instead of dumping a full HTML error page.
func excerpt(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > excerptLimit {
		s = s[:excerptLimit] + "..."
	}
	return s
}
