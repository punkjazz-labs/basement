# Redactor Restore Screen Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Restore mode of the console's Redactor tab. The owner pastes a cloud model's reply. The console puts the real text back, on screen only.

**Architecture:** The backend is already on main: a session restore route and a stateless restore route. This plan is console only. Pure logic and types go in `webui/ui/src/docredact.ts` and `api.ts`, with vitest tests. The screen goes in `views/Redactor.tsx`. New styles go in `styles.css`. The bundle is rebuilt at the end.

**Tech Stack:** React + TypeScript (vite, vitest). Go untouched except the embedded bundle.

**Spec:** The owner approved the restore mockup on 2026-08-14, with one decision: the Redact/Restore switch only, no fourth button in the Redact toolbar. The mockup's copy and layout are restated inline below, so this plan is self-contained.

## Global Constraints

- Commit messages: `manual: <plain lowercase sentence>`.
- Verify before every commit: `cd webui/ui && npm test && npm run build`, then `go build ./... && go vet ./... && go test ./...` from the repo root.
- No em dashes anywhere. UI copy uses short, simple sentences (the owner requires Simplified Technical English).
- Pure logic lives in `docredact.ts` and is tested. `Redactor.tsx` stays a thin wiring layer.
- Nothing under `internal/` changes except the rebuilt `internal/webui/assets` bundle, as its own commit.
- The server is the restore engine. The client builds display segments only to color the output. If the joined segments do not equal the server's text byte for byte, the screen shows the server's text plain, with no colors. The server's text always wins.

## Design decisions (from the approved mockup and the owner's answers)

1. A pill switch with two options, Redact and Restore, sits at the far left of the viewbar. It appears in both modes. It is the only way into Restore mode (owner: "switch only is good").
2. Redact mode is the existing screen, unchanged, plus the switch. When no document is open, the switch shows above the existing drop zone, so Restore stays reachable.
3. Restore mode shows two panes. Left: a textarea, heading "The cloud's reply", hint "Paste it here". Right: the restored text, heading "What it really says".
4. Restored literals get a quiet green tint (`.back`). A pseudonym the mapping never minted stays in place as an amber chip (`.stray`).
5. The right pane's head hint shows the tally: "3 names put back" (distinct pseudonyms restored, the `tokens` field). Zero restores: "nothing to put back".
6. When the reply quotes unknown pseudonyms, a passline shows over the output: "1 pseudonym this document never used: it stays as the model wrote it" (plural: "N pseudonyms this document never used: they stay as the model wrote them").
7. Mapping source: with a document open, the session's mapping is used and the meta line reads "using this session's mapping · <name>". With no document, a drop target accepts the saved `.mapping.json`; after a drop the meta reads "using <filename>". With neither, the meta reads "no document open".
8. Buttons on the right: ghost "Copy restored text" (disabled until a result exists), orange primary "Restore" (disabled until there is reply text and a mapping source; label "Restoring" and disabled while the request runs).
9. The restored text stays on screen. Copy is the only way out. Nothing is downloaded and nothing is written to disk.
10. Editing the reply text clears the previous result. A stale result must never sit next to a newer reply.
11. A failed restore request shows through the existing `noticeBox`, like the tab's other failures. A bad mapping file (parse fails client-side) shows a short error note and no entries are kept.

---

### Task 1: Pure helpers and types with tests

