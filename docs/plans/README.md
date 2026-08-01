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
