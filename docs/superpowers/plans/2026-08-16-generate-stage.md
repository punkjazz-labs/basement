# Generate Stage Rebuild Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the Generate screen so the selected video is the hero (stage + run strip), fix video playback in Safari, add prompt reuse, a done sound with tab-title flash, Cmd+Enter submit, and plain-words wait costs from measured data.

**Architecture:** The server changes only to carry a new optional per-size wait factor in the recipe's media generation config. Everything else is console work: pure helpers in `webui/ui/src/generation.ts` (tested with vitest), a new poster-capture module, and a rebuilt `Generate.tsx` view. The approved mockup is the design authority: https://claude.ai/code/artifact/c4ead3ea-d24f-4247-9209-e380262cd9ea (file `generate-reuse-mockup.html` in the session scratchpad).

**Tech Stack:** Go (recipe schema + httpapi response), React + TypeScript (vite, vitest), CSS in `webui/ui/src/styles.css`.

**Spec:** The approved mockup named above, plus this plan's task text. There is no separate spec document; conflicts resolve against the mockup's visible behavior and the Global Constraints below.

## Global Constraints

- All user-visible copy in ASD-STE100 Simplified Technical English. No em dashes anywhere (docs, code comments, UI copy).
- Copy strings are exact where this plan quotes them.
- Preserve every existing Generate behavior not named as changed: SSE stream with polling fallback, queue display, cancel, delete with confirmBox, prompt rune counting and 75% counter reveal, no maxLength on the textarea, seed validation message, idempotency header, authenticated blob playback, download link, error notes with role="alert".
- The server keeps serving generation files as `application/octet-stream` with attachment disposition and nosniff. Do not change `serveGenerationFile`.
- Only palette tokens from `webui/ui/src/styles.css` `:root`. No new colors.
- Color roles are strict: orange = primary actions, NVIDIA green = serving/completed-live states as already used, amber = warning/running, red = failure.
- Verification before every commit: `cd webui/ui && npm test && npm run build` for console tasks; `go build ./... && go vet ./... && go test ./...` for Go tasks. The embedded bundle (`internal/webui/assets`) is rebuilt only in the final task.
- Commits: `manual: <plain lowercase sentence>` with trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- New localStorage keys are namespaced `basement.generate.*`.

---

### Task 1: Safari playback fix

**Files:**
- Modify: `webui/ui/src/generation.ts` (add helper)
- Modify: `webui/ui/src/views/Generate.tsx` (use helper in `GenerationVideo`)
- Test: `webui/ui/src/generation.test.ts`

**Interfaces:**
- Produces: `export const playableVideoBlob = (blob: Blob): Blob`

**Why:** `apiBlob` returns the server's `application/octet-stream` type. `URL.createObjectURL` keeps that type and Safari refuses to play a video whose source type is not a video type. Re-labeling the blob client-side keeps the server's security posture untouched.

- [ ] **Step 1: Write the failing tests** in `generation.test.ts`:

```ts
describe('playableVideoBlob', () => {
  it('re-labels an octet-stream blob as video/mp4', () => {
    const blob = new Blob(['x'], { type: 'application/octet-stream' })
    expect(playableVideoBlob(blob).type).toBe('video/mp4')
  })
  it('keeps a blob that already carries a video type', () => {
    const blob = new Blob(['x'], { type: 'video/mp4' })
    expect(playableVideoBlob(blob)).toBe(blob)
  })
})
```

- [ ] **Step 2: Run to verify failure**: `cd webui/ui && npx vitest run src/generation.test.ts` fails with `playableVideoBlob is not a function`.

- [ ] **Step 3: Implement** in `generation.ts`:

```ts
// The file endpoint deliberately serves application/octet-stream so nothing
// from it can render in the page's origin. Safari refuses to play a blob
// with that type, so the console re-labels its own copy before playback.
export const playableVideoBlob = (blob: Blob): Blob =>
  blob.type.startsWith('video/') ? blob : new Blob([blob], { type: 'video/mp4' })
```

