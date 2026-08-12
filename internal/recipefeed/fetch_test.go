package recipefeed

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// testKeypair generates an ephemeral ed25519 keypair inside the test
// process; the private key never leaves this function and is never written
// to disk. See the spec 04 executor report.
func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sign(priv ed25519.PrivateKey, message []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message)) + "\n")
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testRecipe(t *testing.T, id string, version int) recipe.Recipe {
	t.Helper()
	embedded, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(embedded, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("fixture recipe missing")
	}
	r.ID = id
	r.Version = version
	if err := recipe.Validate(r); err != nil {
		t.Fatalf("fixture recipe invalid: %v", err)
	}
	return r
}

func marshalIndex(t *testing.T, generatedAt time.Time, recipes ...recipe.Recipe) []byte {
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

// newTestFetcher builds a Fetcher wired to the given public key without
// going through recipe.IndexPublicKey(), so tests can use their own
// ephemeral keypair instead of the placeholder embedded in the binary.
func newTestFetcher(t *testing.T, embedded []recipe.Recipe, pub ed25519.PublicKey) *Fetcher {
	t.Helper()
	f := newFetcher(embedded, t.TempDir(), discardLogger(), pub)
	return f
}

func TestAcceptValidIndexUpdatesSnapshot(t *testing.T) {
	pub, priv := testKeypair(t)
	embedded := []recipe.Recipe{testRecipe(t, "recipe-a", 1)}
	f := newTestFetcher(t, embedded, pub)
	r2 := testRecipe(t, "recipe-a", 2)
	body := marshalIndex(t, time.Now(), r2)
	if err := f.accept(body, sign(priv, body), false); err != nil {
		t.Fatalf("valid index rejected: %v", err)
	}
	_, effective := f.Snapshot()
	got, ok := recipe.Find(effective, "recipe-a")
	if !ok || got.Version != 2 {
		t.Fatalf("effective catalog did not pick up the fresh version: %#v", effective)
	}
}

func TestAcceptBadSignatureLeavesRegistryUntouched(t *testing.T) {
	pub, _ := testKeypair(t)
	_, otherPriv := testKeypair(t)
	embedded := []recipe.Recipe{testRecipe(t, "recipe-a", 1)}
	f := newTestFetcher(t, embedded, pub)
	r2 := testRecipe(t, "recipe-a", 2)
	body := marshalIndex(t, time.Now(), r2)
	if err := f.accept(body, sign(otherPriv, body), false); err == nil {
		t.Fatal("index signed by the wrong key was accepted")
	}
	_, effective := f.Snapshot()
	got, ok := recipe.Find(effective, "recipe-a")
	if !ok || got.Version != 1 {
		t.Fatalf("a rejected index changed the effective catalog: %#v", effective)
	}
}

func TestAcceptRejectsDowngrade(t *testing.T) {
	pub, priv := testKeypair(t)
	embedded := []recipe.Recipe{testRecipe(t, "recipe-a", 1)}
	f := newTestFetcher(t, embedded, pub)

	newer := marshalIndex(t, time.Unix(2000, 0), testRecipe(t, "recipe-a", 2))
	if err := f.accept(newer, sign(priv, newer), false); err != nil {
		t.Fatalf("first (newer) index rejected: %v", err)
	}

	older := marshalIndex(t, time.Unix(1000, 0), testRecipe(t, "recipe-a", 3))
	err := f.accept(older, sign(priv, older), false)
	if err == nil {
		t.Fatal("an index older than the last accepted one was not rejected")
	}
	if !strings.Contains(err.Error(), "older than the last accepted index") {
		t.Fatalf("expected a downgrade-specific error, got: %v", err)
	}

	// The rejected (replayed/older) index must not have taken effect even
	// though it carried a higher recipe version than what's active.
	_, effective := f.Snapshot()
	got, ok := recipe.Find(effective, "recipe-a")
	if !ok || got.Version != 2 {
		t.Fatalf("downgrade rejection did not leave the previously accepted version in place: %#v", effective)
	}
}

func TestAcceptSameGeneratedAtIsANoOpNotAnError(t *testing.T) {
	pub, priv := testKeypair(t)
	embedded := []recipe.Recipe{testRecipe(t, "recipe-a", 1)}
	f := newTestFetcher(t, embedded, pub)
	generatedAt := time.Unix(5000, 0)
	body := marshalIndex(t, generatedAt, testRecipe(t, "recipe-a", 2))
	if err := f.accept(body, sign(priv, body), false); err != nil {
		t.Fatal(err)
	}
	if err := f.accept(body, sign(priv, body), false); err != nil {
		t.Fatalf("resending the same index should not be treated as a downgrade: %v", err)
	}
}

func TestFetcherAllRetainsOldVersionsAfterUpdate(t *testing.T) {
	pub, priv := testKeypair(t)
	embedded := []recipe.Recipe{testRecipe(t, "recipe-a", 1)}
	f := newTestFetcher(t, embedded, pub)
	body := marshalIndex(t, time.Now(), testRecipe(t, "recipe-a", 2))
	if err := f.accept(body, sign(priv, body), false); err != nil {
		t.Fatal(err)
	}
	all, effective := f.Snapshot()
	if _, ok := recipe.FindVersion(all, "recipe-a", 1); !ok {
		t.Fatal("the old (embedded) version must remain resolvable in the accumulated history")
	}
	if _, ok := recipe.FindVersion(all, "recipe-a", 2); !ok {
		t.Fatal("the new version must be resolvable in the accumulated history")
	}
	if got, ok := recipe.Find(effective, "recipe-a"); !ok || got.Version != 2 {
		t.Fatalf("effective catalog should hold only the newest version: %#v", got)
	}
}

func TestInvalidRecipeInFetchedIndexIsDroppedNotFatal(t *testing.T) {
	pub, priv := testKeypair(t)
	f := newTestFetcher(t, nil, pub)
	good := testRecipe(t, "good", 1)
	bad := testRecipe(t, "bad", 1)
	bad.Runtime.Digest = "sha256:not-real"
	body := marshalIndex(t, time.Now(), good, bad)
	if err := f.accept(body, sign(priv, body), false); err != nil {
		t.Fatalf("a batch with one invalid recipe must not fail outright: %v", err)
	}
	_, effective := f.Snapshot()
	if _, ok := recipe.Find(effective, "good"); !ok {
		t.Fatal("the valid recipe in the batch should have been kept")
	}
	if _, ok := recipe.Find(effective, "bad"); ok {
		t.Fatal("the invalid recipe should have been dropped")
	}
}

// --- HTTP integration: fetch, verify, cache, and merge end to end ---

func newIndexServer(t *testing.T, indexBody, sigBody []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(indexBody)
	})
	mux.HandleFunc("/index.json.sig", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sigBody)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestRefreshOnceFetchesVerifiesCachesAndMerges(t *testing.T) {
	pub, priv := testKeypair(t)
	remote := testRecipe(t, "remote-a", 5)
	body := marshalIndex(t, time.Now(), remote)
	server := newIndexServer(t, body, sign(priv, body))

	dataDir := t.TempDir()
	f := newFetcher(nil, dataDir, discardLogger(), pub)
	f.indexURL = server.URL + "/index.json"

	if err := f.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	_, effective := f.Snapshot()
	got, ok := recipe.Find(effective, "remote-a")
	if !ok || got.Version != 5 {
		t.Fatalf("fetched recipe did not merge into the effective catalog: %#v", effective)
	}

	// The verified bytes must have been cached to disk for offline reuse.
	indexPath := filepath.Join(dataDir, "recipes-cache", "index.json")
	sigPath := indexPath + ".sig"
	for _, p := range []string{indexPath, sigPath} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("expected %s to be written: %v", p, statErr)
		}
	}

	// A fresh Fetcher pointed at the same data dir, with no network access,
	// must recover the cached recipe from disk alone (offline fallback).
	offline := newFetcher(nil, dataDir, discardLogger(), pub)
	_, offlineEffective := offline.Snapshot()
	if got, ok := recipe.Find(offlineEffective, "remote-a"); !ok || got.Version != 5 {
		t.Fatalf("cached index was not recovered offline: %#v", offlineEffective)
	}
}

