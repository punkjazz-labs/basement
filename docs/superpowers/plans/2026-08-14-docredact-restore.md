# Redactor Restore Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the reverse trip of the redactor: paste a cloud model's reply (which quotes pseudonyms like `[PERSON_1]`) and get the real text back, restored locally from the session's mapping or from a saved mapping file.

**Architecture:** A pure engine function `Restore` in `internal/docredact` (single pass over pseudonym-shaped matches, so a restored literal is never re-scanned and an invented pseudonym is counted rather than corrupted), plus two HTTP endpoints: a session-based one that uses the live mapping, and a stateless one that accepts a saved mapping file's bytes, for when the session is long gone. No console UI in this plan: the Restore UI goes through the owner's mockup-first design loop separately.

**Tech Stack:** Go only. `httptest` for endpoint tests, the existing labeled corpus for a round-trip invariant.

**Spec:** `docs/plans/12-doc-redactor.md`, open question "Restoring the answer" ("the mapping file makes the reverse trip possible ... arguably the better half of the product"). `internal/docredact/mapping.go` already ships `ParseMapping` whose comment names this exact feature. ADR 0021 governs the mapping file format (warning line + JSON payload); this plan consumes that format unchanged.

## Global Constraints

- Commit messages: `manual: <plain lowercase sentence>` style.
- Verify before every commit: `go build ./... && go vet ./... && go test ./...`.
- No em dashes in docs, comments, or copy.
- No console UI changes: nothing under `webui/` and nothing in `internal/webui/assets` changes in this plan.
- The engine stays pure: `Restore` does no I/O, holds no locks, and never mutates its inputs or the Document.
- Restoration is SINGLE PASS: the output of one replacement is never scanned again. A mapping literal that itself contains pseudonym-shaped text must survive restoration verbatim (there is a test that pins this).
- The stateless endpoint parses the mapping with the existing `docredact.ParseMapping`; it must not invent a second mapping format.

---

### Task 1: The engine (`Restore`) with tests

**Files:**
- Create: `internal/docredact/restore.go`
- Test: `internal/docredact/restore_test.go`

**Interfaces:**
- Produces for Task 2: `func Restore(text string, entries []MappingEntry) (string, RestoreResult)` and `type RestoreResult struct { Replaced int; Tokens int; Unknown []string }` with json tags `replaced`, `tokens`, `unknown`.

- [ ] **Step 1: Write the failing tests** in `internal/docredact/restore_test.go`:

