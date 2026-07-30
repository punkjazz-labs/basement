package redact

import (
	"strings"
	"testing"
)

func TestStringRedactsKnownSecretShapes(t *testing.T) {
	input := "request Authorization: Bearer " + "hf_" + "abcdefghijklmnop and HF_TOKEN=topsecret"
	output := String(input)
	if strings.Contains(output, "hf_abcdefghijklmnop") || strings.Contains(output, "topsecret") || strings.Count(output, "[REDACTED]") < 2 {
		t.Fatalf("redacted output=%q", output)
	}
}
