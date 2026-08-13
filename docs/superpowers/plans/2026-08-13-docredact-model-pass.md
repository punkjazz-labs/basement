# Doc Redactor Model Pass + Offline Benchmark Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build spec 12 step 3 (the model pass) for `internal/docredact`, expose it as a console API endpoint, and add a labeled corpus plus a qualification harness — all runnable and testable without Spark hardware.

**Owner decision, 2026-08-13:** the redaction model will be a *dedicated, pinned* model, not "whatever is serving" — controlled, repeatable quality per machine. The engine below is model-agnostic by construction (a `Completer` seam), the endpoint resolves a dedicated redactor model first and falls back honestly, and the corpus/harness exists to *choose and qualify* that dedicated model with measured numbers, not to debate the direction.

**Architecture:** The engine stays pure: `internal/docredact` gains a `Completer` interface (one method, returns a string reply), chunking, lenient reply parsing, and `Document.ApplyModelPass` which verifies every model-claimed literal by exact string search, counts hallucinated ones, and folds survivors into the existing recompute/overlap machinery as a third source `"model"`. A concrete `ModelClient` speaks OpenAI `/v1/chat/completions` against any base URL (Spark runtime, LiteLLM gateway, httptest). `internal/httpapi` adds one endpoint that runs the pass against the serving model. The benchmark is a corpus of labeled synthetic docs in `testdata/` plus `cmd/docredact-bench` with outcome-based scoring (a gold literal that survives into the redacted output is a leak).

**Tech Stack:** Go standard library only (net/http, encoding/json, strings, sort, flag). No new dependencies. Tests use `httptest`.

**Spec:** `docs/plans/12-doc-redactor.md` (step 3, "Pass 2, the model", test strategy section). Choices already binding: ADR 0021 (`docs/decisions/0021-doc-redactor-engine-choices.md`).

## Global Constraints

- Branch: `spec/12-model-pass` off `main`. Never push to main; merge needs the owner's in-session grant (`docs/PROJECT-AUTONOMY.md`: merge_main ask-per-session).
- Commit style: `manual: <plain lowercase sentence>` (matches current repo practice, satisfies the commit-msg hook).
- Before every commit: `go build ./... && go vet ./... && go test ./...` from repo root. No webui changes in this plan, so no UI build needed.
- Standard library first; no DI framework; API errors via existing `writeError(w, code, err)`; 409 for conflicts with a sentence the UI can show verbatim.
- Error strings lowercase, actionable. Comments state constraints code can't show; never narrate the change process.
- **No invented facts** anywhere user-visible. The benchmark results doc ships with `n/a` values until a real run fills them.
- Never touch protected paths (`.github/`, `scripts/audit/`, `tests/`, `package.json`, etc. — see PROJECT-AUTONOMY.md).
- Do not touch `internal/redact` (unrelated package). Do not modify existing detector behavior.
- Offsets from a model are never trusted: the model returns exact literal substrings; every occurrence is located locally with exact, case-sensitive string search. A literal not present in the document is dropped and counted as hallucinated.

## Existing interfaces the plan builds on (read them first)

- `docredact.Match{Start, End int; Text string; Category Category; Source string}`
- `docredact.ResolveOverlaps([]Match) []Match` — longest wins; ties break by original slice order (stable sort). So appending model matches AFTER detector and manual matches in `recompute` makes pattern/manual beat model on identical spans automatically.
- `Document{Text string; Findings []*Finding; spans []Match; byLiteral map[string]*Finding; manual []manualLiteral; counters map[Category]int}` with `recompute()` as the single place findings are built; surviving findings keep pointer/pseudonym/enabled state.
- `manualMatches(text, literal string, category Category) []Match` — exact non-overlapping occurrences left to right; reuse for model literals (with a different Source — see Task 4).
- `httpapi.docredactSessionAction` dispatches `/api/v1/docredact/sessions/{id}/...` by manual path parsing; sessions hold `mu sync.Mutex` + `doc *docredact.Document`.
- `httpapi.Server.proxyModel` + `s.inferenceTarget(w, r)` + `activeReadyRecipe(ctx)` — how a serving text model is resolved and dialed (`127.0.0.1:<recipe.Service.DefaultHostPort>`); study `roles.go` for role-name resolution and the serving-gate hold.

