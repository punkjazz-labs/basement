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
	if regexp.MustCompile(`hf_[A-Za-z0-9]{12,}`).MatchString(value) {
		return true
	}
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:hf|ngc|api)[_-]?(?:token|key)\s*[:=]\s*([^\s,;]+)`),
		regexp.MustCompile(`(?i)authorization\s*[:=]\s*bearer\s+([^\s,;]+)`),
	} {
		for _, match := range pattern.FindAllStringSubmatch(value, -1) {
			if len(match) > 1 && !strings.EqualFold(match[1], "[REDACTED]") {
				return true
			}
		}
	}
	return false
}
