package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// testKeypair generates an ephemeral ed25519 keypair inside the test
// process, exactly the way cmd/sign-index signs a real index, without ever
// writing a private key to disk.
func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sign(priv ed25519.PrivateKey, message []byte) []byte {
	signature := ed25519.Sign(priv, message)
	return []byte(base64.StdEncoding.EncodeToString(signature) + "\n")
}

// TestRoundTripSurvivesTheClientDecodePath is the load-bearing test. It
// builds an index exactly the way this tool does, signs it exactly the way
// cmd/sign-index does, and decodes it exactly the way the manager's
// recipefeed client does (recipe.VerifyAndParseIndex), then checks that
// every embedded recipe arrives with its identity intact and that merging
// the result back into the embedded catalog changes nothing. A freshly
// built index must never change the effective catalog of the binary that
// built it: if it did, the moment a build was published, every manager
// that had already trusted its own embedded pack would see its catalog
// shift for no reason but its own publish.
func TestRoundTripSurvivesTheClientDecodePath(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "index.json")
	if _, err := run(outPath, "", ""); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	indexBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}

	embedded, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}

	pub, priv := testKeypair(t)
	sig := sign(priv, indexBytes)
	idx, reasons, err := recipe.VerifyAndParseIndex(indexBytes, sig, pub)
	if err != nil {
		t.Fatalf("the built index did not survive the client's own decode path: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("the client dropped recipes from a fresh, unmodified build: %v", reasons)
	}
	if len(idx.Recipes) != len(embedded) {
		t.Fatalf("got %d recipes back, want %d embedded", len(idx.Recipes), len(embedded))
	}

	fetchedByID := make(map[string]recipe.Recipe, len(idx.Recipes))
	for _, r := range idx.Recipes {
		fetchedByID[r.ID] = r
	}
	for _, want := range embedded {
		got, ok := fetchedByID[want.ID]
		if !ok {
			t.Fatalf("recipe %s did not survive the round trip", want.ID)
		}
		if got.ID != want.ID || got.Version != want.Version || got.Runtime.Digest != want.Runtime.Digest {
			t.Fatalf("recipe %s changed identity in the round trip: got id=%s version=%d digest=%s, want id=%s version=%d digest=%s",
				want.ID, got.ID, got.Version, got.Runtime.Digest, want.ID, want.Version, want.Runtime.Digest)
		}
	}

	// The core guarantee: overlaying the fetched layer on top of the
	// embedded one must produce exactly the catalog embedded alone already
	// produces.
	effective := recipe.Merge(embedded, nil, idx.Recipes)
	baseline := recipe.Merge(embedded, nil, nil)
	if !reflect.DeepEqual(effective, baseline) {
		t.Fatalf("merging the freshly built index changed the effective catalog:\ngot:  %#v\nwant: %#v", effective, baseline)
	}
}

// TestOutputEndsInExactlyOneNewline guards the exact on-disk shape the
// tool promises: a text file, not a byte stream with no trailing newline
// and not one with a blank line appended after it.
func TestOutputEndsInExactlyOneNewline(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "index.json")
	if _, err := run(outPath, "", ""); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("output does not end in a newline")
	}
	if strings.HasSuffix(body, "\n\n") {
		t.Fatal("output ends in more than one newline")
	}
}

// TestRevokedFileWithDuplicateEntryFails covers the one check that is this
// tool's own, not the wire format's: two entries naming the same recipe at
// the same version is either a no-op duplicate or a contradiction, and
// either way it belongs to whoever drafted the file to resolve, not to
// every manager that would otherwise fetch the ambiguity.
func TestRevokedFileWithDuplicateEntryFails(t *testing.T) {
	dir := t.TempDir()
	revokedPath := filepath.Join(dir, "revoked.json")
	body := `[
		{"id": "some-recipe-1s", "version": 1, "reason": "wrong quantisation", "revoked_at": "2026-08-12T00:00:00Z"},
		{"id": "some-recipe-1s", "version": 1, "reason": "duplicate of the entry above", "revoked_at": "2026-08-13T00:00:00Z"}
	]`
	if err := os.WriteFile(revokedPath, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "index.json")
	if _, err := run(outPath, revokedPath, ""); err == nil {
		t.Fatal("a revoked file with a duplicate (id, version) entry was accepted")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatal("a failed build left an output file behind")
	}
}

