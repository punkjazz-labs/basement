// Command feed-watch is the zero-touch half of the recipe feed (see
// docs/RECIPE-FEED.md section 4): it notices when an upstream recipe source
// moves and turns the safe part of that move into a published version bump,
// so a user who already installed a recipe gets the fix without anyone doing
// per-update manual work.
//
// Two modes:
//
//	feed-watch -mode check   # report drift only; never writes a file
//	feed-watch -mode bump    # apply the safe subset of drift, then report the rest
//
// check compares every embedded recipe's pinned artifact revision, and (for a
// GitHub source) its pinned source revision, against what upstream reports
// today. It writes a JSON report plus a one-line-per-drift summary to stdout,
// and exits 0 when nothing has moved, 3 when something has, and 4 when every
// recipe it looked at failed to answer at all.
//
// bump runs the same scan, then rewrites a recipe's own YAML file for exactly
// two safe cases: a whole-snapshot artifact whose licence and LICENSE file
// still check out at the new revision, and a per-file-pinned artifact whose
// pinned files are unchanged in name and size at the new revision. Everything
// else, a source move, a changed licence, a pinned file that changed size or
// vanished, is left for a maintainer to read in the report rather than
// guessed at. Edits are surgical string replacements on the raw file, so a
// recipe's comments survive byte for byte outside the lines that changed; a
// result that fails recipe.Validate is discarded rather than written.
//
// feed-watch never runs git. packaging/publish-feed.sh owns commit, sign and
// publish.
//
// Usage (run from the repository root, so -recipes-dir resolves):
//
//	go run ./cmd/feed-watch -mode check -out feed-watch-report.json
//	go run ./cmd/feed-watch -mode bump -out feed-watch-report.json
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

const (
	defaultHFAPIBase     = "https://huggingface.co"
	defaultGitHubAPIBase = "https://api.github.com"
	// userAgent identifies this tool to both upstream APIs, so a maintainer
	// reading their own access logs can tell a feed-watch run from a browser.
	userAgent = "basement-feed-watch"
)

func main() {
	mode := flag.String("mode", "check", "scan mode: check (report only) or bump (apply the safe subset)")
	out := flag.String("out", "feed-watch-report.json", "path to write the JSON drift report to")
	recipesDir := flag.String("recipes-dir", "internal/recipe/recipes", "directory of recipe YAML files this tool may rewrite in bump mode")
	// hfAPIBase and githubAPIBase exist so tests can point this tool at an
	// httptest server instead of the real APIs; production runs leave them
	// unset and get the real hosts, by way of BASEMENT_HF_API /
	// BASEMENT_GITHUB_API when set, else the compiled-in defaults.
	hfAPIBase := flag.String("hf-api-base", "", "override the Hugging Face API base URL (tests only)")
	githubAPIBase := flag.String("github-api-base", "", "override the GitHub API base URL (tests only)")
	flag.Parse()

	if *mode != "check" && *mode != "bump" {
		fmt.Fprintln(os.Stderr, "feed-watch: -mode must be check or bump")
		os.Exit(2)
	}

	recipes, err := recipe.Builtin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "feed-watch:", err)
		os.Exit(1)
	}

	code, err := run(
		recipes, *mode, *out, *recipesDir,
		resolveBase(*hfAPIBase, "BASEMENT_HF_API", defaultHFAPIBase),
		resolveBase(*githubAPIBase, "BASEMENT_GITHUB_API", defaultGitHubAPIBase),
		os.Stdout,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "feed-watch:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// resolveBase picks the API base a client scans against: an explicit flag
// wins, then an environment override for tests that exec this binary rather
// than call its functions, then the real host.
func resolveBase(flagValue, envName, fallback string) string {
	if flagValue != "" {
		return flagValue
	}
	if value := os.Getenv(envName); value != "" {
		return value
	}
	return fallback
}

// run performs one full scan-and-act cycle over recipes and returns the exit
// code the caller should use. recipes is a parameter, not a call to
// recipe.Builtin() here, so a test can hand it two or three synthetic
// recipes instead of driving the whole embedded pack through a fake server.
func run(recipes []recipe.Recipe, mode, outPath, recipesDir, hfBase, githubBase string, stdout io.Writer) (int, error) {
	hf := newHFClient(hfBase)
	gh := newGitHubClient(githubBase)

	scans := make([]recipeScan, 0, len(recipes))
	for _, r := range recipes {
		scans = append(scans, scanRecipe(r, hf, gh))
	}

	var findings []Finding
	var bumped []BumpResult
	var err error
	if mode == "bump" {
		findings, bumped, err = applyBumps(scans, recipesDir, hf)
		if err != nil {
			return 0, err
		}
	} else {
		findings = buildCheckFindings(scans)
	}

	report := Report{
		GeneratedAt: time.Now().UTC(),
		Mode:        mode,
		Findings:    findings,
		Bumped:      bumped,
	}
	if err := writeReport(outPath, report); err != nil {
		return 0, err
	}
	printSummary(stdout, mode, findings, bumped)
	return exitCode(findings), nil
}
