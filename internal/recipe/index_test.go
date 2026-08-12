package recipe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// validTestRecipe clones an embedded recipe (already known to satisfy
// Validate) and gives it a fresh ID and version, so index tests exercise
// the real validator instead of a hand-rolled fixture that might drift from
// what Validate actually enforces.
func validTestRecipe(t *testing.T, id string, version int) Recipe {
	t.Helper()
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("fixture recipe missing")
	}
	r.ID = id
	r.Version = version
	if err := Validate(r); err != nil {
		t.Fatalf("fixture recipe is not valid: %v", err)
	}
	return r
}

func marshalIndex(t *testing.T, generatedAt time.Time, recipes ...Recipe) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"generated_at":   generatedAt.Format(time.RFC3339),
		"recipes":        recipes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// marshalIndexWithRevocations builds an index whose revoked array is written
// from raw maps, so a test can express shapes the Go type cannot — which is
// the whole point of the strict decoder.
func marshalIndexWithRevocations(t *testing.T, generatedAt time.Time, revoked []map[string]any, recipes ...Recipe) []byte {
	t.Helper()
	if recipes == nil {
		recipes = []Recipe{}
	}
	body, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"generated_at":   generatedAt.Format(time.RFC3339),
		"recipes":        recipes,
		"revoked":        revoked,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestVerifyAndParseIndexAcceptsRevocations(t *testing.T) {
	pub, priv := testKeypair(t)
	revokedAt := time.Now().UTC().Truncate(time.Second)
	body := marshalIndexWithRevocations(t, time.Now(), []map[string]any{{
		"id":         "remote-example-1s",
		"version":    2,
		"reason":     "the published weights were the wrong quantisation",
		"revoked_at": revokedAt.Format(time.RFC3339),
	}}, validTestRecipe(t, "remote-example-1s", 3))
	idx, reasons, err := VerifyAndParseIndex(body, sign(priv, body), pub)
	if err != nil {
		t.Fatalf("index with a well-formed revocation rejected: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("valid index produced drop reasons: %v", reasons)
	}
	if len(idx.Revoked) != 1 {
		t.Fatalf("expected one revocation, got: %#v", idx.Revoked)
	}
	got := idx.Revoked[0]
	if got.ID != "remote-example-1s" || got.Version != 2 {
		t.Fatalf("revocation names the wrong recipe version: %#v", got)
	}
	if got.Reason != "the published weights were the wrong quantisation" {
		t.Fatalf("reason was not carried verbatim: %q", got.Reason)
	}
	if !got.RevokedAt.Equal(revokedAt) {
		t.Fatalf("revoked_at=%s, want %s", got.RevokedAt, revokedAt)
	}
	// Revoking version 2 says nothing about version 3, which is still
	// published in the same index and must still arrive.
	if len(idx.Recipes) != 1 || idx.Recipes[0].Version != 3 {
		t.Fatalf("a revocation removed an unrelated recipe version: %#v", idx.Recipes)
	}
}

func TestVerifyAndParseIndexRejectsMalformedRevocations(t *testing.T) {
	// Every one of these is refused outright rather than dropped with a
	// reason: a revocation this manager cannot read is a safety statement it
	// would be silently discarding, and the wildcard shapes are refused
	// because the schema must not be able to express "all versions" at all.
	cases := []struct {
		name  string
		entry map[string]any
	}{
		{"missing reason", map[string]any{"id": "remote-example-1s", "version": 1, "revoked_at": "2026-08-12T00:00:00Z"}},
		{"blank reason", map[string]any{"id": "remote-example-1s", "version": 1, "reason": "   ", "revoked_at": "2026-08-12T00:00:00Z"}},
		{"missing id", map[string]any{"version": 1, "reason": "licence problem", "revoked_at": "2026-08-12T00:00:00Z"}},
		{"missing version", map[string]any{"id": "remote-example-1s", "reason": "licence problem", "revoked_at": "2026-08-12T00:00:00Z"}},
		{"wildcard version", map[string]any{"id": "remote-example-1s", "version": "*", "reason": "licence problem", "revoked_at": "2026-08-12T00:00:00Z"}},
		{"version range", map[string]any{"id": "remote-example-1s", "versions": "<=3", "reason": "licence problem", "revoked_at": "2026-08-12T00:00:00Z"}},
		{"all versions flag", map[string]any{"id": "remote-example-1s", "version": 1, "all_versions": true, "reason": "licence problem", "revoked_at": "2026-08-12T00:00:00Z"}},
		{"product wide", map[string]any{"id": "*", "version": 0, "reason": "licence problem", "revoked_at": "2026-08-12T00:00:00Z"}},
		{"missing revoked_at", map[string]any{"id": "remote-example-1s", "version": 1, "reason": "licence problem"}},
		{"revoked_at is not RFC3339", map[string]any{"id": "remote-example-1s", "version": 1, "reason": "licence problem", "revoked_at": "12 August 2026"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pub, priv := testKeypair(t)
			body := marshalIndexWithRevocations(t, time.Now(), []map[string]any{testCase.entry})
			if _, _, err := VerifyAndParseIndex(body, sign(priv, body), pub); err == nil {
				t.Fatalf("index carrying a %s revocation was accepted", testCase.name)
			}
		})
	}
}

func TestVerifyAndParseIndexStillRejectsUnknownTopLevelFieldAlongsideRevocations(t *testing.T) {
	// revoked entering the current schema version does not loosen the
	// decoder: it remains the case that any other new top-level field is a
	// schema version bump by definition.
	pub, priv := testKeypair(t)
	body, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"generated_at":   time.Now().Format(time.RFC3339),
		"recipes":        []Recipe{},
		"revoked":        []map[string]any{},
		"unrevoked":      []map[string]any{{"id": "remote-example-1s", "version": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyAndParseIndex(body, sign(priv, body), pub); err == nil {
		t.Fatal("index with an unknown top-level field alongside revoked was accepted")
	}
}

func TestVerifyAndParseIndexAcceptsAnIndexWithNoRevocations(t *testing.T) {
	// No feed publishes revocations most of the time, and an index that
	// simply omits the array is the ordinary case, not a malformed document.
	pub, priv := testKeypair(t)
	body := marshalIndex(t, time.Now(), validTestRecipe(t, "remote-example-1s", 1))
	idx, _, err := VerifyAndParseIndex(body, sign(priv, body), pub)
	if err != nil {
		t.Fatalf("index without a revoked array rejected: %v", err)
	}
	if len(idx.Revoked) != 0 {
		t.Fatalf("expected no revocations, got: %#v", idx.Revoked)
	}
}

func TestVerifyAndParseIndexAcceptsValidIndex(t *testing.T) {
	pub, priv := testKeypair(t)
	r := validTestRecipe(t, "remote-example-1s", 1)
	body := marshalIndex(t, time.Now(), r)
	idx, reasons, err := VerifyAndParseIndex(body, sign(priv, body), pub)
	if err != nil {
		t.Fatalf("valid index rejected: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("valid index produced drop reasons: %v", reasons)
	}
	if len(idx.Recipes) != 1 || idx.Recipes[0].ID != "remote-example-1s" {
		t.Fatalf("unexpected recipes in parsed index: %#v", idx.Recipes)
	}
}

func TestVerifyAndParseIndexRejectsBadSignature(t *testing.T) {
	pub, _ := testKeypair(t)
	_, otherPriv := testKeypair(t)
	r := validTestRecipe(t, "remote-example-1s", 1)
	body := marshalIndex(t, time.Now(), r)
	if _, _, err := VerifyAndParseIndex(body, sign(otherPriv, body), pub); err == nil {
		t.Fatal("index signed by the wrong key was accepted")
	}
}

func TestVerifyAndParseIndexNeverParsesBeforeVerifying(t *testing.T) {
	pub, _ := testKeypair(t)
	// Bytes that are not even valid JSON: if verification ran after parsing
	// (or parsing ran regardless of verification), this would fail with a
	// JSON decode error instead of a signature error, revealing that
	// unverified bytes reached the parser.
	body := []byte("not json at all { [ }")
	_, _, err := VerifyAndParseIndex(body, []byte("bm90LWEtcmVhbC1zaWduYXR1cmU="), pub)
	if err == nil {
		t.Fatal("garbage index with a garbage signature was accepted")
	}
	if !strings.Contains(err.Error(), "verify index signature") {
		t.Fatalf("expected a signature-verification error before any parsing, got: %v", err)
	}
}

func TestVerifyAndParseIndexDemotesWireVerifiedClaimsToCandidate(t *testing.T) {
	// A verified label is earned by qualification evidence recorded in this
	// repository. A correctly signed index claiming otherwise hands out a
	// label nobody earned, so the claim arrives demoted, with a reason the
	// operator can read, rather than trusted or silently rewritten.
	pub, priv := testKeypair(t)
	r := validTestRecipe(t, "remote-claims-verified-1s", 1)
	r.Trust = "basement-verified"
	r.Verification = "dgx-spark-verified"
	body := marshalIndex(t, time.Now(), r)
	idx, reasons, err := VerifyAndParseIndex(body, sign(priv, body), pub)
	if err != nil {
		t.Fatalf("a signed index with an inflated label must still parse: %v", err)
	}
	if len(idx.Recipes) != 1 || idx.Recipes[0].Trust != "basement-candidate" || idx.Recipes[0].Verification != "candidate" {
		t.Fatalf("expected the recipe demoted to candidate, got: %#v", idx.Recipes)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "demoted to candidate") {
		t.Fatalf("expected a demotion reason, got: %v", reasons)
	}
}

func TestVerifyAndParseIndexDropsInvalidRecipeWithoutPoisoningBatch(t *testing.T) {
	pub, priv := testKeypair(t)
	good := validTestRecipe(t, "remote-good-1s", 1)
	bad := validTestRecipe(t, "remote-bad-1s", 1)
	bad.Runtime.Digest = "sha256:not-a-real-digest" // fails Validate's digest pattern
	body := marshalIndex(t, time.Now(), good, bad)
	idx, reasons, err := VerifyAndParseIndex(body, sign(priv, body), pub)
	if err != nil {
		t.Fatalf("a batch with one invalid recipe should not fail outright: %v", err)
	}
	if len(idx.Recipes) != 1 || idx.Recipes[0].ID != "remote-good-1s" {
		t.Fatalf("expected only the good recipe to survive, got: %#v", idx.Recipes)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "remote-bad-1s") {
		t.Fatalf("expected a drop reason naming the bad recipe, got: %v", reasons)
	}
}

func TestVerifyAndParseIndexDropsRecipeWithUnknownField(t *testing.T) {
	pub, priv := testKeypair(t)
	good := validTestRecipe(t, "remote-good-1s", 1)
	goodJSON, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var withExtra map[string]any
	if err := json.Unmarshal(goodJSON, &withExtra); err != nil {
		t.Fatal(err)
	}
	withExtra["id"] = "remote-extra-1s"
	withExtra["something_from_the_future"] = true
	body, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"generated_at":   time.Now().Format(time.RFC3339),
		"recipes":        []any{withExtra},
	})
	if err != nil {
		t.Fatal(err)
	}
	idx, reasons, err := VerifyAndParseIndex(body, sign(priv, body), pub)
	if err != nil {
		t.Fatalf("a single malformed recipe entry should not fail the whole index: %v", err)
	}
	if len(idx.Recipes) != 0 {
		t.Fatalf("recipe with an unknown field should have been dropped, got: %#v", idx.Recipes)
	}
	if len(reasons) != 1 {
		t.Fatalf("expected exactly one drop reason, got: %v", reasons)
	}
}

func TestVerifyAndParseIndexRejectsUnsupportedSchemaVersion(t *testing.T) {
	pub, priv := testKeypair(t)
	body, err := json.Marshal(map[string]any{
		"schema_version": 2,
		"generated_at":   time.Now().Format(time.RFC3339),
		"recipes":        []Recipe{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyAndParseIndex(body, sign(priv, body), pub); err == nil {
		t.Fatal("unsupported schema_version was accepted")
	}
}

func TestVerifyAndParseIndexRejectsUnknownTopLevelField(t *testing.T) {
	pub, priv := testKeypair(t)
	body, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"generated_at":   time.Now().Format(time.RFC3339),
		"recipes":        []Recipe{},
		"channel":        "beta", // multiple channels are an explicit non-goal
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyAndParseIndex(body, sign(priv, body), pub); err == nil {
		t.Fatal("index with an unknown top-level field was accepted")
	}
}
