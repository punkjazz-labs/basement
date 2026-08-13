// Command docredact-bench measures internal/docredact's redaction quality
// against a labeled corpus: how many gold literals still leak into the
// redacted output, how many enabled findings redacted something that was
// never gold, and how a candidate model backend changes those numbers. The
// corpus is ground truth and the leak count is the number that matters --
// every other column exists to explain it.
//
// The pattern-only arm always runs first as the baseline every model arm is
// judged against. A model arm only runs when both -base-url and -model are
// set; -model is a comma-separated list, so one invocation can score several
// candidate models against the same corpus in one run.
//
// Usage:
//
//	go run ./cmd/docredact-bench
//	go run ./cmd/docredact-bench -base-url http://127.0.0.1:4000 -model gpt-oss-20b,qwen3-14b -api-key sk-...
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/punkjazz-labs/basement/internal/docredact"
)

func main() {
	corpusDir := flag.String("corpus", "internal/docredact/testdata/corpus", "labeled corpus directory (see docredact.LoadCorpus)")
	baseURL := flag.String("base-url", "", "OpenAI-compatible base URL for a model arm; empty runs pattern-only")
	modelList := flag.String("model", "", "comma-separated model ids, one arm per model (requires -base-url)")
	apiKey := flag.String("api-key", "", "bearer token for the model backend, e.g. a LiteLLM gateway key")
	jsonPath := flag.String("json", "", "write machine-readable results to this path")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-document timeout for a model arm")
	flag.Parse()

	if err := run(*corpusDir, *baseURL, *modelList, *apiKey, *jsonPath, *timeout, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "docredact-bench:", err)
		os.Exit(1)
	}
}

// arm is one scoring pass over the whole corpus: the always-on pattern-only
// baseline (client is nil) or one candidate model.
type arm struct {
	Name   string
	client docredact.Completer
}

// armResult aggregates every corpus document's Score into totals for one
// arm, plus the model-pass bookkeeping (chunks failed, documents that timed
// out, wall time) Score itself does not carry.
type armResult struct {
	Name         string                     `json:"name"`
	Docs         int                        `json:"docs"`
	DocsFailed   int                        `json:"docs_failed"` // timed out or ctx-cancelled; not scored
	Gold         int                        `json:"gold"`
	Leaked       int                        `json:"leaked"`
	LeakedByCat  map[docredact.Category]int `json:"leaked_by_category"`
	GoldByCat    map[docredact.Category]int `json:"gold_by_category"`
	OverRedacted int                        `json:"over_redacted"`
	Hallucinated int                        `json:"hallucinated"`
	ChunksFailed int                        `json:"chunks_failed"`
	// WallTime only sums elapsed time for the Docs that were actually
	// scored: a doc that hit DocsFailed contributes nothing here, so
	// WallTime/Docs is never inflated by the timeouts it is trying to
	// report on.
	WallTime time.Duration `json:"wall_time_ns"`
}

func run(corpusDir, baseURL, modelList, apiKey, jsonPath string, timeout time.Duration, out io.Writer) error {
	if modelList != "" && baseURL == "" {
		return fmt.Errorf("-model requires -base-url")
	}

	corpus, err := docredact.LoadCorpus(corpusDir)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}

	arms := []arm{{Name: "pattern-only"}}
	for _, name := range strings.Split(modelList, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		arms = append(arms, arm{
			Name: name,
			client: &docredact.ModelClient{
				BaseURL: baseURL,
				Model:   name,
				APIKey:  apiKey,
				HTTP:    &http.Client{Timeout: timeout},
			},
		})
	}

	results := make([]armResult, 0, len(arms))
	for _, a := range arms {
		results = append(results, scoreArm(a, corpus, timeout))
	}

	printTable(out, results)

	if jsonPath != "" {
		encoded, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return fmt.Errorf("encode json results: %w", err)
		}
		if err := os.WriteFile(jsonPath, encoded, 0o644); err != nil {
			return fmt.Errorf("write json results to %s: %w", jsonPath, err)
		}
	}
	return nil
}