---

### Task 1: Model categories and source

**Files:**
- Modify: `internal/docredact/docredact.go` (categories, prefixes, ParseCategory, add `SourceModel`)
- Test: `internal/docredact/registry_test.go` (extend) or new `internal/docredact/category_test.go`

**Interfaces:**
- Produces: `const SourceModel = "model"`; `CategoryPerson Category = "person"` (prefix `PERSON`), `CategoryOrg Category = "org"` (`ORG`), `CategoryAddress Category = "address"` (`ADDRESS`), `CategoryJobTitle Category = "job_title"` (`TITLE`), `CategoryAmount Category = "amount"` (`AMOUNT`). `ParseCategory` accepts all five.
- These are model/manual categories only: **no Detector exists for them** (like `CategoryPhrase`). `Registry()` is unchanged.

- [ ] **Step 1: Write the failing test**

```go
func TestModelCategories(t *testing.T) {
	cases := []struct {
		name   string
		want   Category
		prefix string
	}{
		{"person", CategoryPerson, "PERSON"},
		{"org", CategoryOrg, "ORG"},
		{"address", CategoryAddress, "ADDRESS"},
		{"job_title", CategoryJobTitle, "TITLE"},
		{"amount", CategoryAmount, "AMOUNT"},
	}
	for _, c := range cases {
		got, known := ParseCategory(c.name)
		if !known || got != c.want {
			t.Fatalf("ParseCategory(%q) = %v, %v", c.name, got, known)
		}
		if c.want.Prefix() != c.prefix {
			t.Fatalf("Prefix(%v) = %q, want %q", c.want, c.want.Prefix(), c.prefix)
		}
	}
	if SourceModel != "model" {
		t.Fatalf("SourceModel = %q", SourceModel)
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/docredact/ -run TestModelCategories` → FAIL (undefined identifiers).
- [ ] **Step 3: Implement** — add the constants next to `SourceManual`, the five categories next to `CategoryPhrase` with a comment noting they are model-pass categories with no Detector, extend `Prefix()` and `ParseCategory`'s switch.
- [ ] **Step 4: Run** `go test ./internal/docredact/` → PASS.
- [ ] **Step 5: Commit** — `manual: the redactor knows the model pass categories`

### Task 2: Chunking

**Files:**
- Create: `internal/docredact/chunk.go`
- Test: `internal/docredact/chunk_test.go`