- [ ] **Step 4: Wire it** in `Generate.tsx` `GenerationVideo`: `apiBlob(...).then(blob => { objectURL = URL.createObjectURL(playableVideoBlob(blob)) ... })` (keep the existing mounted/revoke logic exactly).

- [ ] **Step 5: Verify**: `npm test && npm run build`. **Commit** `manual: label downloaded videos as video mp4 so safari plays them`.

---

### Task 2: measured wait factors in the recipe

**Files:**
- Modify: `internal/recipe/recipe.go` (or wherever the comfyui service config struct lives; find the struct that yields the `media_generation` JSON the console reads, it carries `default_short_edge`, `frame_block`, `sampler_steps`)
- Modify: `internal/recipe/validate.go` (same package validation path that already checks graphs and canvas fields)
- Modify: `internal/recipe/recipes/minimax-h3-comfyui-1s.yaml`
- Test: the existing recipe/validator test files in `internal/recipe/`

**Interfaces:**
- Produces: JSON field `size_waits` inside `media_generation`: an array of `{ "short_edge": number, "factor": number }`. Optional; may be absent or empty.

- [ ] **Step 1: Write failing tests**: a recipe with `size_waits: [{short_edge: 768, factor: 1}, {short_edge: 1088, factor: 2.85}]` round-trips through load and appears in the served config; validation rejects a factor below 1, a non-positive short edge, and a duplicate short edge. Use the existing table-test style of the package.

- [ ] **Step 2: Run to verify failure**: `go test ./internal/recipe/`.

- [ ] **Step 3: Implement**: add the struct field with yaml/json tags `size_waits`, entries `short_edge` / `factor`. Validation: factor >= 1, short_edge > 0, no duplicate short edges. Absent stays absent (omitempty on the slice).

- [ ] **Step 4: Fill the H3 recipe** from `docs/H3-MEASUREMENTS.md` (5.17 s clip, 20 steps): 1061 s at 768, 3021 s at 1088, 7750 s at 1440. Append to the `comfyui:` block with this comment style:

```yaml
    # Wall times measured in docs/H3-MEASUREMENTS.md for a 5.17 s clip:
    # 1061 s at 768, 3021 s at 1088, 7750 s at 1440. Factors are those
    # times divided by the smallest one.
    size_waits:
      - short_edge: 768
        factor: 1
      - short_edge: 1088
        factor: 2.85
      - short_edge: 1440
        factor: 7.3
```

Bump `version: 2` to `version: 3` and add one comment line: `# Version 3 adds the measured size wait factors for the console.`

- [ ] **Step 5: Verify**: `go build ./... && go vet ./... && go test ./...`. **Commit** `manual: recipes can carry measured wait factors per canvas size`.

---

### Task 3: console helpers for waits, arithmetic, and reuse

**Files:**
- Modify: `webui/ui/src/api.ts` (extend the media generation type with `size_waits?: { short_edge: number; factor: number }[]`)
- Modify: `webui/ui/src/generation.ts`
- Test: `webui/ui/src/generation.test.ts`

**Interfaces:**
- Consumes: the existing `canvasSizes`, `durationOptions`, `canvasShapes` helpers and the media generation config type (call it `MediaGeneration` below; use its real exported name).
- Produces:
  - `export function sizeWaitLabel(config: MediaGeneration, shortEdge: number): string` returns `'shortest wait'` for the entry with factor 1 (or the smallest factor), `about ${n}× the wait` with n = `Math.round(factor)` for others, `''` when `size_waits` is absent or has no entry for that edge.
  - `export function sizeWaitHint(config: MediaGeneration): string` returns `'Waits measured on this model for a 5 second clip. A longer clip waits more.'` when `size_waits` has entries, else `'A bigger size waits much longer. The largest size can take hours.'`
  - `export function durationArithmetic(config: MediaGeneration, blocks: number): string` returns `= ${frames} frames at ${config.frames_per_second} fps` where frames = `config.frame_block * blocks + config.frame_offset`.
  - `export interface ReuseValues { prompt: string; shape: CanvasShape; shortEdge: number; blocks: number }`
  - `export function reuseValues(generation: Generation, config: MediaGeneration): ReuseValues` derives shape from width vs height (wider = horizontal, taller = vertical, equal = square), picks the offered size whose width and height match the generation exactly for that shape (fall back to the default tier when no exact match), and the duration option whose frames match `generation.frames` (fall back to `default_blocks`). Never returns the seed: a reuse is a new take.

