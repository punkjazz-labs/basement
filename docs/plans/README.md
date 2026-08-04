# Spec pack

Specs for delegating implementation to an executor model while Claude reviews diffs.

Execution order and why:

1. **01 quick wins** — three small independent changes; calibrates the executor before
   trusting it with anything structural. Judge the diffs here first.
2. **02 concurrent installs** — bounded engine work (reservation bookkeeping) with
   clear tests.
3. **03 fleet phase 1** — the strategic priority: read-only multi-Spark view plus the
   hardware runbook that prepares real multi-node testing.
4. **04 remote recipe index** — security-critical; assign only after 01–03 diffs proved
   trustworthy, and review this one line by line.
5. **05 memory calculator** — data layer only; UI waits for an approved mockup.
6. **07 installers** — packaging only (release script, macOS/Windows double-click
   artifacts); independent of 02–05 and safe to run in parallel with them.

Specs 08 through 10 (native installers, update affordance, the basement rename) and
`multispark-v1.md` were written and executed after that list and are recorded in git
rather than re-ordered here.

## The remaining roadmap, 12 to 17

Written as pick-up-cold specs: each states its problem, the outcome a user sees, a design
grounded in named files, a build plan in reviewable chunks, a test strategy with the
hardware seams stubbed, and open questions marked for the owner. Nothing here is started.

1. **13 multi-model router** — six phases from per-model ports to concurrent serving to a
   fleet-wide `/v1`. The largest item and the one every other roadmap entry gets easier
   after. Phase C needs a recipe-level co-residency contract and new hardware
   qualification; phase E reverses a consequence of ADR 0013 and needs a new ADR first.
2. **14 release notes** — what changed, in the console, sourced from release bodies a
   human writes at tag time. Small, and it makes 16 explainable.
3. **15 signed recipe feed** — ADR 0009's remaining four pieces: real keys with rotation,
   the trust clamp, feed state the owner can see, and a verifier. The fetch chain itself
   already shipped as spec 04.
4. **16 manager self-update** — release signing, a root oneshot triggered by a systemd
   path unit, versioned slots, rollback on a failed health check. Needs 14 for the copy
   and a key-ceremony ADR shared with 15.
5. **17 meeting notes** — a fully local Granola replacement. Its own repository, running
   on the owner's Mac, capturing two audio tracks and transcribing locally, writing the
   notes through basement's `/v1`. Independent of everything else here.
6. **12 doc redactor** — its own repository too: find sensitive spans with a local model,
   let the owner review each one, export a copy that is safe to upload. Independent.

Suggested order: 14, then 15A and 16A together (they share one key ceremony ADR), then
16B, then 13 phase by phase. 12 and 17 are separate products and can run in parallel with
all of it, by whoever is not holding the manager.

`00-conventions.md` is mandatory reading for the executor before any spec. Prompt
skeleton for each assignment:

> Read docs/plans/00-conventions.md fully, then docs/plans/NN-*.md. Work on branch
> spec/NN-... . Follow the spec exactly; where it says investigate, investigate and
> record findings. Do not exceed scope. Finish with the report format from the
> conventions.

Review gates (Claude, per spec): diff read, conventions violations, acceptance
artifacts present, hard-rule check (facts, pinning, safety machinery). Specs touching
`internal/engine` or auth (02, 03, 04) get a line-by-line review; UI-only diffs get a
screenshot review plus targeted reads.

`06-deferred.md` lists everything intentionally not specced. The public website is a
separate exploration: `docs/website/DIRECTION.md`.
