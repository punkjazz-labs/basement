# Redactor Console Model Pass Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the approved model-pass design into the console's Redactor tab: an "Ask the model" action, a pass-receipt line over the findings list, model findings grouped and pilled correctly, and the viewbar moved onto the console's pill button family.

**Architecture:** The Go endpoint (`POST /api/v1/docredact/sessions/{id}/modelpass`) already exists and is not touched. All changes are in the console: pure logic and types in `webui/ui/src/docredact.ts` and `webui/ui/src/api.ts` (vitest-tested), thin wiring in `webui/ui/src/views/Redactor.tsx`, styles in `webui/ui/src/styles.css`, then a bundle rebuild into `internal/webui/assets`.

**Tech Stack:** React + TypeScript (vite build, vitest tests), Go embed for the bundle.

**Spec:** `docs/plans/12-doc-redactor.md` (the doc redactor spec; ADR 0022 records the model-pass contract). The visual design is the mockup the owner approved on 2026-08-13, whose decisions are restated inline below so this plan is self-contained.

## Global Constraints

- Commit messages: `manual: <plain lowercase sentence>` style used across this repo.
- Verify before every commit: `go build ./... && go vet ./... && go test ./...`, and for console work also `cd webui/ui && npm test` and `npm run build`.
- No em dashes in documentation or UI copy.
- No invented product facts; UI copy for the no-model case uses the manager's own sentence, not a paraphrase.
- Pure logic lives in `docredact.ts` and is tested; `Redactor.tsx` stays a thin wiring layer. This is the existing pattern and the reviewer should reject logic embedded in the TSX that could be pure.
- The Go side is frozen for this plan: any change under `internal/` other than the rebuilt `internal/webui/assets` bundle is out of scope.

## Design decisions this plan encodes (from the approved mockup and owner refinements)