- [ ] **Step 1: Write failing tests** covering: wait label for factor 1, rounded factor (2.85 gives `about 3× the wait`), absent `size_waits` gives `''` and the fallback hint; arithmetic string for 7 blocks of the H3 grid (`= 124 frames at 24 fps`); reuse for an exact HD horizontal match, a vertical match, a generation whose size is no longer offered (falls back to default tier), and a frame count off the current grid (falls back to `default_blocks`).

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement** the four exports plus the api.ts type extension.

- [ ] **Step 4: Verify**: `npm test && npm run build`. **Commit** `manual: helpers that say waits in plain words and refill the form from a result`.

---

### Task 4: poster capture and cache

**Files:**
- Create: `webui/ui/src/posters.ts`
- Test: `webui/ui/src/posters.test.ts`

**Interfaces:**
- Produces:
  - `export function cachedPoster(id: string): string | null` returns the stored data URI or null.
  - `export function storePoster(id: string, dataURI: string, store?: Storage): void` stores under key `basement.generate.poster.${id}`; when storage throws (quota), evict oldest entries by the companion index key `basement.generate.posters` (a JSON array of ids, oldest first) until the write succeeds or the index is empty, then give up silently. Cap the index at 200 ids; evict beyond that.
  - `export function forgetPoster(id: string, store?: Storage): void` for deletes.
  - `export function capturePoster(videoURL: string): Promise<string>` creates an off-screen `<video muted playsinline preload="auto">`, seeks to 0, waits for a decodable frame (`requestVideoFrameCallback` when present, else the `loadeddata` event), draws to a canvas scaled to 128 px wide (height from the video's aspect), and resolves `canvas.toDataURL('image/jpeg', 0.7)`. Rejects on video `error`. Always removes its listeners and releases the element.

- [ ] **Step 1: Write failing tests** for the cache functions only (capture touches media APIs jsdom lacks): store and read back, index ordering (oldest evicted first when a quota error is simulated with a throwing Storage stub), the 200-id cap, and forget removing both entry and index membership. Inject a fake `Storage` object; default to `window.localStorage` when not provided.

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement.** `capturePoster` is excluded from coverage assertions; keep it small and free of module state.

- [ ] **Step 4: Verify**: `npm test && npm run build`. **Commit** `manual: capture and cache one poster frame per finished run`.

---

### Task 5: the stage screen

**Files:**
- Modify: `webui/ui/src/views/Generate.tsx`
- Modify: `webui/ui/src/styles.css`
- Test: `webui/ui/src/generation.test.ts` (only if new pure helpers emerge; extract them to `generation.ts` when they do)

**Interfaces:**
- Consumes: everything produced by Tasks 1, 3, 4.

Rebuild the view to the mockup's structure. Layout: `.gen-layout` becomes `360px minmax(0,1fr)` (single column under 900 px, form first). Left card keeps the model context header and the form: mode pills, prompt textarea (with the existing counter rules), Shape pills, Size options (each a two-line option: size on top, wait label under it in `--faint` 10.5px), the size hint line from `sizeWaitHint`, Duration pills with the `durationArithmetic` line under them, Seed field, then the footer: a full-width primary Generate button showing the shortcut glyph `⌘⏎` in a bordered `.kbd` span, and under it the existing queue sentence.

Right card, top to bottom:
1. Header row: `Results` h2, the sound toggle (label text `Sound when a run finishes`), spacer, note `Played from local disk only`.
2. The stage: when a completed run is selected, the existing `GenerationVideo` player at full card width. When the selected run is running or queued, the poster area shows the existing progress presentation (`GenerationProgress` or the queue line) inside a 16:9 `.stage-empty` panel. When there are no runs at all, the panel says `No generations yet. Describe a clip and generate it.`
3. Under the stage: one meta row (state chip, `width × height`, duration string, `Seed n`, `Generated in mm:ss` when finished), then the full prompt as wrapping text (not truncated), then the actions row: `Download` (completed only), `Use this prompt` (completed and failed; accent-bordered quiet button), `Cancel` (active only), `Delete` (terminal only). The stage shows `generation.error` for failed runs with the existing role="alert" rule.
4. The strip: label row `All runs · newest first`, then a horizontal `overflow-x: auto` row of fixed-width (128 px) cards, newest first, every run included. Card content: poster image when `cachedPoster(id)` has one, else a `.thumb-blank` panel with the state chip; under it the state chip (running cards add a 3 px amber progress bar bound to `progress_value/progress_max`), the first words of the prompt (ellipsized single line), and the finished time when completed. Clicking a card selects it into the stage. The selected card gets the `sel` border treatment from the mockup.

Behavior wiring:
- Selection: reuse the existing `selectedVideoID` state but rename to `stagedID` and let it hold any run (not only completed). Keep the existing auto-select of the newest completed run only when nothing valid is staged; a newly completed run that IS the staged run refreshes in place.
- Posters: an effect watches for completed runs without a cached poster; for at most one run at a time it fetches the file with `apiBlob`, makes an object URL from `playableVideoBlob`, calls `capturePoster`, stores via `storePoster`, revokes the URL. Failures mark the id in a session-local `Set` so the console does not retry in a loop. Delete calls `forgetPoster`.
- Reuse: `Use this prompt` calls `reuseValues` and sets prompt, shape, shortEdge, blocks; seed is cleared. Show under the textarea, until the next edit or submit: `Filled from the staged result. The seed is empty: this run will be a new take.` in `--accent-deep` 12px with a small info role, dismissed on any prompt/shape/size/duration change.
- Sound toggle: persisted at `basement.generate.sound` (`'on'`/`'off'`, default off). When on and a run transitions into `completed` or `failed` while `document.hidden`, play a short two-tone WebAudio chime (oscillator, ~0.18 s, gain 0.08, e.g. 880 Hz then 1175 Hz) built inline; no audio asset files. Guard AudioContext creation behind the first user gesture (create lazily inside the toggle handler and keep the instance).
- Title flash: while any finished-unseen run exists and the tab is hidden, alternate `document.title` between the original and `● Done · basement` every second; restore the original title and clear the unseen mark on `visibilitychange` to visible. Track the original title once at module scope.
- Keyboard: `Cmd+Enter` (and `Ctrl+Enter`) inside the prompt textarea submits when `canSubmit`.
- Transition detection for sound/flash: compare previous statuses by id in a ref updated on every generations state change; the SSE handler and the poller both flow through the same state setter, so hook the comparison into a `useEffect` on `generations`.

CSS: add `.stage`, `.stage-empty`, `.strip`, `.thumb`, `.thumb-blank`, `.kbd`, `.sound-toggle`, `.filled-note`, two-line size options, and the 900 px breakpoint; remove styles that only served the old card list (`.gen-result*` rules that no longer match) in the same commit so dead rules do not accumulate. Follow the mockup's spacing and the palette tokens exactly; the mockup file is in the session scratchpad as `generate-reuse-mockup.html`.

- [ ] **Step 1: Extract any new pure logic** (unseen-set update, transition detection given two status maps, strip ordering) into `generation.ts` with tests written first.
- [ ] **Step 2: Rebuild the view and styles.**
- [ ] **Step 3: Verify**: `cd webui/ui && npm test && npm run build`. Walk the built UI mentally against the mockup: stage, strip, reuse note, toggle, arithmetic line, wait labels.
- [ ] **Step 4: Commit** `manual: the generate screen stages the video and lines up the runs`.

---

### Task 6: embedded bundle

**Files:**
- Modify: `internal/webui/assets/*` (build output)

- [ ] **Step 1:** `cd webui/ui && npm run build` with the repo's documented bundle-refresh flow (same as previous bundle commits: copy `dist` into `internal/webui/assets`).
- [ ] **Step 2:** `go build ./... && go vet ./... && go test ./...` from the repo root.
- [ ] **Step 3: Commit** `manual: rebuild the console bundle for the generate stage`.
