// Package recipefeed fetches, verifies, and caches the signed remote recipe
// index (spec 04). It owns the only accumulating record of every recipe
// version this manager has ever verified — embedded, cached on disk, and
// freshly fetched — because engine and httpapi need to keep resolving
// already-installed models against the exact version they were installed
// with, even after the catalog moves on to a newer one.
package recipefeed

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

// IndexURL is the single HTTPS location the manager fetches the signed
// recipe index from. It is a placeholder: punkjazz-labs/runonspark-recipes
// does not exist yet — see the spec 04 executor report. Once a real index is
// published, this is the only line that needs to change.
const IndexURL = "https://raw.githubusercontent.com/punkjazz-labs/runonspark-recipes/main/index.json"

// signatureSuffix turns the index URL into its detached signature's URL.
const signatureSuffix = ".minisig"

const (
	// maxIndexBytes and maxSignatureBytes cap the untrusted response bodies
	// before they are read into memory, regardless of what Content-Length
	// claims. The real index is expected to stay in the tens of kilobytes
	// even with hundreds of recipes; this leaves generous headroom while
	// still bounding a malicious or broken response.
	maxIndexBytes     = 8 << 20 // 8 MiB
	maxSignatureBytes = 4 << 10 // 4 KiB (real content is ~88 bytes)

	fetchTimeout    = 15 * time.Second
	RefreshInterval = 6 * time.Hour
)

// newHTTPClient builds a client dedicated to index fetches: it never follows
// redirects. The index URL is a fixed constant, so a redirect to anywhere —
// same host or not — is outside the trusted path and is treated as a fetch
// failure rather than followed.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not followed when fetching the recipe index")
		},
	}
}

// Fetcher owns the recipe registry after startup: the embedded set plus
// everything ever verified from disk cache or the network, and the version
// rule (recipe.Merge) that turns those into one effective catalog. It is
// also the single accumulating source of truth recipe.FindVersion needs, so
// an already-installed model keeps resolving to the exact recipe version it
// was installed with even after the catalog moves on.
type Fetcher struct {
	cacheDir   string
	indexURL   string // always IndexURL in production; overridden by tests only
	publicKey  ed25519.PublicKey
	logger     *slog.Logger
	httpClient *http.Client

	mu           sync.Mutex
	embedded     []recipe.Recipe
	cached       []recipe.Recipe
	fresh        []recipe.Recipe
	all          map[string]recipe.Recipe // keyed by id+"@"+version; only ever grows
	lastAccepted time.Time
}

// NewFetcher seeds the registry with the embedded recipes and whatever
// verified cache already exists on disk (a previous run's last accepted
// index). It never touches the network — startup must work offline, and
// must not block on it — so RefreshOnce/Run own every network call.
func NewFetcher(embedded []recipe.Recipe, dataDir string, logger *slog.Logger) *Fetcher {
	return newFetcher(embedded, dataDir, logger, recipe.IndexPublicKey())
}

// newFetcher is NewFetcher with the public key made explicit, so tests can
// verify against an ephemeral test key from the very first (cache-loading)
// moment of construction, exactly as production verifies against the one
// real embedded key for the object's entire lifetime — the key is never
// swapped out after construction.
func newFetcher(embedded []recipe.Recipe, dataDir string, logger *slog.Logger, publicKey ed25519.PublicKey) *Fetcher {
	if logger == nil {
		logger = slog.Default()
	}
	f := &Fetcher{
		cacheDir:   filepath.Join(dataDir, "recipes-cache"),
		indexURL:   IndexURL,
		publicKey:  publicKey,
		logger:     logger,
		httpClient: newHTTPClient(),
		embedded:   embedded,
		all:        make(map[string]recipe.Recipe, len(embedded)),
	}
	for _, r := range embedded {
		f.all[versionKey(r.ID, r.Version)] = r
	}
	f.loadCache()
	return f
}

// loadCache re-verifies whatever the last successful fetch wrote to disk.
// Re-verifying on load (rather than trusting the files as already-checked)
// means a cache directory tampered with outside the manager — a different
// process, a restored backup, a bug — is caught the same way a bad network
// response would be: rejected, logged, and simply treated as absent. That
// keeps the embedded set as the floor in every failure mode, not just
// network ones.
func (f *Fetcher) loadCache() {
	indexPath := filepath.Join(f.cacheDir, "index.json")
	sigPath := filepath.Join(f.cacheDir, "index.json"+signatureSuffix)
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		if !os.IsNotExist(err) {
			f.logger.Warn("recipe index: could not read cached index; ignoring cache", "error", err)
		}
		return
	}
	sigBytes, err := os.ReadFile(sigPath)
	if err != nil {
		f.logger.Warn("recipe index: cached index has no matching signature file; ignoring cache", "error", err)
		return
	}
	if err := f.accept(indexBytes, sigBytes, false); err != nil {
		f.logger.Warn("recipe index: cached index failed verification; ignoring cache", "error", err)
	}
}