func TestRefreshOnceRejectsOversizedIndex(t *testing.T) {
	pub, priv := testKeypair(t)
	oversized := make([]byte, maxIndexBytes+1024)
	server := newIndexServer(t, oversized, sign(priv, oversized))

	f := newFetcher(nil, t.TempDir(), discardLogger(), pub)
	f.indexURL = server.URL + "/index.json"
	if err := f.RefreshOnce(context.Background()); err == nil {
		t.Fatal("oversized index response was not rejected")
	}
}

func TestRefreshOnceNetworkFailureLeavesEmbeddedRecipesUntouched(t *testing.T) {
	pub, _ := testKeypair(t)
	embedded := []recipe.Recipe{testRecipe(t, "recipe-a", 1)}
	// Point at a server that always 500s, simulating a network/availability
	// failure rather than a signature problem.
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	f := newFetcher(embedded, t.TempDir(), discardLogger(), pub)
	f.indexURL = server.URL + "/index.json"
	before, beforeEffective := f.Snapshot()

	if err := f.RefreshOnce(context.Background()); err == nil {
		t.Fatal("expected the fetch to fail")
	}
	after, afterEffective := f.Snapshot()
	if fmt.Sprint(before) != fmt.Sprint(after) || fmt.Sprint(beforeEffective) != fmt.Sprint(afterEffective) {
		t.Fatalf("a failed fetch changed the registry:\nbefore=%#v\nafter=%#v", beforeEffective, afterEffective)
	}
	if got, ok := recipe.Find(afterEffective, "recipe-a"); !ok || got.Version != 1 {
		t.Fatalf("embedded recipe was not left in place after a network failure: %#v", afterEffective)
	}
}