```go
package docredact

import (
	"reflect"
	"testing"
)

func restoreEntries() []MappingEntry {
	return []MappingEntry{
		{Token: "[PERSON_1]", Literal: "Marta Ferretti"},
		{Token: "[PERSON_11]", Literal: "Jonas Weber"},
		{Token: "[ORG_1]", Literal: "Nordwind Logistik GmbH"},
	}
}

func TestRestoreReplacesEveryOccurrence(t *testing.T) {
	text := "Ask [PERSON_1] at [ORG_1]. [PERSON_1] answers on Fridays."
	restored, result := Restore(text, restoreEntries())
	want := "Ask Marta Ferretti at Nordwind Logistik GmbH. Marta Ferretti answers on Fridays."
	if restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	if result.Replaced != 3 || result.Tokens != 2 {
		t.Fatalf("result = %+v, want Replaced 3 Tokens 2", result)
	}
	if len(result.Unknown) != 0 {
		t.Fatalf("unknown = %v, want none", result.Unknown)
	}
}

func TestRestoreDoesNotConfuseTokenPrefixes(t *testing.T) {
	// [PERSON_1] must never match inside [PERSON_11]: the closing bracket
	// makes each pseudonym self-delimiting, and this pins it.
	restored, result := Restore("[PERSON_11] then [PERSON_1]", restoreEntries())
	if want := "Jonas Weber then Marta Ferretti"; restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	if result.Replaced != 2 || result.Tokens != 2 {
		t.Fatalf("result = %+v, want Replaced 2 Tokens 2", result)
	}
}

func TestRestoreCountsUnknownTokensAndLeavesThemIntact(t *testing.T) {
	text := "[PERSON_1] met [PERSON_9] and [X_2]; [PERSON_9] again."
	restored, result := Restore(text, restoreEntries())
	want := "Marta Ferretti met [PERSON_9] and [X_2]; [PERSON_9] again."
	if restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	// Distinct, first-appearance order.
	if wantUnknown := []string{"[PERSON_9]", "[X_2]"}; !reflect.DeepEqual(result.Unknown, wantUnknown) {
		t.Fatalf("unknown = %v, want %v", result.Unknown, wantUnknown)
	}
	if result.Replaced != 1 || result.Tokens != 1 {
		t.Fatalf("result = %+v, want Replaced 1 Tokens 1", result)
	}
}

func TestRestoreIsSinglePass(t *testing.T) {
	// A literal that contains pseudonym-shaped text must arrive verbatim
	// and never be re-scanned: restoration reads the input once.
	entries := []MappingEntry{
		{Token: "[PHRASE_1]", Literal: "see [ORG_1] file"},
		{Token: "[ORG_1]", Literal: "Nordwind"},
	}
	restored, result := Restore("[PHRASE_1] and [ORG_1]", entries)
	if want := "see [ORG_1] file and Nordwind"; restored != want {
		t.Fatalf("restored = %q, want %q", restored, want)
	}
	if result.Replaced != 2 || result.Tokens != 2 || len(result.Unknown) != 0 {
		t.Fatalf("result = %+v, want Replaced 2 Tokens 2 Unknown none", result)
	}
}

func TestRestoreEmptyCases(t *testing.T) {
	if restored, result := Restore("", restoreEntries()); restored != "" || result.Replaced != 0 || len(result.Unknown) != 0 {
		t.Fatalf("empty text: %q %+v", restored, result)
	}
	if restored, result := Restore("no tokens here", nil); restored != "no tokens here" || result.Replaced != 0 {
		t.Fatalf("nil entries: %q %+v", restored, result)
	}
	if result := func() RestoreResult { _, r := Restore("plain", restoreEntries()); return r }(); result.Unknown == nil {
		t.Fatalf("Unknown must be an empty slice, not nil, so the JSON reads [] rather than null")
	}
}

func TestRestoreRoundTripsTheCorpus(t *testing.T) {
	// The product invariant: redact, then restore with the mapping, and the
	// original text comes back byte for byte, for every corpus document.
	docs, err := LoadCorpus("testdata/corpus")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	for _, cd := range docs {
		doc := Analyze(cd.Text)
		restored, _ := Restore(doc.Redacted(), doc.Mapping())
		if restored != cd.Text {
			t.Fatalf("%s: restore(redacted) != original", cd.Name)
		}
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**: `go test ./internal/docredact/ -run TestRestore -v`. Expected: FAIL, `Restore` undefined.

- [ ] **Step 3: Implement `internal/docredact/restore.go`**:

```go
package docredact

import "regexp"

// tokenShape matches anything shaped like a pseudonym this package mints:
// an uppercase prefix, an underscore, a number, in brackets. The console's
// preview uses the same shape. A match is only restored when the mapping
// actually names it; everything else is left exactly where it stood and
// reported as unknown, because a pseudonym the mapping never minted is the
// cloud model inventing one.
var tokenShape = regexp.MustCompile(`\[[A-Z][A-Z0-9]*_\d+\]`)

// RestoreResult tallies what one Restore call did: how many pseudonym
// occurrences were swapped back, how many distinct pseudonyms that covered,
// and which pseudonym-shaped strings had no mapping entry, distinct and in
// first-appearance order.
type RestoreResult struct {
	Replaced int      `json:"replaced"`
	Tokens   int      `json:"tokens"`
	Unknown  []string `json:"unknown"`
}