// Snapshot returns the current accumulated history and the current
// effective (merged) catalog. Safe for concurrent use; the returned slices
// are not mutated after being returned.
func (f *Fetcher) Snapshot() (all, effective []recipe.Recipe) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all = make([]recipe.Recipe, 0, len(f.all))
	for _, r := range f.all {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ID != all[j].ID {
			return all[i].ID < all[j].ID
		}
		return all[i].Version < all[j].Version
	})
	effective = recipe.Merge(f.embedded, f.cached, f.fresh)
	return all, effective
}

// RefreshOnce performs one fetch-verify-accept cycle against the network.
// Any failure — network, signature, downgrade — is returned for the caller
// to log; the registry is left exactly as it was, so the effective catalog
// never regresses because of a bad fetch.
func (f *Fetcher) RefreshOnce(ctx context.Context) error {
	indexBytes, err := f.fetchCapped(ctx, f.indexURL, maxIndexBytes)
	if err != nil {
		return fmt.Errorf("fetch recipe index: %w", err)
	}
	sigBytes, err := f.fetchCapped(ctx, f.indexURL+signatureSuffix, maxSignatureBytes)
	if err != nil {
		return fmt.Errorf("fetch recipe index signature: %w", err)
	}
	return f.accept(indexBytes, sigBytes, true)
}

// Run fetches immediately, then every interval, calling onUpdate after each
// attempt (successful or not) with the resulting snapshot — which on
// failure is simply unchanged from before. It returns when ctx is done.
// Failures never reach the user: only the log line here states why.
func (f *Fetcher) Run(ctx context.Context, interval time.Duration, onUpdate func(all, effective []recipe.Recipe)) {
	attempt := func() {
		if err := f.RefreshOnce(ctx); err != nil {
			f.logger.Warn("recipe index: refresh failed; keeping the current recipe set", "error", err)
		}
		onUpdate(f.Snapshot())
	}
	attempt()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt()
		}
	}
}

// accept runs the full trust chain (recipe.VerifyAndParseIndex), enforces
// downgrade protection against the last accepted GeneratedAt, and — only on
// genuine acceptance — updates the in-memory registry and, when persist is
// true, the on-disk cache. It is the single place either the network path
// (RefreshOnce) or the disk-cache path (loadCache) commits a new index, so
// the two can never disagree about what counts as newer.
func (f *Fetcher) accept(indexBytes, sigBytes []byte, persist bool) error {
	idx, reasons, err := recipe.VerifyAndParseIndex(indexBytes, sigBytes, f.publicKey)
	if err != nil {
		return err
	}
	for _, reason := range reasons {
		f.logger.Warn("recipe index: dropped an invalid recipe", "reason", reason)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.lastAccepted.IsZero() && !idx.GeneratedAt.After(f.lastAccepted) {
		if idx.GeneratedAt.Before(f.lastAccepted) {
			return fmt.Errorf("index generated_at %s is older than the last accepted index (%s); rejecting a possible downgrade or replay",
				idx.GeneratedAt.Format(time.RFC3339), f.lastAccepted.Format(time.RFC3339))
		}
		// Equal timestamp: the same index resent verbatim. Not an error —
		// an unattended refresh that finds nothing new must stay silent,
		// not log a failure every cycle.
		return nil
	}
	f.cached = idx.Recipes
	f.fresh = idx.Recipes
	f.lastAccepted = idx.GeneratedAt
	for _, r := range idx.Recipes {
		f.all[versionKey(r.ID, r.Version)] = r
	}
	if persist {
		if err := f.writeCache(indexBytes, sigBytes); err != nil {
			// The verified index is already live in memory; a cache write
			// failure only costs the next restart a redundant fetch, so it
			// is logged, not treated as a refresh failure.
			f.logger.Warn("recipe index: failed to persist verified cache to disk", "error", err)
		}
	}
	return nil
}

// writeCache stores the exact verified bytes (index and signature) so a
// restart can re-verify and reuse them without the network, and so an
// operator can audit exactly what was accepted. Both files are written to a
// temporary name in the same directory and renamed into place, so a crash
// mid-write can never leave a truncated index paired with the previous
// (mismatched) signature or vice versa.
func (f *Fetcher) writeCache(indexBytes, sigBytes []byte) error {
	if err := os.MkdirAll(f.cacheDir, 0o750); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(f.cacheDir, "index.json"), indexBytes); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(f.cacheDir, "index.json"+signatureSuffix), sigBytes)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fetchCapped reads at most capBytes+1 from url, regardless of
// Content-Length, and fails if the body turns out to exceed the cap — an
// oversized response is rejected outright rather than silently truncated
// and handed to the signature verifier as if it were the whole message.
func (f *Fetcher) fetchCapped(ctx context.Context, url string, capBytes int64) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, capBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > capBytes {
		return nil, fmt.Errorf("response exceeds the %d byte limit", capBytes)
	}
	return body, nil
}

func versionKey(id string, version int) string { return fmt.Sprintf("%s@%d", id, version) }
