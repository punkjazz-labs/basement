# Spec 01: quick wins

Three small, independent changes. Do them in order; commit each separately on
`spec/01-quick-wins`.

## A. Per-recipe start timeout

**Problem.** `internal/operations/host.go` `waitHTTP` hardcodes a 20 minute deadline
(`deadline := time.Now().Add(20 * time.Minute)`), and the console copy hardcodes the
same claim (`FIRST_START_NOTE` in `webui/ui/src/views/Deployment.tsx`, plus similar
wording around line 401 of `webui/ui/src/views/Models.tsx`). A future large model that
legitimately needs longer will fail at exactly 20:00, and the copy will have promised
otherwise.

**Change.**
1. `internal/recipe/types.go`: add to the runtime section
   `StartTimeoutMinutes int` with yaml/json tag `start_timeout_minutes` and a comment
   stating the constraint: the health wait gives up after this long, and the console
   copy derives from it; 0 means the default of 20.
2. `waitHTTP` reads the recipe value, falling back to 20 when 0 or negative.
3. Expose the effective value in the recipe JSON the API already returns (it will flow
   automatically if the field is on the struct; verify).
4. `Deployment.tsx`: derive the note from the active recipe instead of the constant.
   Copy, exactly: `Loading the model into memory. The first start can take up to
   {N} minutes, with live progress the whole way. Later starts are much faster.`
5. `Models.tsx` install explainer: same derivation, wording adjusted to fit the
   sentence already there; keep "with live progress the whole way".
6. Do not add the field to the three existing recipe yamls; they use the default.

**Acceptance.** Unit test for the fallback (0 → 20, 45 → 45). `go test ./...` green.
Mock-harness screenshot showing the deployment dialog with the derived copy.

## B. Install choice: download only vs switch now

**Problem.** Installing a model while another is serving implicitly ends with a swap.
Users should choose that consciously before the work starts.

**Investigate first.** Find where the console starts an install (the Install action in
`Models.tsx`) and what the API accepts (`POST` in `internal/httpapi/server.go`, job kind
`install`), and how the engine plan decides to run the switch/start steps
(`internal/engine/engine.go` `plan`, `BeginSwitch`). Write down the actual flow in your
report before coding.

**Change.**
1. API: the install request accepts `"activate": bool` (default true, preserving
   current behavior). The engine plan for `activate: false` ends after artifacts and
   image are downloaded and verified; no container creation, no switch, model state
   ends `stopped`/inactive, job state `ready`.
2. Console: when the user clicks Install and another model is currently serving, the
   existing confirm dialog gains a choice (two stacked options, radio style, first
   selected):
   - `Download and switch now` with the consequence line
     `This stops {ActiveModelName} while {NewModelName} starts.`
   - `Download only` with the line `{ActiveModelName} keeps serving. Start
     {NewModelName} later from the Models tab.`
   When nothing is serving, the dialog is unchanged.
3. The dialog's single primary button stays the green pill; its label stays `Install`.

**Constraints.** Do not touch the rollback path. `activate: false` must never acquire
the runtime semaphore. Respect rule 5 of the conventions: all verify steps that protect
a download (disk, artifact access, docker) still run.

**Acceptance.** Unit test: plan for `activate: false` contains no container/switch
operations. Mock-harness screenshots of both dialog states. `go test ./...` green.

## C. Release dates in the model card

**Problem.** Cards show no dates. Users asked when a model came out and when its recipe
last changed.

**Change.**
1. `internal/recipe/types.go`: add optional `ModelReleased string` tag `model_released`
   (display string, e.g. `May 2026`) near `ModelBy`, with a comment that the value is
   researched by maintainers and absent means unknown.
2. `webui/ui/src/api.ts`: mirror the optional field.
3. `Models.tsx` facts grid: add `<dt>Released</dt>` showing `recipe.model_released ||
   'n/a'`, placed after `Model by`. Also add `<dt>Recipe version</dt>` showing
   `v{recipe.version}` if the facts grid does not already show it.
4. **Do not fill in any dates.** Leave all three recipes untouched; maintainers add the
   researched values separately. The UI must render `n/a` cleanly.

**Acceptance.** Mock-harness screenshot of the expanded card with a fixture that has
`model_released` set and one without (renders `n/a`). Type check green.