// scoreArm runs one arm over every corpus document -- pattern detection
// always, a model pass only when a is a model arm -- and aggregates each
// document's Score. A document whose model pass fails outright (ctx
// cancellation from -timeout, the only error ApplyModelPass returns) counts
// as DocsFailed and is left out of every other total rather than scored on a
// half-finished pass.
func scoreArm(a arm, corpus []docredact.CorpusDoc, timeout time.Duration) armResult {
	result := armResult{
		Name:        a.Name,
		LeakedByCat: make(map[docredact.Category]int),
		GoldByCat:   make(map[docredact.Category]int),
	}

	for _, cd := range corpus {
		start := time.Now()
		doc := docredact.Analyze(cd.Text)

		var pass docredact.ModelPassResult
		if a.client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			p, err := doc.ApplyModelPass(ctx, a.client)
			cancel()
			if err != nil {
				// Excluded from WallTime too, not just from the score
				// totals: a doc that never finished must not drag the
				// average time of the docs that did finish upward.
				result.DocsFailed++
				continue
			}
			pass = p
		}

		score := docredact.ScoreDocument(doc, cd.Gold, pass)
		result.WallTime += time.Since(start)
		result.Docs++
		result.ChunksFailed += pass.ChunksFailed
		result.Gold += score.Gold
		result.Leaked += score.Leaked
		result.OverRedacted += score.OverRedacted
		result.Hallucinated += score.Hallucinated
		for cat, n := range score.LeakedByCat {
			result.LeakedByCat[cat] += n
		}
		for cat, n := range score.GoldByCat {
			result.GoldByCat[cat] += n
		}
	}
	return result
}

// printTable renders one summary row per arm plus a per-arm leak-by-category
// breakdown, plain aligned text via text/tabwriter -- no color, no emoji, and
// every number is a direct count or a rate over one, never an estimate.
func printTable(out io.Writer, results []armResult) {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ARM\tGOLD\tLEAKED\tLEAK%\tOVER-REDACTED\tHALLUCINATED\tCHUNKS FAILED\tDOCS FAILED\tAVG TIME/SCORED DOC")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\t%d\t%d\t%d\t%s\n",
			r.Name, r.Gold, r.Leaked, percent(r.Leaked, r.Gold),
			r.OverRedacted, r.Hallucinated, r.ChunksFailed, r.DocsFailed,
			avgDuration(r.WallTime, r.Docs))
	}
	w.Flush()

	for _, r := range results {
		fmt.Fprintf(out, "\n%s -- leaks by category\n", r.Name)
		cw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(cw, "CATEGORY\tGOLD\tLEAKED\tLEAK%")
		for _, cat := range sortedCategories(r.GoldByCat) {
			gold := r.GoldByCat[cat]
			fmt.Fprintf(cw, "%s\t%d\t%d\t%s\n", cat, gold, r.LeakedByCat[cat], percent(r.LeakedByCat[cat], gold))
		}
		cw.Flush()
	}
}

// sortedCategories returns m's keys in a fixed, deterministic order so the
// per-category table prints the same row order on every run.
func sortedCategories(m map[docredact.Category]int) []docredact.Category {
	cats := make([]docredact.Category, 0, len(m))
	for c := range m {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i] < cats[j] })
	return cats
}

// percent formats n/total as a percentage, or "n/a" when total is 0 rather
// than dividing by zero or printing a misleading 0.0%.
func percent(n, total int) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(total))
}

// avgDuration formats total/docs rounded to the millisecond, or "n/a" when
// no document was scored. total must already exclude any doc that failed --
// this only ever averages over the docs it was actually timed for.
func avgDuration(total time.Duration, docs int) string {
	if docs == 0 {
		return "n/a"
	}
	return (total / time.Duration(docs)).Round(time.Millisecond).String()
}
