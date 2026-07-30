package redact

import (
	"encoding/json"
	"regexp"
	"strings"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+)?)[^\s,;]+`),
	regexp.MustCompile(`(?i)((?:hf|ngc|api)[_-]?(?:token|key)\s*[:=]\s*)[^\s,;]+`),
	regexp.MustCompile(`hf_[A-Za-z0-9]{12,}`),
}

func String(value string) string {
	redacted := value
	for _, pattern := range patterns {
		redacted = pattern.ReplaceAllString(redacted, `${1}[REDACTED]`)
	}
	return redacted
}

func JSON(value any) json.RawMessage {
	body, _ := json.Marshal(value)
	return json.RawMessage(String(string(body)))
}

func ContainsLikelySecret(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "hf_token=") || strings.Contains(lower, "authorization: bearer") || regexp.MustCompile(`hf_[A-Za-z0-9]{12,}`).MatchString(value)
}