1. The viewbar cluster reads open, improve, export: `Open file` (ghost pill), `Ask the model` (ghost pill), `Export safe copy` (orange primary pill). The primary stays on export. The old `.viewbar .open` / `.viewbar .export` 6px button styles are retired; only Redactor.tsx uses them.
2. Owner refinement 2026-08-13: the orange primary must be disabled or absent until a document is loaded. The tab already returns a drop zone with no viewbar when no document is open, which satisfies "absent"; while a document is open the export button stays live (the redacted text always exists once analyzed).
3. Owner refinement 2026-08-13: export is only ever the redacted output plus mapping, never the raw text. Already true of the endpoint; nothing in this plan may add a raw-text export.
4. The pass leaves one quiet receipt line pinned between the Findings heading and the list: `asked <model>: N new findings` plus, only when nonzero and in amber, `, M claims not in the document`. M is the `hallucinated` count. `duplicates`, `chunks_total`, `chunks_failed` are diagnostics and are not shown.
5. While the pass runs the line reads `asking the model` with a small dot. Chunk progress is NOT shown: the endpoint is a single POST with no streaming, so the console cannot know it. (The mockup drew "2 of 3 chunks read"; this plan drops it deliberately rather than fake it. Ruling recorded here so the reviewer does not flag the difference as a miss.)
6. While the pass runs, `Open file` and `Ask the model` are disabled; toggles, preview, selection-hide and export stay enabled. (Server-side those requests queue behind the session mutex until the pass finishes; that is acceptable and needs no Go change.)
7. Degraded pass (`degraded: true`): the receipt line is replaced by the amber sentence `the model's answers were unusable, so these findings are from patterns alone`. The list, toggles and export keep working and the button stays live for a retry.
8. No model serving (HTTP 503): an error note appears under the viewbar showing the server's own message (`no text model is serving, so the model pass cannot run. The pattern findings are unchanged.`) with the faint follow-up line `Start a text model on the Models tab, then ask again.` The Ask button stays enabled so the owner can retry after starting one.
9. Model source rows wear the existing `.pill.model` purple pill (already in styles.css, unused until now). Within each category group, model rows sort after pattern and manual rows, so a rule row is always the first thing read. Sort is stable otherwise.
10. Category filing (this replaces the mockup's "Decision needed 1"): `person`, `org`, `job_title` file under "People and companies"; `address` under "Contact"; `amount` under "Money and accounts"; the nine national-ID categories (`fr_nir`, `es_dni`, `pt_nif`, `de_steuer_id`, `nl_bsn`, `uk_nino`, `br_cpf`, `cn_resident_id`, `jp_my_number`) file explicitly under "Dates and IDs". The unknown-category fallback to the last group stays.
11. The receipt, any error note, and the asking state all reset when a new document is opened.

---

### Task 1: Pure logic and types (`docredact.ts`, `api.ts`) with tests

**Files:**
- Modify: `webui/ui/src/api.ts` (add types next to `DocredactSession`)
- Modify: `webui/ui/src/docredact.ts` (GROUPS, ordering, receipt phrasing)
- Test: `webui/ui/src/docredact.test.ts` (extend the existing file, keep its style)

**Interfaces:**
- Produces for Task 2: `DocredactModelPass`, `DocredactModelPassResponse` (api.ts); `orderFindings`, `passReceipt` (docredact.ts); updated `GROUPS` behaviour inside `groupFindings`.

- [ ] **Step 1: Add the response types to `api.ts`**, directly after `DocredactSession`:

```ts
// One model pass over an open session: the tally the engine kept while
// applying the model's answers, plus the model that actually answered, so a
// fall back from the redactor role to whatever is serving is visible.
export interface DocredactModelPass {
  accepted: number
  duplicates: number
  hallucinated: number
  chunks_total: number
  chunks_failed: number
  degraded: boolean
  model: string
}

export interface DocredactModelPassResponse {
  findings: DocredactFinding[]
  model_pass: DocredactModelPass
}
```

- [ ] **Step 2: Write failing tests in `docredact.test.ts`** for the three pure changes (follow the existing test file's describe/it naming style):

```ts
describe('groupFindings with model categories', () => {
  const finding = (category: string, source = 'pattern', literal = 'x'): DocredactFinding =>
    ({ id: literal, token: '[X_1]', literal, category, source, occurrences: 1, enabled: true })

  it('files person, org and job_title under People and companies', () => {
    const groups = groupFindings([finding('person'), finding('org'), finding('job_title')])
    expect(groups).toHaveLength(1)
    expect(groups[0].heading).toBe('People and companies')
    expect(groups[0].findings).toHaveLength(3)
  })

  it('files address under Contact and amount under Money and accounts', () => {
    const groups = groupFindings([finding('address'), finding('amount')])
    expect(groups.map(group => group.heading)).toEqual(['Contact', 'Money and accounts'])
  })

  it('files every national identifier under Dates and IDs', () => {
    const ids = ['fr_nir', 'es_dni', 'pt_nif', 'de_steuer_id', 'nl_bsn', 'uk_nino', 'br_cpf', 'cn_resident_id', 'jp_my_number']
    const groups = groupFindings(ids.map(category => finding(category, 'pattern', category)))
    expect(groups).toHaveLength(1)
    expect(groups[0].heading).toBe('Dates and IDs')
    expect(groups[0].findings).toHaveLength(ids.length)
  })

  it('still files a category nobody named under the last heading', () => {
    const groups = groupFindings([finding('never_heard_of_it')])
    expect(groups).toHaveLength(1)
    expect(groups[0].heading).toBe('Dates and IDs')
  })

  it('sorts model rows after pattern and manual rows inside a group, stably', () => {
    const groups = groupFindings([
      finding('person', 'model', 'model-first'),
      finding('person', 'pattern', 'rule-a'),
      finding('person', 'model', 'model-second'),
      finding('person', 'manual', 'by-hand'),
    ])
    expect(groups[0].findings.map(item => item.literal))
      .toEqual(['rule-a', 'by-hand', 'model-first', 'model-second'])
  })
})

describe('passReceipt', () => {
  const pass = (accepted: number, hallucinated: number): DocredactModelPass =>
    ({ accepted, duplicates: 0, hallucinated, chunks_total: 1, chunks_failed: 0, degraded: false, model: 'Qwen 3.6 27B' })

  it('counts new findings in words', () => {
    expect(passReceipt(pass(4, 0))).toEqual({ model: 'Qwen 3.6 27B', findings: '4 new findings', claims: '' })
    expect(passReceipt(pass(1, 0)).findings).toBe('1 new finding')
    expect(passReceipt(pass(0, 0)).findings).toBe('no new findings')
  })

  it('names claims only when the model made one that was not in the document', () => {
    expect(passReceipt(pass(2, 1)).claims).toBe('1 claim not in the document')
    expect(passReceipt(pass(2, 3)).claims).toBe('3 claims not in the document')
  })
})
```

- [ ] **Step 3: Run the new tests to see them fail**: `cd webui/ui && npx vitest run src/docredact.test.ts`. Expected: failures on missing `passReceipt` and wrong grouping.

- [ ] **Step 4: Implement in `docredact.ts`**. Replace the `GROUPS` constant and extend `groupFindings` with ordering; add `passReceipt`:

```ts
const GROUPS: Array<{ heading: string; categories: string[] }> = [
  { heading: 'People and companies', categories: ['phrase', 'person', 'org', 'job_title'] },
  { heading: 'Contact', categories: ['email', 'phone', 'address'] },
  { heading: 'Money and accounts', categories: ['iban', 'card', 'amount'] },
  {
    heading: 'Dates and IDs',
    categories: [
      'dob', 'it_codice_fiscale', 'us_ssn', 'ipv4', 'ipv6',
      'fr_nir', 'es_dni', 'pt_nif', 'de_steuer_id', 'nl_bsn',
      'uk_nino', 'br_cpf', 'cn_resident_id', 'jp_my_number',
    ],
  },
]

// Inside a group, whatever a rule or the owner found comes before what the
// model claimed: a rule row is exact by construction, so it is the first
// thing read. The sort is stable, so rows keep the document order they
// arrived in otherwise.
export const orderFindings = (findings: DocredactFinding[]): DocredactFinding[] =>
  [...findings.filter(finding => finding.source !== 'model'),
   ...findings.filter(finding => finding.source === 'model')]
```

and in `groupFindings`, wrap the per-group filter result with `orderFindings(...)`. Then:

```ts
// ---- The pass receipt -------------------------------------------------------

export interface PassReceipt {
  model: string
  findings: string
  claims: string
}

// The one line a finished pass leaves behind. Claims are the literals the
// model named that were not in the document: the engine already dropped
// them, so the receipt only has to say it happened, and only when it did.
export function passReceipt(pass: DocredactModelPass): PassReceipt {
  return {
    model: pass.model,
    findings: pass.accepted === 0 ? 'no new findings' : counted(pass.accepted, 'new finding', 'new findings'),
    claims: pass.hallucinated === 0 ? '' : counted(pass.hallucinated, 'claim not in the document', 'claims not in the document'),
  }
}
```

(`counted` already exists in the file; import `DocredactModelPass` from `./api`.)

- [ ] **Step 5: Run the tests to see them pass**: `cd webui/ui && npx vitest run src/docredact.test.ts`. Expected: PASS, including every pre-existing test.

- [ ] **Step 6: Commit**: `git add -A && git commit -m "manual: the console knows what a model pass returns"`

---

### Task 2: Wire `Redactor.tsx` and add the passline styles

**Files:**
- Modify: `webui/ui/src/views/Redactor.tsx`
- Modify: `webui/ui/src/styles.css` (add `.passline`, retire `.viewbar .open` / `.viewbar .export`)

**Interfaces:**
- Consumes from Task 1: `DocredactModelPassResponse`, `DocredactModelPass`, `ApiError` (already exported from api.ts), `passReceipt`.

- [ ] **Step 1: Add state and the ask action to `Redactor.tsx`**:

```tsx
const [pass, setPass] = useState<DocredactModelPass | null>(null)
const [asking, setAsking] = useState(false)
const [passProblem, setPassProblem] = useState<string | null>(null)
```

In `open()`, alongside the existing resets, add `setPass(null)`, `setPassProblem(null)`.

```tsx
// The model pass is one POST: the manager picks the model (redactor role
// first), runs every chunk, and answers with the findings as they now
// stand. A degraded pass is an answer, not an error; only "no text model
// is serving" refuses, and that sentence is shown as the manager wrote it.
const askModel = async () => {
  if (!doc || asking) return
  setAsking(true)
  setPassProblem(null)
  try {
    const body = await api<DocredactModelPassResponse>(sessionPath('/modelpass'), { method: 'POST' })
    setPass(body.model_pass)
    setFindings(body.findings ?? [])
    await loadPreview(doc.sessionID)
  } catch (problem) {
    if (problem instanceof ApiError && problem.status === 503) setPassProblem(problem.message)
    else noticeBox('Could not ask the model', message(problem))
  } finally {
    setAsking(false)
  }
}
```

(Import `ApiError`, `DocredactModelPass`, `DocredactModelPassResponse` from `../api` and `passReceipt` from `../docredact`. `ApiError` is already exported from `api.ts` with a `status: number` field and the server's message as `.message`; the 503 test is on the HTTP status, never on message text.)

- [ ] **Step 2: Move the viewbar onto the pill family and add the ask button**:

```tsx
<div className="right">
  <button className="ghost" disabled={busy || asking} onClick={() => fileInput.current?.click()}>Open file</button>
  <button className="ghost" disabled={busy || asking} onClick={() => void askModel()}>
    {asking ? 'Asking the model' : 'Ask the model'}
  </button>
  <button className="primary" onClick={exportSafeCopy}>Export safe copy</button>
</div>
```

- [ ] **Step 3: Render the error note under the viewbar** (only when `passProblem` is set):

```tsx
{passProblem && (
  <div className="error-note">
    <strong>Could not ask the model</strong>
    <p>{passProblem}</p>
    <p className="faint">Start a text model on the Models tab, then ask again.</p>
  </div>
)}
```

- [ ] **Step 4: Render the passline** between `findings-head` and `findings-body`:

```tsx
{asking && (
  <div className="passline working"><span className="dot" />asking the model</div>
)}
{!asking && pass && pass.degraded && (
  <div className="passline degraded">
    <span className="dot" />the model&apos;s answers were unusable, so these findings are from patterns alone
  </div>
)}
{!asking && pass && !pass.degraded && (() => {
  const receipt = passReceipt(pass)
  return (
    <div className="passline">
      <span>asked</span> <span className="who">{receipt.model}</span><span>: {receipt.findings}</span>
      {receipt.claims !== '' && <span className="claims">, {receipt.claims}</span>}
    </div>
  )
})()}
```

- [ ] **Step 5: styles.css**. Replace the `.viewbar .open` / `.viewbar .export` rules (lines around 1119-1123, used only by Redactor) with the passline block, placed with the other redactor styles:

```css
/* The one line the pass leaves behind, directly over the list it changed.
   Quiet by default: it is a receipt, not an alarm. A claim that was not in
   the document is the model misbehaving, so it wears the warning colour,
   never the failure colour: nothing failed. */
.passline {
  display: flex; align-items: baseline; gap: 6px; flex-wrap: wrap;
  padding: 7px 14px; border-bottom: 1px solid var(--line);
  background: color-mix(in srgb, var(--surface-2) 50%, transparent);
  color: var(--muted); font-size: 11.5px;
}
.passline .who { color: var(--ink); font-weight: 550; }
.passline .claims { color: var(--warn); }
.passline.degraded { color: var(--warn); }
.passline .dot { width: 6px; height: 6px; border-radius: 50%; background: var(--warn); flex: none; align-self: center; }
```

- [ ] **Step 6: Verify**: `cd webui/ui && npm test && npm run build` (the build also runs `tsc --noEmit`, which is the type check for the TSX). Expected: green, and the build rewrites `internal/webui/assets`.

- [ ] **Step 7: Revert the bundle for now** so the rebuild lands as its own reviewed commit in Task 3: `git checkout -- ../../internal/webui/assets` (from `webui/ui`; equivalently `git checkout -- internal/webui/assets` at the repo root).

- [ ] **Step 8: Commit**: `git add -A && git commit -m "manual: the redactor tab can ask the model"`

---

### Task 3: Rebuild the embedded bundle and verify the whole tree

**Files:**
- Modify: `internal/webui/assets/**` (generated by the vite build; never edited by hand)

- [ ] **Step 1: Rebuild**: `cd webui/ui && npm run build`. Expected: vite writes into `internal/webui/assets` (its configured outDir).

- [ ] **Step 2: Verify the Go tree carries the new bundle**: from the repo root, `go build ./... && go vet ./... && go test ./...`. Expected: green.

- [ ] **Step 3: Run the console tests once more**: `cd webui/ui && npm test`. Expected: green.

- [ ] **Step 4: Commit**: `git add -A && git commit -m "manual: rebuild the console bundle for the model pass"`