**Interfaces:**
- Produces: `func ChunkText(text string, size, overlap int) []string` and package constants `ModelChunkSize = 6000`, `ModelChunkOverlap = 400` (bytes; spec: "overlap of a few hundred characters"). Guarantees: every byte of text appears in at least one chunk; consecutive chunks share `overlap` bytes; chunk boundaries snap back to the nearest newline or space within the last 200 bytes when one exists (so literals aren't split mid-word more than necessary — the overlap is the real safety net); a text shorter than `size` returns `[]string{text}`; `size <= overlap` panics (programmer error).

- [ ] **Step 1: Failing tests** — cover: short text returns one identical chunk; a 15000-byte synthetic text chunks so that concatenating with overlap removed reconstructs the original; every consecutive pair shares exactly the overlap region (or more after boundary snapping); a literal placed to straddle a naive boundary appears whole in at least one chunk.

```go
func TestChunkTextShortTextIsOneChunk(t *testing.T) {
	got := ChunkText("hello", 100, 10)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got %#v", got)
	}
}

func TestChunkTextCoversEveryByteAndOverlaps(t *testing.T) {
	text := strings.Repeat("lorem ipsum dolor sit amet ", 600) // ~16k
	chunks := ChunkText(text, 6000, 400)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	// every chunk is a substring at increasing positions, and the whole text is covered
	pos := 0
	covered := 0
	for _, c := range chunks {
		i := strings.Index(text[pos:], c)
		if i < 0 {
			t.Fatal("chunk is not a substring in order")
		}
		start := pos + i
		if start > covered {
			t.Fatalf("gap: chunk starts at %d but only %d covered", start, covered)
		}
		if start+len(c) > covered {
			covered = start + len(c)
		}
		pos = start
	}
	if covered != len(text) {
		t.Fatalf("covered %d of %d bytes", covered, len(text))
	}
}

func TestChunkTextStraddlingLiteralSurvives(t *testing.T) {
	pad := strings.Repeat("x ", 2995)
	literal := "mario.rossi@example.com"
	text := pad + literal + strings.Repeat(" y", 2000)
	found := false
	for _, c := range ChunkText(text, 6000, 400) {
		if strings.Contains(c, literal) {
			found = true
		}
	}
	if !found {
		t.Fatal("literal split across every chunk")
	}
}
```

- [ ] **Step 2: Run to fail.** `go test ./internal/docredact/ -run TestChunkText`
- [ ] **Step 3: Implement** — simple loop: window `[start, start+size)`, snap end back to last `\n` or space in the final 200 bytes if any (never below `start+size-200`), append, next `start = end - overlap`. Final chunk runs to `len(text)`.
- [ ] **Step 4: Run to pass.**
- [ ] **Step 5: Commit** — `manual: long documents chunk with overlap for the model pass`

### Task 3: Lenient model-reply parsing

**Files:**
- Create: `internal/docredact/modelparse.go`
- Test: `internal/docredact/modelparse_test.go`

**Interfaces:**
- Produces: `type ModelItem struct { Literal string \`json:"literal"\`; Category string \`json:"category"\` }` and `func ExtractModelItems(reply string) ([]ModelItem, error)`.
- Behavior: first try `json.Unmarshal` of the whole trimmed reply as `[]ModelItem`. Failing that, scan for the first `[` and walk to its matching `]` (tracking string/escape state so brackets inside JSON strings don't fool it) and unmarshal that slice. Also accepts an object wrapper `{"items":[...]}` (some structured-output modes wrap). Returns an error only when no parseable array exists. Items with empty/whitespace literals are dropped here. Categories are NOT validated here (that's ApplyModelPass's job via ParseCategory).

- [ ] **Step 1: Failing tests** — table-driven:

```go
func TestExtractModelItems(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  []ModelItem
		ok    bool
	}{
		{"clean array", `[{"literal":"Mario Rossi","category":"person"}]`,
			[]ModelItem{{"Mario Rossi", "person"}}, true},
		{"prose around it", "Sure! Here you go:\n```json\n[{\"literal\":\"Acme S.p.A.\",\"category\":\"org\"}]\n```\nLet me know.",
			[]ModelItem{{"Acme S.p.A.", "org"}}, true},
		{"object wrapper", `{"items":[{"literal":"Via Roma 1, Milano","category":"address"}]}`,
			[]ModelItem{{"Via Roma 1, Milano", "address"}}, true},
		{"bracket inside string", `[{"literal":"pay [in full] to Mario","category":"amount"}]`,
			[]ModelItem{{"pay [in full] to Mario", "amount"}}, true},
		{"empty literal dropped", `[{"literal":"  ","category":"person"},{"literal":"Anna","category":"person"}]`,
			[]ModelItem{{"Anna", "person"}}, true},
		{"empty array is fine", `[]`, nil, true},
		{"garbage", `the document contains no sensitive data`, nil, false},
	}
	for _, c := range cases {
		got, err := ExtractModelItems(c.reply)
		if c.ok != (err == nil) {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %#v want %#v", c.name, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to fail.**
- [ ] **Step 3: Implement.** Keep the bracket walker small: state machine over bytes with `inString`, `escaped`, `depth`.
- [ ] **Step 4: Run to pass.**
- [ ] **Step 5: Commit** — `manual: the model's answer parses leniently or not at all`

### Task 4: Document integration — ApplyModelPass

**Files:**
- Create: `internal/docredact/modelpass.go`
- Modify: `internal/docredact/document.go` (add `model []manualLiteral` field, fold into `recompute`)
- Test: `internal/docredact/modelpass_test.go`

**Interfaces:**
- Consumes: `ChunkText`, `ExtractModelItems`, `ParseCategory`, `manualMatches`, `recompute`.
- Produces:

```go
// Completer is one round to a model: system+user prompt in, raw text reply out.
type Completer interface {
	Complete(ctx context.Context, system, user string, structured bool) (string, error)
}

// ErrStructuredUnsupported is returned by a Completer whose backend rejected
// response_format structured output; the pass retries the same chunk unstructured.
var ErrStructuredUnsupported = errors.New("structured output unsupported")

type ModelPassResult struct {
	Accepted     int    `json:"accepted"`     // model literals that produced findings
	Duplicates   int    `json:"duplicates"`   // literals another pass already covers
	Hallucinated int    `json:"hallucinated"` // literals not present in the document
	ChunksTotal  int    `json:"chunks_total"`
	ChunksFailed int    `json:"chunks_failed"` // unusable after repair retry or transport error
	Degraded     bool   `json:"degraded"`      // no chunk was usable: pattern-only stands
}

func (d *Document) ApplyModelPass(ctx context.Context, c Completer) (ModelPassResult, error)
```

- Algorithm (this is the spec's literal-only contract):
  1. `chunks := ChunkText(d.Text, ModelChunkSize, ModelChunkOverlap)`.
  2. Per chunk: call `c.Complete(ctx, modelPassSystemPrompt, chunk, structured)` — `structured` starts true; on `ErrStructuredUnsupported` retry once with `structured=false` and stay unstructured for all later chunks. Any other error counts the chunk failed and continues (a mid-pass transport error must not throw away usable chunks).
  3. Parse with `ExtractModelItems`. On parse error, one repair retry: `c.Complete(ctx, modelPassSystemPrompt, "Your previous reply was not a JSON array. Reply with only the JSON array, nothing else.\n\nPrevious reply:\n"+reply, false)`; parse again; still bad → chunk failed.
  4. Collect items across chunks; dedup by exact literal (`strings.TrimSpace`d, case-sensitive; empty after trim → skip).
  5. For each unique literal: not in `d.Text` (`strings.Contains`) → `Hallucinated++`. Already a finding or already a manual/model literal → `Duplicates++`. Otherwise append to `d.model` with `ParseCategory(item.Category)` (unknown names become phrase — the honest default, same rule as manual).
  6. One `recompute()` at the end. `Accepted` = count of findings whose `Source == SourceModel` present after recompute (a model literal can still lose the overlap contest to a longer existing finding — that is neither accepted nor hallucinated; count it in `Duplicates`).
  7. `Degraded = ChunksFailed == ChunksTotal`. Error return is reserved for `ctx` cancellation; model misbehavior is data (`Degraded`), not an error.
- `recompute` change: after the manual loop, `for _, m := range d.model { matches = append(matches, modelMatches(d.Text, m.literal, m.category)...) }` where `modelMatches` is `manualMatches` with `Source: SourceModel` (factor a shared `literalMatches(text, literal, category, source)`; keep `manualMatches` as a one-line wrapper so nothing else changes). Appending model matches LAST is what makes pattern and manual beat model on same-length ties in `ResolveOverlaps`.
- The system prompt (const `modelPassSystemPrompt`): instructs — find text identifying a person in context: names, organisations, addresses, job titles, amounts tied to a person; reply with ONLY a JSON array of `{"literal": "<exact substring copied verbatim>", "category": "person|org|address|job_title|amount"}`; copy characters exactly as written; do not invent text that is not in the document; empty array if nothing found.

- [ ] **Step 1: Failing tests** — use a scripted fake Completer:

```go
type fakeCompleter struct {
	replies []string // popped per call; "STRUCTURED-UNSUPPORTED" and "ERROR" are sentinels
	calls   []string // records user prompts for assertions
}

func (f *fakeCompleter) Complete(_ context.Context, _, user string, structured bool) (string, error) {
	f.calls = append(f.calls, user)
	if len(f.replies) == 0 {
		return "", errors.New("no scripted reply")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	switch r {
	case "STRUCTURED-UNSUPPORTED":
		return "", ErrStructuredUnsupported
	case "ERROR":
		return "", errors.New("connection refused")
	}
	return r, nil
}
```

Tests: (a) happy path — doc with "Mario Rossi" twice and an email; model returns Mario Rossi as person; result Accepted=1; finding has Source "model", Token "[PERSON_1]", Occurrences 2; email finding still Source "pattern". (b) hallucinated literal counted and absent from findings. (c) literal equal to an existing pattern finding → Duplicates=1, finding keeps Source "pattern". (d) parse garbage then repaired JSON on retry → accepted; assert the second call's user prompt contains the previous reply. (e) garbage twice → ChunksFailed=1, Degraded=true (single chunk), findings unchanged. (f) ErrStructuredUnsupported then valid reply → accepted, and a second chunk's call goes straight to unstructured (assert via a two-chunk doc: make text > ModelChunkSize). (g) model literal containing a pattern literal ("Sig. Mario Rossi <mario@example.com>" scenario: model returns a phrase containing the email) → longer model literal wins the email's span, email finding disappears, exactly like manual. (h) unknown category name → phrase finding. (i) ctx cancelled → error returned, document untouched.

- [ ] **Step 2: Run to fail.**
- [ ] **Step 3: Implement** (`modelpass.go` + the small `document.go`/`manual.go` refactor).
- [ ] **Step 4: Run full package tests** — existing manual/overlap/mapping tests must still pass untouched.
- [ ] **Step 5: Commit** — `manual: the model pass claims literals and the document verifies every one`

### Task 5: ModelClient — OpenAI chat completions over any base URL

**Files:**
- Create: `internal/docredact/modelclient.go`
- Test: `internal/docredact/modelclient_test.go` (httptest — this is the spec's own test strategy: "an httptest server standing in for /v1, returning canned completions")

**Interfaces:**
- Produces:

```go
// ModelClient implements Completer against an OpenAI-compatible /v1.
type ModelClient struct {
	BaseURL string // e.g. "http://127.0.0.1:8000" — no trailing /v1
	Model   string // served model id passed through in the request body
	APIKey  string // optional; sent as Bearer when non-empty (LiteLLM wants one)
	HTTP    *http.Client
}

func (c *ModelClient) Complete(ctx context.Context, system, user string, structured bool) (string, error)
```

- Request: `POST {BaseURL}/v1/chat/completions`, body `{"model": c.Model, "messages": [{"role":"system","content":system},{"role":"user","content":user}], "temperature": 0}`. When `structured`, add `"response_format": {"type":"json_schema","json_schema":{"name":"redaction_findings","schema":{...}}}` with the array-of-{literal,category} schema (vLLM structured output, spec's preference 1).
- A 4xx response whose body mentions `response_format` (or any 400 while `structured`) returns `ErrStructuredUnsupported` — the pass falls back (spec's preference 2). Other non-200s and transport errors return plain errors with status and a trimmed body excerpt (lowercase, actionable).
- Reply extraction: `choices[0].message.content`. A `reasoning` field, think tags, or extra fields are ignored — only content is read.

- [ ] **Step 1: Failing tests** — httptest server asserting method/path/body shape; canned `{"choices":[{"message":{"content":"[]"}}]}`; a 400-on-response_format case returning `ErrStructuredUnsupported`; a 500 case surfacing status in the error; auth header present only when APIKey set; temperature 0 and model id in body.
- [ ] **Step 2: Run to fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run to pass.**
- [ ] **Step 5: Commit** — `manual: one client speaks to any v1 endpoint for the model pass`

### Task 6: The modelpass endpoint

**Files:**
- Modify: `internal/httpapi/docredact.go` (new case in `docredactSessionAction`)
- Test: `internal/httpapi/docredact_test.go` (extend, following the file's existing fixtures)

**Interfaces:**
- Consumes: `docredact.ModelClient`, `Document.ApplyModelPass`, `s.activeReadyRecipe`, the serving-gate (`s.gate` — read `roles.go:tryAdmit` and `proxyModel` first and mirror how a hold is acquired and released).
- Produces: `POST /api/v1/docredact/sessions/{id}/modelpass` → `200 {"findings": [...], "model_pass": {<ModelPassResult fields>, "model": "<display name>"}}`.
- Behavior:
  - `AuthorizeMutation` like every other POST in the file.
  - Resolve the redaction model in this order (owner decision 2026-08-13: a dedicated model is the intended steady state): (1) a role named `redactor` when the roles system (`roles.go`) has one assigned and its model is ready — study how roles resolve a `/v1` request's model field and reuse that helper if it is cleanly callable outside a proxied request; (2) an explicit `"model"` field in the request body resolved the same way; (3) fall back to `activeReadyRecipe`. The response's `model_pass.model` always names which model actually answered, so the fallback is visible, never silent. A media-generation recipe or nothing ready → `503` with an honest sentence: `"no text model is serving, so the model pass cannot run. The pattern findings are unchanged."`
  - Acquire the serving hold before dialing; release when the pass returns (a redaction pass is short-lived compared to a switch and must not race one — same reason as `proxyModel`).
  - Build `docredact.ModelClient{BaseURL: fmt.Sprintf("http://127.0.0.1:%d", active.Service.DefaultHostPort), Model: <the recipe's served model id — find the exact field in internal/recipe/types.go>, HTTP: &http.Client{Timeout: 2 * time.Minute}}` (per-request timeout; the pass's overall bound is the request context).
  - Lock the session mutex for the whole pass (consistent with every other session case; a second concurrent modelpass on the same session simply waits and then mostly produces Duplicates — idempotent enough, no extra state).
  - `ApplyModelPass` error (ctx cancelled) → client went away; nothing to write.
- Tests (no hardware): build the server fixture the way existing docredact httpapi tests do, point a fake runtime at it — an `httptest.Server` returning canned completions — by injecting a recipe whose `DefaultHostPort` is the httptest listener's port (the existing httpapi test scaffolding constructs recipes; follow `server_test.go` / `roles_test.go` patterns). Cover: happy path (model finding appears with source "model" in the response findings), degraded path (runtime returns garbage twice → `model_pass.degraded == true`, pattern findings intact), no-model-serving 503 sentence, auth required.
- [ ] **Step 1: Failing tests.** — as above.
- [ ] **Step 2: Run to fail** — `go test ./internal/httpapi/ -run Docredact`.
- [ ] **Step 3: Implement the case.**
- [ ] **Step 4: Run full httpapi tests to pass.**
- [ ] **Step 5: Commit** — `manual: the console can ask the serving model what the patterns missed`

### Task 7: Labeled benchmark corpus

**Files:**
- Create: `internal/docredact/testdata/corpus/*.json` (12–16 documents)
- Create: `internal/docredact/benchcorpus.go` (loader — exported so `cmd/docredact-bench` can use it)
- Test: `internal/docredact/benchcorpus_test.go`

**Interfaces:**
- Produces:

```go
type CorpusDoc struct {
	Name   string     `json:"name"`
	Locale string     `json:"locale"` // "IT" or "US"
	Text   string     `json:"text"`
	Gold   []GoldItem `json:"gold"`
}

type GoldItem struct {
	Literal  string   `json:"literal"`
	Category Category `json:"category"`
}

func LoadCorpus(dir string) ([]CorpusDoc, error) // reads *.json sorted by filename
```

- Corpus content rules (this is test data, so it must be unmistakably synthetic but structurally valid):
  - Every checksum-bearing gold literal must actually validate: IBANs pass mod-97 (compute them, e.g. rearrange and pick check digits so the number ≡ 1 mod 97), cards pass Luhn (use known test PANs: `4111111111111111`, `5500005555555559`, `4012888888881881`), codici fiscali have a valid check character (compute per the official odd/even table), SSNs use the hyphenated form avoiding invalid ranges (e.g. `212-09-9999` style — never area 000/666/9xx).
  - Emails/domains use `example.com` / `example.it`; phones use +1 555 and synthetic +39 numbers; names/orgs/addresses are invented Italian and American ones (Maria Bianchi, Studio Legale Ferrari, 742 Maple Street Springfield…).
  - Document shapes: an Italian employment contract, a medical referral letter, a US HR complaint, an invoice with IBAN and amounts, a rental agreement, a cover letter, meeting minutes, a support email thread, a bank letter, a legal demand letter, plus 2–4 negative-heavy docs (order numbers that look like phones, ISO dates that are not birth dates, version strings that look like IPs, a public-figure-free tech memo with no PII at all — gold list empty).
  - Gold lists include BOTH pattern-category items (email, iban, card…) and model-category items (person, org, address, job_title, amount). Every gold literal appears verbatim in its text.
- [ ] **Step 1: Failing test first** (loader + corpus invariants):

```go
func TestCorpusInvariants(t *testing.T) {
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 12 {
		t.Fatalf("corpus has %d docs, want at least 12", len(docs))
	}
	for _, doc := range docs {
		for _, g := range doc.Gold {
			if !strings.Contains(doc.Text, g.Literal) {
				t.Fatalf("%s: gold literal %q not in text", doc.Name, g.Literal)
			}
		}
		// every pattern-category gold literal must be found by the pattern pass:
		// the corpus may not claim the deterministic pass finds things it does not.
		found := map[string]bool{}
		for _, f := range Analyze(doc.Text).Findings {
			found[f.Literal] = true
		}
		for _, g := range doc.Gold {
			switch g.Category {
			case CategoryEmail, CategoryPhone, CategoryIBAN, CategoryCard,
				CategoryIPv4, CategoryIPv6, CategoryDOB, CategoryITCF, CategoryUSSSN:
				if !found[g.Literal] {
					t.Fatalf("%s: pattern gold %q (%s) not detected", doc.Name, g.Literal, g.Category)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run to fail** (no loader, no corpus).
- [ ] **Step 3: Write loader + corpus docs.** Iterate until the invariant test passes — if a pattern detector genuinely misses a valid literal, FIX THE CORPUS DOC or move that literal to the model-category gold only if honest; never weaken a detector in this plan. If a detector bug surfaces, record it in the final report instead of patching it here.
- [ ] **Step 4: Run to pass.**
- [ ] **Step 5: Commit** — `manual: a labeled corpus the redactor can be measured against`

### Task 8: Scoring and the bench command

**Files:**
- Create: `internal/docredact/benchscore.go`
- Create: `cmd/docredact-bench/main.go`
- Test: `internal/docredact/benchscore_test.go`

**Interfaces:**
- Consumes: `LoadCorpus`, `Analyze`, `ApplyModelPass`, `ModelClient`, `ModelPassResult`.
- Produces:

```go
// Score measures outcomes, not span bookkeeping: a gold literal still visible
// in the redacted output is a leak, whatever the passes did internally.
type Score struct {
	Gold          int            `json:"gold"`
	Leaked        int            `json:"leaked"`
	LeakedByCat   map[Category]int `json:"leaked_by_category"`
	GoldByCat     map[Category]int `json:"gold_by_category"`
	OverRedacted  int            `json:"over_redacted"` // enabled findings overlapping no gold occurrence
	Hallucinated  int            `json:"hallucinated"`  // from ModelPassResult, 0 for pattern-only
}

func ScoreDocument(doc *Document, gold []GoldItem, pass ModelPassResult) Score
```

- Leak rule: gold literal leaks iff `strings.Contains(doc.Redacted(), g.Literal)`. (Replacement tokens never contain user text, so a redacted occurrence cannot false-negative this check; a gold literal whose every occurrence sat inside a longer accepted finding is correctly not a leak.)
- Over-redaction rule: count enabled findings none of whose resolved spans intersect any occurrence-span of any gold literal (compute gold occurrence spans with the same exact-search loop as `manualMatches`).
- `cmd/docredact-bench` flags: `-corpus` (default `internal/docredact/testdata/corpus`), `-base-url` (empty = pattern-only run), `-model` (comma-separated list, one arm each), `-api-key` (optional, for a LiteLLM gateway), `-json` (write machine-readable results to a path), `-timeout` per document (default 5m). Output per arm: total gold, leaks (rate), per-category leaks, over-redactions, hallucinated, chunks failed, wall time per doc — as a plain aligned text table; honest numbers only, no color, no emoji. The pattern-only arm always runs first as the baseline.
- [ ] **Step 1: Failing tests for ScoreDocument** — hand-built doc: text with one email (pattern catches), one name the fake model catches, one name nobody catches (leak), one over-redaction (manual add of an innocuous word); assert every Score field exactly.
- [ ] **Step 2: Run to fail.**
- [ ] **Step 3: Implement `benchscore.go`, then `main.go`** (main is thin: flags → LoadCorpus → per arm per doc: Analyze, optional ApplyModelPass with a ModelClient, ScoreDocument, aggregate, print).
- [ ] **Step 4: Run tests + smoke run** — `go run ./cmd/docredact-bench` (pattern-only) must print a table with zero leaks in pattern categories and leaks equal to model-category gold counts (nothing runs the model). Paste the table into the task report.
- [ ] **Step 5: Commit** — `manual: a bench command measures what the redactor misses`

### Task 9: ADR and results document

**Files:**
- Create: `docs/decisions/0022-doc-redactor-model-pass.md`
- Create: `docs/research/redactor-model-qualification.md`

**Interfaces:** none (documentation).

- [ ] **Step 1: Write ADR 0022** — record: the owner's 2026-08-13 decision that redaction uses a dedicated pinned model (repeatable quality beats convenience; the endpoint's `redactor`-role-first resolution encodes it, with the visible fallback as the interim state); the Completer seam and why the engine owns chunking/parsing/verification while httpapi owns model resolution; the literal-only contract and hallucination counting (from the spec, now implemented); append-order tie-breaking making pattern beat model on equal spans; degradation semantics (misbehavior is data, not error; `Degraded` only when zero usable chunks); the five model categories and why unknown names fall to phrase; the endpoint's serving-gate hold; structured-output preference with `ErrStructuredUnsupported` fallback. Match ADR 0021's voice and format. Status: accepted, implemented 2026-08-13.
- [ ] **Step 2: Write the qualification method doc** — `docs/research/redactor-model-qualification.md`: the owner's decision (2026-08-13: the redactor uses a dedicated pinned model for controlled, repeatable quality; this doc is how that model gets chosen); the leak-based metric and why (product outcome, not span bookkeeping); the candidate classes to measure (very small open instruct models in the 0.5B–4B range runnable alongside a primary model, and GLiNER-class NER models as a possible future third pass per spec 12's own exit clause — name NO specific candidate as chosen); how to run (bench command lines for: pattern-only; via LiteLLM gateway/Ollama on the Studio for quality numbers now; via a Spark for latency/memory once free); and a results table whose every cell is `n/a` with the sentence "no measured run has filled this table yet." The dedicated model ships as a pinned recipe assigned to the `redactor` role once it passes; co-residency with a primary model is a roles/#38 qualification, recorded there. No invented numbers anywhere.
- [ ] **Step 3: `go build ./... && go vet ./... && go test ./...`** — still green (docs only, but run anyway per convention).
- [ ] **Step 4: Commit** — `manual: the model pass choices and the benchmark method are written down`

### Task 10: Console mockup for the model pass (approval-gated, NOT wired)

**Files:**
- Create (scratchpad only, never committed): a static HTML mockup of the Redactor tab with the model pass affordance.

**Interfaces:** none. Console implementation is deliberately OUT of this plan: the design loop requires owner approval of a static mockup before any new console concept is built.

- [ ] **Step 1: Build the mockup** in the session scratchpad using the console design system (dense dark table, pill buttons radius 999px, one primary `#76b900` per cluster, no emoji, sentence case): the existing findings table plus — an "Ask the model" ghost button next to analyze; model findings rows with a `model` source tag sorted after pattern findings within a category; a status line for the pass ("asked <model name>: 4 new findings, 1 claim not in the document"); the degraded sentence ("the model's answers were unusable, so these findings are from patterns alone"); the hallucinated-count line visible when nonzero (spec: "That count is shown").
- [ ] **Step 2: Publish as a private artifact** for the owner to review; the artifact link goes in the final report. No commit.

---

## Self-review notes (already applied)

- Spec coverage: chunking+overlap (T2), literal-only contract + hallucination count (T4), structured-output preference then lenient parse with one repair retry (T3/T5/T4), degradation to pattern-only that still exports (T4/T6), findings say which pass (T1/T4), model findings after pattern within category (T10 mockup; engine order untouched), httptest-for-/v1 test strategy (T5/T6), "count is shown" (T6 response + T10).
- Type consistency: `Completer`, `ModelItem`, `ModelPassResult`, `Score`, `CorpusDoc/GoldItem` names match across tasks.
- Deliberately out of scope: console wiring (mockup-gated), PDF/docx input (spec step 5), NER third pass (needs the harness's numbers first), the dedicated-model recipe itself and its `redactor` role assignment (needs a measured winner first), any run against models on the MSIs (owner: machines not ready).