// TestMalformedRevokedFileFails covers every other way a -revoked file can
// be wrong: not JSON at all, an unknown field (the same strictness the
// client's own decoder holds the wire format to), and each required field
// missing in turn.
func TestMalformedRevokedFileFails(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not JSON at all", `not json`},
		{"unknown field", `[{"id": "x-1s", "version": 1, "reason": "r", "revoked_at": "2026-08-12T00:00:00Z", "all_versions": true}]`},
		{"missing id", `[{"version": 1, "reason": "r", "revoked_at": "2026-08-12T00:00:00Z"}]`},
		{"missing reason", `[{"id": "x-1s", "version": 1, "revoked_at": "2026-08-12T00:00:00Z"}]`},
		{"blank reason", `[{"id": "x-1s", "version": 1, "reason": "   ", "revoked_at": "2026-08-12T00:00:00Z"}]`},
		{"zero version", `[{"id": "x-1s", "version": 0, "reason": "r", "revoked_at": "2026-08-12T00:00:00Z"}]`},
		{"missing revoked_at", `[{"id": "x-1s", "version": 1, "reason": "r"}]`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			revokedPath := filepath.Join(dir, "revoked.json")
			if err := os.WriteFile(revokedPath, []byte(testCase.body), 0o640); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(dir, "index.json")
			if _, err := run(outPath, revokedPath, ""); err == nil {
				t.Fatalf("malformed revoked file (%s) was accepted", testCase.name)
			}
			if _, err := os.Stat(outPath); !os.IsNotExist(err) {
				t.Fatal("a failed build left an output file behind")
			}
		})
	}
}

// TestValidRevocationIsIncludedAndSurvivesTheClientDecodePath is the
// positive counterpart of the two tests above: a well-formed -revoked file
// actually reaches the output, unchanged, through the same client decode
// path the round-trip test exercises.
func TestValidRevocationIsIncludedAndSurvivesTheClientDecodePath(t *testing.T) {
	dir := t.TempDir()
	revokedPath := filepath.Join(dir, "revoked.json")
	body := `[{"id": "some-recipe-1s", "version": 1, "reason": "the published weights were the wrong quantisation", "revoked_at": "2026-08-12T00:00:00Z"}]`
	if err := os.WriteFile(revokedPath, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "index.json")
	if _, err := run(outPath, revokedPath, ""); err != nil {
		t.Fatalf("run failed on a well-formed revoked file: %v", err)
	}
	indexBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv := testKeypair(t)
	idx, _, err := recipe.VerifyAndParseIndex(indexBytes, sign(priv, indexBytes), pub)
	if err != nil {
		t.Fatalf("index with a valid revocation did not survive the client's decode path: %v", err)
	}
	if len(idx.Revoked) != 1 {
		t.Fatalf("expected one revocation in the built index, got: %#v", idx.Revoked)
	}
	got := idx.Revoked[0]
	if got.ID != "some-recipe-1s" || got.Version != 1 || got.Reason != "the published weights were the wrong quantisation" {
		t.Fatalf("revocation was not carried through unchanged: %#v", got)
	}
}

// TestGeneratedAtFlagIsUsedVerbatim proves the -generated-at flag actually
// controls the timestamp written, which is the whole point of the flag: a
// publish must be reproducible byte for byte given the same binary, the
// same revoked file, and the same -generated-at.
func TestGeneratedAtFlagIsUsedVerbatim(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "index.json")
	want := "2026-08-12T00:00:00Z"
	if _, err := run(outPath, "", want); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		GeneratedAt time.Time `json:"generated_at"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	wantTime, err := time.Parse(time.RFC3339, want)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.GeneratedAt.Equal(wantTime) {
		t.Fatalf("generated_at = %s, want %s", decoded.GeneratedAt, wantTime)
	}
}

// TestGeneratedAtFlagRejectsAMalformedTimestamp keeps the flag from
// silently accepting something that is not RFC3339, which would otherwise
// surface only much later as a client-side rejection of the published
// index.
func TestGeneratedAtFlagRejectsAMalformedTimestamp(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "index.json")
	if _, err := run(outPath, "", "12 August 2026"); err == nil {
		t.Fatal("a malformed -generated-at value was accepted")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatal("a failed build left an output file behind")
	}
}
