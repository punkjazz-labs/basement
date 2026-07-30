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
	if ContainsLikelySecret(output) {
		t.Fatalf("redacted placeholder was treated as a live secret: %q", output)
	}
	if !ContainsLikelySecret(input) {
		t.Fatal("live secret was not detected")
	}
}
