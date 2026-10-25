package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	if _, err := run(outPath, ""); err != nil {
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

	// Embedded history can contain several versions of one ID. Verify every
	// exact version survives instead of letting an ID-only map hide one.
	for _, want := range embedded {
		got, ok := recipe.FindVersion(idx.Recipes, want.ID, want.Version)
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
	if _, err := run(outPath, ""); err != nil {
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

// TestSummaryNamesTheRecipeCount pins the summary line, because
// packaging/publish-feed.sh reads the recipe count out of it for the feed
// repository's commit message.
func TestSummaryNamesTheRecipeCount(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "index.json")
	summary, err := run(outPath, "")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	embedded, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	want := "wrote " + outPath + ": " + strconv.Itoa(len(embedded)) + " recipe(s)"
	if summary != want {
		t.Fatalf("summary = %q, want %q", summary, want)
	}
}

// TestGeneratedAtFlagIsUsedVerbatim proves the -generated-at flag actually
// controls the timestamp written, which is the whole point of the flag: a
// publish must be reproducible byte for byte given the same binary and the
// same -generated-at.
func TestGeneratedAtFlagIsUsedVerbatim(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "index.json")
	want := "2026-08-12T00:00:00Z"
	if _, err := run(outPath, want); err != nil {
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
	if _, err := run(outPath, "12 August 2026"); err == nil {
		t.Fatal("a malformed -generated-at value was accepted")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatal("a failed build left an output file behind")
	}
}