**Files:**
- Modify: `webui/ui/src/api.ts` (one response type, after `DocredactModelPassResponse`)
- Modify: `webui/ui/src/docredact.ts`
- Test: `webui/ui/src/docredact.test.ts` (extend in the file's existing style)

**Interfaces:**
- Produces for Task 2: `DocredactRestoreResponse` (api.ts); `RestoreEntry`, `restoreEntriesFromFindings`, `parseMappingFile`, `RestoreSegment`, `restoreSegments`, `segmentsText`, `restoredHint`, `strayLine` (docredact.ts).

- [ ] **Step 1: Add the response type to `api.ts`**:

```ts
// One restore call: the restored text, how much came back, and which
// pseudonym-shaped strings had no mapping entry.
export interface DocredactRestoreResponse {
  text: string
  replaced: number
  tokens: number
  unknown: string[]
}
```

- [ ] **Step 2: Write failing tests in `docredact.test.ts`**:

```ts
describe('restore helpers', () => {
  const entries = [
    { token: '[PERSON_1]', literal: 'Marta Ferretti' },
    { token: '[ORG_1]', literal: 'Nordwind Logistik GmbH' },
  ]

  it('builds entries from enabled findings only', () => {
    const findings = [
      { id: 'a', token: '[PERSON_1]', literal: 'Marta Ferretti', category: 'person', source: 'model', occurrences: 1, enabled: true },
      { id: 'b', token: '[DOB_1]', literal: '1998-06-02', category: 'dob', source: 'pattern', occurrences: 1, enabled: false },
    ]
    expect(restoreEntriesFromFindings(findings)).toEqual([{ token: '[PERSON_1]', literal: 'Marta Ferretti' }])
  })

  it('splits a reply into text, restored and stray segments', () => {
    const segments = restoreSegments('Ask [PERSON_1] and [PERSON_9].', entries)
    expect(segments).toEqual([
      { kind: 'text', text: 'Ask ' },
      { kind: 'restored', text: 'Marta Ferretti' },
      { kind: 'text', text: ' and ' },
      { kind: 'stray', text: '[PERSON_9]' },
      { kind: 'text', text: '.' },
    ])
  })

  it('joins segments back into the exact restored text', () => {
    const segments = restoreSegments('Ask [PERSON_1] at [ORG_1].', entries)
    expect(segmentsText(segments)).toBe('Ask Marta Ferretti at Nordwind Logistik GmbH.')
  })

  it('never rescans a restored literal that looks like a pseudonym', () => {
    const tricky = [{ token: '[PHRASE_1]', literal: 'see [ORG_1] file' }, ...entries]
    expect(segmentsText(restoreSegments('[PHRASE_1] and [ORG_1]', tricky)))
      .toBe('see [ORG_1] file and Nordwind Logistik GmbH')
  })

  it('parses a mapping file and refuses garbage', () => {
    const raw = 'WARNING: do not upload.\n{"entries":[{"token":"[X_1]","literal":"x","category":"phrase","source":"manual","occurrences":1}]}'
    expect(parseMappingFile(raw)).toEqual([{ token: '[X_1]', literal: 'x' }])
    expect(parseMappingFile('no newline')).toBeNull()
    expect(parseMappingFile('warning\nnot json')).toBeNull()
    expect(parseMappingFile('warning\n{"entries":[{"token":1}]}')).toBeNull()
  })

  it('words the tally and the stray line', () => {
    expect(restoredHint(0)).toBe('nothing to put back')
    expect(restoredHint(1)).toBe('1 name put back')
    expect(restoredHint(3)).toBe('3 names put back')
    expect(strayLine(0)).toBe('')
    expect(strayLine(1)).toBe('1 pseudonym this document never used: it stays as the model wrote it')
    expect(strayLine(2)).toBe('2 pseudonyms this document never used: they stay as the model wrote them')
  })
})
```

- [ ] **Step 3: Run the new tests, see them fail**: `cd webui/ui && npx vitest run src/docredact.test.ts`.

- [ ] **Step 4: Implement in `docredact.ts`** (a new section after the export-names section; `TOKEN` and `counted` already exist in the file):

```ts
// ---- The reverse trip -------------------------------------------------------

// The pairs a restore needs: the pseudonym and the real text. From an open
// session these come from the enabled findings; from a saved mapping file
// they come from parseMappingFile.
export interface RestoreEntry {
  token: string
  literal: string
}

export const restoreEntriesFromFindings = (findings: DocredactFinding[]): RestoreEntry[] =>
  findings.filter(finding => finding.enabled)
    .map(finding => ({ token: finding.token, literal: finding.literal }))

// A saved .mapping.json: one warning line, then JSON. The same shape
// internal/docredact writes and parses. Anything that does not parse
// cleanly is refused whole: a half-read mapping would restore half the
// names and look done.
export function parseMappingFile(raw: string): RestoreEntry[] | null {
  const newline = raw.indexOf('\n')
  if (newline < 0) return null
  try {
    const payload = JSON.parse(raw.slice(newline + 1)) as { entries?: Array<Record<string, unknown>> }
    if (!payload || !Array.isArray(payload.entries)) return null
    const entries: RestoreEntry[] = []
    for (const entry of payload.entries) {
      if (typeof entry.token !== 'string' || typeof entry.literal !== 'string') return null
      entries.push({ token: entry.token, literal: entry.literal })
    }
    return entries
  } catch {
    return null
  }
}

// The reply split for display: what the person typed, what came back, and
// what the cloud model invented. One pass, so a restored literal is never
// rescanned. The server is still the engine of record: the caller must
// compare segmentsText against the server's text and drop the colors on
// any mismatch.
export type RestoreSegment =
  | { kind: 'text'; text: string }
  | { kind: 'restored'; text: string }
  | { kind: 'stray'; text: string }

export function restoreSegments(reply: string, entries: RestoreEntry[]): RestoreSegment[] {
  const byToken = new Map(entries.map(entry => [entry.token, entry.literal]))
  const segments: RestoreSegment[] = []
  let last = 0
  for (const match of reply.matchAll(new RegExp(TOKEN.source, 'g'))) {
    const index = match.index ?? 0
    if (index > last) segments.push({ kind: 'text', text: reply.slice(last, index) })
    const literal = byToken.get(match[0])
    if (literal === undefined) segments.push({ kind: 'stray', text: match[0] })
    else segments.push({ kind: 'restored', text: literal })
    last = index + match[0].length
  }
  if (last < reply.length) segments.push({ kind: 'text', text: reply.slice(last) })
  return segments
}

export const segmentsText = (segments: RestoreSegment[]): string =>
  segments.map(segment => segment.text).join('')

// The right pane's tally, counting distinct pseudonyms put back.
export const restoredHint = (tokens: number): string =>
  tokens === 0 ? 'nothing to put back' : counted(tokens, 'name put back', 'names put back')

// The amber line over the output when the reply quotes pseudonyms this
// document never minted.
export const strayLine = (unknown: number): string => {
  if (unknown === 0) return ''
  if (unknown === 1) return '1 pseudonym this document never used: it stays as the model wrote it'
  return `${unknown} pseudonyms this document never used: they stay as the model wrote them`
}
```

- [ ] **Step 5: Run the tests, see them pass**, then the whole suite: `npx vitest run`.

- [ ] **Step 6: Verify and commit** (revert bundle churn from any build first): the three source files, message `manual: the console knows how to show a restore`

---

### Task 2: The Restore screen and styles, then the bundle

**Files:**
- Modify: `webui/ui/src/views/Redactor.tsx`
- Modify: `webui/ui/src/styles.css`
- Modify: `internal/webui/assets/**` (generated, final commit only)

**Interfaces:**
- Consumes from Task 1: everything listed there, plus existing `api`, `noticeBox`, `sessionPath`.

- [ ] **Step 1: Add mode and restore state to `Redactor.tsx`**:

```tsx
const [mode, setMode] = useState<'redact' | 'restore'>('redact')
const [reply, setReply] = useState('')
const [mappingFile, setMappingFile] = useState<{ name: string; raw: string; entries: RestoreEntry[] } | null>(null)
const [mappingProblem, setMappingProblem] = useState(false)
const [restored, setRestored] = useState<DocredactRestoreResponse | null>(null)
const [restoring, setRestoring] = useState(false)
```

Opening a new document keeps restore state as is, except `setRestored(null)`: the old result belonged to the old mapping.

- [ ] **Step 2: The switch**, rendered in both modes at the far left of the viewbar (and above the drop zone when no document is open in redact mode):

```tsx
const jobs = (
  <div className="jobs">
    <button aria-current={mode === 'redact'} onClick={() => setMode('redact')}>Redact</button>
    <button aria-current={mode === 'restore'} onClick={() => setMode('restore')}>Restore</button>
  </div>
)
```

The no-document redact screen becomes: a `viewbar` row holding `jobs` and a meta span "no document open", then the existing drop zone unchanged.

- [ ] **Step 3: The restore action**:

```tsx
// The server does the restoring; this screen only shows the answer. With a
// session open its live mapping is used. Without one, the saved mapping
// file's own bytes go along, parsed again server-side by the code that
// wrote them.
const runRestore = async () => {
  if (restoring || reply.trim() === '') return
  setRestoring(true)
  try {
    const body = doc
      ? await api<DocredactRestoreResponse>(sessionPath('/restore'), {
          method: 'POST', body: JSON.stringify({ text: reply }),
        })
      : await api<DocredactRestoreResponse>('/api/v1/docredact/restore', {
          method: 'POST', body: JSON.stringify({ text: reply, mapping: mappingFile?.raw ?? '' }),
        })
    setRestored(body)
  } catch (problem) {
    noticeBox('Could not restore that reply', message(problem))
  } finally {
    setRestoring(false)
  }
}
```

- [ ] **Step 4: The mapping drop** (restore mode, no document open only):

```tsx
const openMapping = async (file: File | null) => {
  if (!file) return
  const raw = await file.text()
  const entries = parseMappingFile(raw)
  if (entries === null) {
    setMappingFile(null)
    setMappingProblem(true)
    return
  }
  setMappingProblem(false)
  setMappingFile({ name: file.name, raw, entries })
  setRestored(null)
}
```

Rendered as (shown only in restore mode with no document and no accepted mapping file):

```tsx
{mappingProblem && (
  <div className="error-note" role="alert">
    <strong>That file is not a mapping</strong>
    <p>Use the .mapping.json file that the export saved. Nothing was read from this one.</p>
  </div>
)}
<label
  className="drop"
  onDragOver={event => event.preventDefault()}
  onDrop={event => {
    event.preventDefault()
    void openMapping(event.dataTransfer.files?.[0] ?? null)
  }}
>
  <b>Drop the mapping file here</b>
  <span className="sub">the .mapping.json saved next to your redacted copy · it never leaves this machine</span>
  <input
    type="file"
    accept=".json"
    hidden
    onChange={event => {
      void openMapping(event.target.files?.[0] ?? null)
      event.target.value = ''
    }}
  />
</label>
```

- [ ] **Step 5: The restore view** (rendered when `mode === 'restore'`):

```tsx
const entries = doc ? restoreEntriesFromFindings(findings) : (mappingFile?.entries ?? [])
const segments = restored === null ? null : restoreSegments(reply, entries)
// The server's text is the truth. Colors appear only when the client's
// segmentation reproduces it exactly.
const colorable = restored !== null && segments !== null && segmentsText(segments) === restored.text
const canRestore = !restoring && reply.trim() !== '' && (doc !== null || mappingFile !== null)

return (
  <div className="stack">
    <div className="viewbar">
      {jobs}
      <span className="meta">
        {doc ? `using this session's mapping · ${doc.name}`
          : mappingFile ? `using ${mappingFile.name}` : 'no document open'}
      </span>
      <div className="right">
        <button className="ghost" disabled={restored === null} onClick={copyRestored}>Copy restored text</button>
        <button className="primary" disabled={!canRestore} onClick={() => void runRestore()}>
          {restoring ? 'Restoring' : 'Restore'}
        </button>
      </div>
    </div>

    {!doc && !mappingFile && (/* the mapping drop from Step 4, plus the error note when mappingProblem */)}

    <div className="restore-cols">
      <section className="restore-col">
        <div className="restore-col-head"><h2>The cloud's reply</h2><span className="hint">Paste it here</span></div>
        <textarea
          className="restore-in"
          value={reply}
          placeholder="Paste the reply that quotes [PERSON_1]-style pseudonyms."
          onChange={event => { setReply(event.target.value); setRestored(null) }}
        />
      </section>
      <section className="restore-col">
        <div className="restore-col-head">
          <h2>What it really says</h2>
          {restored !== null && <span className="hint">{restoredHint(restored.tokens)}</span>}
        </div>
        {restored !== null && restored.unknown.length > 0 && (
          <div className="passline"><span className="claims">{strayLine(restored.unknown.length)}</span></div>
        )}
        {restored === null
          ? <div className="restore-out faint">Restored text appears here, on this screen only. It is never written to a file.</div>
          : colorable
            ? <div className="restore-out">{segments!.map((segment, index) =>
                segment.kind === 'restored' ? <span key={index} className="back">{segment.text}</span>
                : segment.kind === 'stray' ? <span key={index} className="stray">{segment.text}</span>
                : <Fragment key={index}>{segment.text}</Fragment>)}</div>
            : <div className="restore-out">{restored.text}</div>}
      </section>
    </div>
  </div>
)
```

`copyRestored` writes `restored.text` with `navigator.clipboard.writeText` and falls back to a hidden textarea plus `document.execCommand('copy')` when the clipboard API is missing (the console is also reached over plain http on the LAN, where `navigator.clipboard` does not exist).

- [ ] **Step 6: Styles** in `styles.css`, with the other redactor styles, copied from the approved mockup:

```css
/* The tab's two jobs. Redact is the existing flow; Restore is the reverse
   trip. One switch, far left of the viewbar, in both modes. */