// Restore swaps every mapped pseudonym in text back to its literal. It is
// a single pass: replacement output is never re-scanned, so a literal that
// itself contains pseudonym-shaped text survives verbatim, and restoration
// can never cascade. Neither text nor entries is mutated.
func Restore(text string, entries []MappingEntry) (string, RestoreResult) {
	byToken := make(map[string]string, len(entries))
	for _, entry := range entries {
		byToken[entry.Token] = entry.Literal
	}

	result := RestoreResult{Unknown: []string{}}
	seenTokens := make(map[string]bool)
	seenUnknown := make(map[string]bool)

	restored := tokenShape.ReplaceAllStringFunc(text, func(token string) string {
		literal, ok := byToken[token]
		if !ok {
			if !seenUnknown[token] {
				seenUnknown[token] = true
				result.Unknown = append(result.Unknown, token)
			}
			return token
		}
		result.Replaced++
		if !seenTokens[token] {
			seenTokens[token] = true
			result.Tokens++
		}
		return literal
	})
	return restored, result
}
```

- [ ] **Step 4: Run the tests to see them pass**: `go test ./internal/docredact/ -run TestRestore -v`, then the full package: `go test ./internal/docredact/`. Expected: PASS.

- [ ] **Step 5: Verify and commit**: `go build ./... && go vet ./... && go test ./...`, then `git add internal/docredact/restore.go internal/docredact/restore_test.go && git commit -m "manual: the redactor can put real names back"`

---

### Task 2: The two restore endpoints with tests

**Files:**
- Modify: `internal/httpapi/docredact.go` (session route case + stateless handler)
- Modify: `internal/httpapi/server.go` (one route registration line, next to the existing docredact lines at ~219-220)
- Test: `internal/httpapi/docredact_test.go` (extend, matching its existing style)

**Interfaces:**
- Consumes from Task 1: `docredact.Restore`, `docredact.RestoreResult`, plus existing `docredact.ParseMapping`, `(*docredact.Document).Mapping()`.

- [ ] **Step 1: Read `internal/httpapi/docredact_test.go` first** and follow its existing helpers and style for the new tests.

- [ ] **Step 2: Write failing endpoint tests** covering:

1. Session restore: analyze a text with an email in it, fetch the findings to learn the token, POST `{"text": "quote <token> back"}` to `/api/v1/docredact/sessions/{id}/restore`, expect 200 with the literal restored in `text`, `replaced` 1, `tokens` 1, `unknown` `[]`.
2. Session restore with a disabled finding: toggle the finding off first; its token is then NOT in the mapping, so restoring a reply that quotes it returns the token untouched and listed in `unknown`.
3. Session restore with empty text: 400.
4. Unknown session id: 404.
5. Stateless restore: build a real mapping via `docredact.Analyze` + `MappingBytes`, POST `{"text": ..., "mapping": "<the file bytes as a string>"}` to `/api/v1/docredact/restore`, expect the same 200 shape.
6. Stateless restore with garbage mapping: 400 whose error mentions the mapping.
7. Stateless restore with empty text: 400.

Test names: `TestDocredactSessionRestore`, `TestDocredactSessionRestoreSkipsDisabledFindings`, `TestDocredactRestoreStateless`, `TestDocredactRestoreRejectsBadInput` (fold the 400/404 cases into this one following the file's table style if it has one).

- [ ] **Step 3: Run them to see them fail**: `go test ./internal/httpapi/ -run TestDocredact -v` (new ones FAIL, existing ones PASS).

- [ ] **Step 4: Implement.** In `docredactSessionAction`, add a case (place it after the `modelpass` case, before `preview`):

```go
	// The reverse trip: the owner pastes a cloud model's reply and gets the
	// real text back. The mapping used is the session's current one, so a
	// finding switched off before export stays unknown here too: its token
	// was never in the redacted copy, so a reply quoting it is the model
	// inventing a pseudonym, not this session's own.
	case len(parts) == 2 && parts[1] == "restore" && r.Method == http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		var request struct {
			Text string `json:"text"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(request.Text) == "" {
			writeError(w, http.StatusBadRequest, errors.New("text is required"))
			return
		}
		session.mu.Lock()
		entries := session.doc.Mapping()
		session.mu.Unlock()
		restored, result := docredact.Restore(request.Text, entries)
		writeJSON(w, http.StatusOK, restoreResponse(restored, result))
```

Add the stateless handler and the shared response shape at the bottom of `docredact.go`:

```go
// restoreResponse is the one wire shape both restore routes answer with,
// so the console never has to care which route it called.
func restoreResponse(text string, result docredact.RestoreResult) map[string]any {
	return map[string]any{
		"text":     text,
		"replaced": result.Replaced,
		"tokens":   result.Tokens,
		"unknown":  result.Unknown,
	}
}

// docredactRestore is the stateless reverse trip, for a reply that comes
// back after the session is gone: the owner supplies the saved mapping
// file's own bytes and the pasted reply, and nothing here touches a
// session or this machine's disk. The mapping is parsed by the same code
// that wrote it (docredact.ParseMapping), warning line and all.
func (s *Server) docredactRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var request struct {
		Text    string `json:"text"`
		Mapping string `json:"mapping"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeError(w, http.StatusBadRequest, errors.New("text is required"))
		return
	}
	_, entries, err := docredact.ParseMapping([]byte(request.Mapping))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("mapping file not understood: %w", err))
		return
	}
	restored, result := docredact.Restore(request.Text, entries)
	writeJSON(w, http.StatusOK, restoreResponse(restored, result))
}
```

Register the stateless route in `server.go`, directly under the two existing docredact lines:

```go
	mux.HandleFunc("/api/v1/docredact/restore", server.withReadAuth(server.docredactRestore))
```

- [ ] **Step 5: Run the endpoint tests to see them pass**: `go test ./internal/httpapi/ -run TestDocredact -v`. Expected: PASS, all.

- [ ] **Step 6: Verify and commit**: `go build ./... && go vet ./... && go test ./...`, then commit the three files with message `manual: the manager answers the reverse trip`
