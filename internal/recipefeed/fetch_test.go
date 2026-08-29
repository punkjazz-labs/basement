package recipefeed

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// testRecorder stands in for the SQLite revocation store. It is insert-only
// for the same reason the real one is: there is no un-revoke path to model.
type testRecorder struct {
	entries map[string]recipe.Revocation
	fail    error
}

func newTestRecorder() *testRecorder {
	return &testRecorder{entries: map[string]recipe.Revocation{}}
}

func (r *testRecorder) RecordRevocation(_ context.Context, id string, version int, reason string, revokedAt time.Time) error {
	if r.fail != nil {
		return r.fail
	}
	key := fmt.Sprintf("%s@%d", id, version)
	if _, exists := r.entries[key]; exists {
		return nil
	}
	r.entries[key] = recipe.Revocation{ID: id, Version: version, Reason: reason, RevokedAt: revokedAt}
	return nil
}

func (r *testRecorder) revoked(id string, version int) (recipe.Revocation, bool) {
	entry, ok := r.entries[fmt.Sprintf("%s@%d", id, version)]
	return entry, ok
}

func marshalIndexWithRevocations(t *testing.T, generatedAt time.Time, revoked []recipe.Revocation, recipes ...recipe.Recipe) []byte {
	t.Helper()
	if recipes == nil {
		recipes = []recipe.Recipe{}
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

// newTestFetcher builds a Fetcher wired to the given public key without
// going through recipe.IndexPublicKey(), so tests can use their own
// ephemeral keypair instead of the placeholder embedded in the binary.
func newTestFetcher(t *testing.T, embedded []recipe.Recipe, pub ed25519.PublicKey) *Fetcher {
	t.Helper()
	f := newFetcher(embedded, t.TempDir(), discardLogger(), pub, newTestRecorder())
	return f
}

// The two durations that decide when a Spark looks and when it warns. They
// are plain constants nothing else asserts, so this is the only place a
// change to either one is caught.
func TestTheScheduledCheckIsHourly(t *testing.T) {
	if RefreshInterval != time.Hour {
		t.Fatalf("RefreshInterval=%s, want 1h: a recipe published in the morning must reach a running Spark the same morning", RefreshInterval)
	}
	// The check got faster; how old an index may be before the console says so
	// did not change with it.
	if StalenessBound != 30*24*time.Hour {
		t.Fatalf("StalenessBound=%s, want 720h", StalenessBound)
	}
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

// --- Revocation (ADR 0009 item 7) ---

func TestAcceptedRevocationIsRecordedPermanently(t *testing.T) {
	pub, priv := testKeypair(t)
	recorder := newTestRecorder()
	f := newFetcher([]recipe.Recipe{testRecipe(t, "recipe-a", 1)}, t.TempDir(), discardLogger(), pub, recorder)

	revokedAt := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	first := marshalIndexWithRevocations(t, time.Unix(1000, 0),
		[]recipe.Revocation{{ID: "recipe-a", Version: 1, Reason: "the published weights were the wrong quantisation", RevokedAt: revokedAt}},
		testRecipe(t, "recipe-a", 2))
	if err := f.accept(first, sign(priv, first), false); err != nil {
		t.Fatalf("index carrying a revocation rejected: %v", err)
	}
	entry, ok := recorder.revoked("recipe-a", 1)
	if !ok {
		t.Fatal("the revocation was not recorded")
	}
	if entry.Reason != "the published weights were the wrong quantisation" {
		t.Fatalf("reason was not carried verbatim: %q", entry.Reason)
	}

	// A later index says nothing about it. Permanence means the machine does
	// not forget: nothing above the recorder can withdraw what was accepted,
	// so a compromised key cannot quietly restore a pulled recipe.
	second := marshalIndexWithRevocations(t, time.Unix(2000, 0), nil, testRecipe(t, "recipe-a", 3))
	if err := f.accept(second, sign(priv, second), false); err != nil {
		t.Fatalf("a later index without revocations was rejected: %v", err)
	}
	if _, ok := recorder.revoked("recipe-a", 1); !ok {
		t.Fatal("a later index that omitted the entry un-revoked it")
	}
	if len(recorder.entries) != 1 {
		t.Fatalf("unexpected revocation record: %#v", recorder.entries)
	}
}

func TestAcceptRejectsAnIndexWhoseRevocationCannotBeRecorded(t *testing.T) {
	// Taking the recipes while dropping the revocation that came with them
	// would leave a revoked version installable, so the whole index is
	// refused and the registry is left as it was for the next attempt.
	pub, priv := testKeypair(t)
	recorder := newTestRecorder()
	recorder.fail = errors.New("database is unwritable")
	f := newFetcher([]recipe.Recipe{testRecipe(t, "recipe-a", 1)}, t.TempDir(), discardLogger(), pub, recorder)

	body := marshalIndexWithRevocations(t, time.Now(),
		[]recipe.Revocation{{ID: "recipe-a", Version: 1, Reason: "wrong weights", RevokedAt: time.Now()}},
		testRecipe(t, "recipe-a", 2))
	if err := f.accept(body, sign(priv, body), false); err == nil {
		t.Fatal("an index whose revocation could not be recorded was accepted")
	}
	_, effective := f.Snapshot()
	if got, ok := recipe.Find(effective, "recipe-a"); !ok || got.Version != 1 {
		t.Fatalf("the refused index changed the catalog: %#v", effective)
	}
}

func TestAcceptingARevocationStopsNothingThatIsServing(t *testing.T) {
	// The manager never stops a running model on its own. Ingesting a
	// revocation for the exact version in the effective catalog must leave
	// that catalog resolvable, unchanged, and free of any instruction to act:
	// the Fetcher holds no executor, engine or store beyond the insert-only
	// recorder, so there is nothing here that could stop a container.
	pub, priv := testKeypair(t)
	recorder := newTestRecorder()
	serving := testRecipe(t, "recipe-a", 1)
	f := newFetcher([]recipe.Recipe{serving}, t.TempDir(), discardLogger(), pub, recorder)
	beforeAll, beforeEffective := f.Snapshot()

	body := marshalIndexWithRevocations(t, time.Now(),
		[]recipe.Revocation{{ID: "recipe-a", Version: 1, Reason: "the runtime image was compromised", RevokedAt: time.Now()}})
	if err := f.accept(body, sign(priv, body), false); err != nil {
		t.Fatalf("an index that only revokes was rejected: %v", err)
	}
	afterAll, afterEffective := f.Snapshot()
	if _, ok := recipe.FindVersion(afterAll, "recipe-a", 1); !ok {
		t.Fatal("the revoked version stopped resolving, so an installed model could no longer be operated")
	}
	if len(afterAll) != len(beforeAll) {
		t.Fatalf("ingest changed the version history: before=%#v after=%#v", beforeAll, afterAll)
	}
	if fmt.Sprint(beforeEffective) != fmt.Sprint(afterEffective) {
		t.Fatalf("ingest changed the effective catalog:\nbefore=%#v\nafter=%#v", beforeEffective, afterEffective)
	}
}

// --- Feed health (ADR 0009 items 6 and 7) ---

func TestHealthReportsNeverFetchedBeforeAnyIndexIsAccepted(t *testing.T) {
	pub, _ := testKeypair(t)
	f := newTestFetcher(t, []recipe.Recipe{testRecipe(t, "recipe-a", 1)}, pub)
	health := f.Health()
	if health.State != StateNeverFetched {
		t.Fatalf("state=%q, want %q", health.State, StateNeverFetched)
	}
	if health.AcceptedGeneratedAt != nil || health.FetchedAt != nil {
		t.Fatalf("a feed that was never fetched cannot report times: %#v", health)
	}
	if health.Stale {
		t.Fatal("nothing accepted cannot be stale; the state already says it")
	}
}

func TestHealthReportsOKAfterASuccessfulFetch(t *testing.T) {
	pub, priv := testKeypair(t)
	generatedAt := time.Now().UTC().Truncate(time.Second)
	body := marshalIndex(t, generatedAt, testRecipe(t, "remote-a", 5))
	server := newIndexServer(t, body, sign(priv, body))
	f := newFetcher(nil, t.TempDir(), discardLogger(), pub, newTestRecorder())
	f.indexURL = server.URL + "/index.json"
	if err := f.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	health := f.Health()
	if health.State != StateOK {
		t.Fatalf("state=%q, want %q", health.State, StateOK)
	}
	if health.AcceptedGeneratedAt == nil || !health.AcceptedGeneratedAt.Equal(generatedAt) {
		t.Fatalf("accepted_generated_at=%v, want %s", health.AcceptedGeneratedAt, generatedAt)
	}
	if health.FetchedAt == nil {
		t.Fatal("a successful fetch must report when it happened")
	}
	if health.Stale {
		t.Fatal("an index generated just now is not stale")
	}
}

func TestHealthReportsStaleOnceTheAcceptedIndexPassesTheBound(t *testing.T) {
	// Thirty-one days: one day past the bound, so the console can say a
	// revocation may have been missed rather than imply the feed is current.
	pub, priv := testKeypair(t)
	f := newFetcher(nil, t.TempDir(), discardLogger(), pub, newTestRecorder())
	old := time.Now().Add(-31 * 24 * time.Hour)
	body := marshalIndex(t, old, testRecipe(t, "recipe-a", 1))
	if err := f.accept(body, sign(priv, body), false); err != nil {
		t.Fatalf("an old but validly signed index must still be accepted: %v", err)
	}
	health := f.Health()
	if !health.Stale {
		t.Fatalf("an index older than %s must report stale: %#v", StalenessBound, health)
	}

	fresh := marshalIndex(t, time.Now(), testRecipe(t, "recipe-a", 2))
	if err := f.accept(fresh, sign(priv, fresh), false); err != nil {
		t.Fatal(err)
	}
	if f.Health().Stale {
		t.Fatal("a fresh index must clear the staleness warning")
	}
}

func TestHealthReportsUnreachableAndKeepsTheLastAcceptedIndex(t *testing.T) {
	pub, priv := testKeypair(t)
	generatedAt := time.Now().UTC().Truncate(time.Second)
	body := marshalIndex(t, generatedAt, testRecipe(t, "remote-a", 5))

	reachable := true
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		if !reachable {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/index.json.sig", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sign(priv, body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	f := newFetcher(nil, t.TempDir(), discardLogger(), pub, newTestRecorder())
	f.indexURL = server.URL + "/index.json"
	if err := f.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fetchedAt := f.Health().FetchedAt

	reachable = false
	if err := f.RefreshOnce(context.Background()); err == nil {
		t.Fatal("expected the second fetch to fail")
	}
	health := f.Health()
	if health.State != StateUnreachable {
		t.Fatalf("state=%q, want %q", health.State, StateUnreachable)
	}
	if health.AcceptedGeneratedAt == nil || !health.AcceptedGeneratedAt.Equal(generatedAt) {
		t.Fatalf("an unreachable feed must keep reporting the last accepted index: %#v", health)
	}
	if health.FetchedAt == nil || !health.FetchedAt.Equal(*fetchedAt) {
		t.Fatalf("a failed fetch must not count as a fetch: %#v", health)
	}
	if _, effective := f.Snapshot(); len(effective) != 1 {
		t.Fatalf("an unreachable feed changed the catalog: %#v", effective)
	}

	reachable = true
	if err := f.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := f.Health().State; state != StateOK {
		t.Fatalf("state=%q after the feed came back, want %q", state, StateOK)
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
	f := newFetcher(nil, dataDir, discardLogger(), pub, newTestRecorder())
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
	offline := newFetcher(nil, dataDir, discardLogger(), pub, newTestRecorder())
	_, offlineEffective := offline.Snapshot()
	if got, ok := recipe.Find(offlineEffective, "remote-a"); !ok || got.Version != 5 {
		t.Fatalf("cached index was not recovered offline: %#v", offlineEffective)
	}
}

func TestRefreshOnceRejectsOversizedIndex(t *testing.T) {
	pub, priv := testKeypair(t)
	oversized := make([]byte, maxIndexBytes+1024)
	server := newIndexServer(t, oversized, sign(priv, oversized))

	f := newFetcher(nil, t.TempDir(), discardLogger(), pub, newTestRecorder())
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

	f := newFetcher(embedded, t.TempDir(), discardLogger(), pub, newTestRecorder())
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

// --- A forced fetch: the same attempt, on the owner's word ---

func TestFetchNowRunsTheSameAttemptAndPublishesIt(t *testing.T) {
	pub, priv := testKeypair(t)
	remote := testRecipe(t, "remote-forced", 3)
	body := marshalIndex(t, time.Now(), remote)
	server := newIndexServer(t, body, sign(priv, body))

	f := newFetcher(nil, t.TempDir(), discardLogger(), pub, newTestRecorder())
	f.indexURL = server.URL + "/index.json"

	// The publication hook is the one the scheduled cycle uses. A forced
	// fetch that updated this registry alone would leave the engine and the
	// API on the old catalog until the next cycle, hours later.
	published := 0
	var lastEffective []recipe.Recipe
	f.SetOnUpdate(func(_, effective []recipe.Recipe) {
		published++
		lastEffective = effective
	})

	health := f.FetchNow(context.Background())
	if health.State != StateOK {
		t.Fatalf("state=%q after a forced fetch, want %q", health.State, StateOK)
	}
	if health.AcceptedGeneratedAt == nil || health.FetchedAt == nil {
		t.Fatalf("a forced fetch reported no timestamps: %#v", health)
	}
	if published != 1 {
		t.Fatalf("the forced fetch published %d times, want 1", published)
	}
	if got, ok := recipe.Find(lastEffective, "remote-forced"); !ok || got.Version != 3 {
		t.Fatalf("the forced fetch did not publish the fetched catalog: %#v", lastEffective)
	}
}

func TestFetchNowOnAnUnreachableFeedReportsItRatherThanFailing(t *testing.T) {
	pub, _ := testKeypair(t)
	embedded := []recipe.Recipe{testRecipe(t, "recipe-a", 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	f := newFetcher(embedded, t.TempDir(), discardLogger(), pub, newTestRecorder())
	f.indexURL = server.URL + "/index.json"
	published := 0
	f.SetOnUpdate(func(_, _ []recipe.Recipe) { published++ })

	// Nothing has ever been accepted here, so the honest word is that this
	// machine has no index in force, not that a fetch failed.
	health := f.FetchNow(context.Background())
	if health.State != StateNeverFetched {
		t.Fatalf("state=%q after a failed forced fetch, want %q", health.State, StateNeverFetched)
	}
	if published != 1 {
		t.Fatalf("a failed forced fetch published %d times, want 1", published)
	}
	_, effective := f.Snapshot()
	if got, ok := recipe.Find(effective, "recipe-a"); !ok || got.Version != 1 {
		t.Fatalf("a failed forced fetch disturbed the embedded floor: %#v", effective)
	}
}