.jobs { display: flex; gap: 2px; border: 1px solid var(--line); border-radius: 999px; padding: 2px; width: max-content; background: var(--surface); flex: none; }
.jobs button { border: 0; background: none; color: var(--muted); border-radius: 999px; padding: 4px 14px; font-size: 12.5px; font-weight: 500; }
.jobs button[aria-current='true'] { background: var(--surface-2); color: var(--ink); font-weight: 550; }

/* Two panes: the reply on the left, what it really says on the right.
   Restored literals wear a quiet green tint, the one place the console's
   green appears off the serving light, so the eye can audit what came
   back. A pseudonym the mapping never minted stays amber: the cloud model
   invented it, nothing failed. */
.restore-cols { display: grid; grid-template-columns: 1fr 1fr; border: 1px solid var(--line); border-radius: var(--radius); overflow: hidden; background: var(--surface); }
.restore-col { display: flex; flex-direction: column; min-height: 300px; }
.restore-col + .restore-col { border-left: 1px solid var(--line); }
.restore-col-head { display: flex; align-items: baseline; gap: 8px; padding: 12px 14px; border-bottom: 1px solid var(--line); }
.restore-col-head h2 { font-size: 13px; }
.restore-col-head .hint { margin-left: auto; color: var(--faint); font-size: 11.5px; }
.restore-in { flex: 1; border: 0; background: none; color: var(--ink); font: 13px/1.75 var(--sans); padding: 16px 20px; resize: none; outline: none; }
.restore-in::placeholder { color: var(--faint); }
.restore-out { flex: 1; padding: 16px 20px; font-size: 13px; line-height: 1.75; white-space: pre-wrap; overflow-y: auto; max-height: 560px; }
.restore-out .back { background: color-mix(in srgb, var(--live) 12%, transparent); border-radius: 3px; padding: 0 2px; }
.restore-out .stray { font-family: var(--mono); font-size: 11.5px; color: var(--warn); background: color-mix(in srgb, var(--warn) 8%, transparent); border: 1px solid color-mix(in srgb, var(--warn) 25%, transparent); border-radius: 4px; padding: 0 5px; white-space: nowrap; }

@media (max-width: 980px) {
  .restore-cols { grid-template-columns: 1fr; }
  .restore-col + .restore-col { border-left: 0; border-top: 1px solid var(--line); }
}
```

- [ ] **Step 7: Verify**: `cd webui/ui && npm test && npm run build`, then repo root `go build ./... && go vet ./... && go test ./...`.

- [ ] **Step 8: Commit in two commits**: source first (revert the bundle churn before committing), message `manual: the redactor tab has a restore screen`; then rebuild and commit only `internal/webui/assets`, message `manual: rebuild the console bundle for the restore screen`.
