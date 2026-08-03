# Executor conventions

Read this before touching any file. These rules are not style preferences; several encode
product promises. A change that violates them will be rejected in review even if it works.

## What this project is

basement is a local-first Go manager for GB10 machines (NVIDIA DGX Spark, MSI
EdgeXpert). It installs curated vLLM model recipes and serves an embedded React console.
The product promise is trust: pinned versions, verified installs, honest copy, receipts
for everything. Users include non-experts; the console must read like a considered
appliance, not a devops tool.

The module path and repository are still `github.com/punkjazz-labs/basement`
— they rename together with the GitHub repository itself (the owner's action, not an
executor's); only the product name, binaries, and everything users see are `basement`
(see docs/plans/10-rename-basement.md).

## Layout

- `cmd/basement/` CLI entry
- `internal/engine/` job engine: planning, execution, rollback, per-recipe locks, a
  one-slot runtime semaphore for GPU-touching steps
- `internal/operations/` step executors (verify_*, download, container lifecycle)
- `internal/httpapi/server.go` REST API + SSE job events
- `internal/recipe/` recipe schema (`types.go`) and embedded recipes (`recipes/*.yaml`)
- `internal/store/` SQLite persistence (jobs, models, keys)
- `webui/ui/` React 19 + TypeScript + Vite console; build output is committed into
  `internal/webui/assets` and embedded via go:embed
- `docs/adr/` architecture decision records; read the relevant ones before changing
  engine or operations behavior

## Build and verify (run all before every commit)

```
cd webui/ui && npx tsc --noEmit && npm run build   # builds into internal/webui/assets
cd ../.. && go build ./... && go vet ./... && go test ./...
```

UI changes must additionally be verified against the Playwright mock harness pattern:
a small node script that serves `internal/webui/assets` plus mocked `/api/v1/*` fixtures,
drives the page, and screenshots it. See the acceptance section of each spec. Convert
PNGs with `sips -s format jpeg` before visually inspecting if your tooling needs it.

## Git

- One branch per spec: `spec/NN-short-name`. Commit messages: `manual: NN: what
  changed` in plain lowercase, e.g. `manual: 01: per-recipe start timeout replaces
  hardcoded 20 minutes`. The `manual:` prefix satisfies the repo's commit-msg hook.
- Never push to main. Never touch `.github/`, `LICENSE`, or repo settings.
- Do not commit generated screenshots or scratch scripts.

## Hard product rules

1. **No invented facts.** Any user-visible factual claim (who made a model, who wrote a
   recipe, licences, dates, speeds) comes from recipe data or is `n/a`. Never guess,
   never extrapolate, never fill placeholders with plausible values. If a spec needs a
   factual value that is missing, leave `n/a` and flag it in your report.
2. **Weights are never modified.** Artifacts are pinned upstream revisions. Nothing in
   any change may patch, convert, or re-quantize model files.
3. **Pinning is sacred.** Container images by sha256 digest, artifacts by revision,
   source repos by commit. Never introduce a floating tag or `latest`.
4. **Quantization lines state the format only** (NVFP4, FP8, BF16). Who built the
   quantized weights is deliberately not shown.
5. **Safety machinery is not optional.** Preflight verifications, health gates, the
   inference smoke test, receipts, and rollback paths must not be weakened to make a
   feature easier.

## Console design system

- Dense Linear/Vercel-style table with row expansion; first-run hero. Dark theme.
- One pill button family, radius 999px: `primary` (NVIDIA green `#76b900`, dark ink,
  one per row/dialog), `ghost`, `danger`, `quiet`. Never mix radii in one cluster.
  Never add a second primary to a cluster.
- No emoji anywhere. No em dashes in UI copy; use `n/a` for absent values.
- Copy voice: sentence case, plain verbs, controls say exactly what happens
  ("Uninstall", not "Remove model files maybe"). Errors say what went wrong and what to
  do next, no apologies. Numbers are honest; when a number is an estimate, say so.
- Markdown in chat is rendered via `marked` + `DOMPurify.sanitize`; keep that pattern.
- Every new visual concept (not incremental edits) requires a static mockup approved by
  the owner before implementation. The specs mark which items are mockup-gated.

## Code style

- Match the surrounding idiom exactly: naming, error wording (lowercase, actionable),
  comment density. Comments state constraints the code cannot show; never narrate what
  the next line does and never reference the change process ("now we", "fixed").
- Frontend: functional components, hooks, no new dependencies without the spec saying
  so. Backend: standard library first; the project has no DI framework, keep it that way.
- API errors use the existing `writeError(w, code, err)` helper; 409 for conflicts with
  a sentence the UI can show verbatim.

## Reporting

After each spec, produce: branch name, commands run with results, screenshots for UI
work, and a list of any deviations from the spec with reasons. Unresolved questions go
in the report, not in guessed code.
