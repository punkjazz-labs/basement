# Spec 02: concurrent installs

Branch `spec/02-concurrent-installs`. Depends on nothing; do after 01 so the executor
is calibrated.

## Current state (verified)

The engine already permits concurrency structurally: jobs lock per recipe
(`recipeLock`), and only steps in `runtimeOperations` (container/GPU work) take the
machine-wide one-slot semaphore (`acquireRuntime`). Two installs of different recipes
can therefore download in parallel today; only their final load-into-memory phases
serialize. What is missing is safety and UI.

## Gap 1: joint disk reservation

`verify_disk` checks each job against free disk in isolation. Two installs can each
pass preflight and jointly overflow the disk mid-download.

**Change.**
1. Add a reservation registry to the engine: `map[jobID]int64` guarded by the existing
   mutex. On job start (install kind only), reserve the recipe's remaining footprint:
   `required_bytes` is the conservative choice; use it. Release on any terminal state
   (the existing deferred cleanup path in `run`).
2. Pass the sum of *other* jobs' reservations into the execution context
   (`operations.Execution` gains `ReservedBytes int64`), and `verify_disk` subtracts it
   from available space before comparing.
3. The error when it fails must name the cause so the UI can show it verbatim:
   `not enough free space while another install is running, so wait for it to finish or
   free up space`.

**Constraints.** No global lock around downloads; reservation bookkeeping only. Do not
change `verify_disk` semantics for the single-job case.

## Gap 2: console allows queueing a second install

**Investigate.** Check whether `Models.tsx` disables Install while a job is running
(look for `working` from `App.tsx` or per-row disabling).

**Change.** Installing recipe B while recipe A installs is allowed. The per-recipe
guard stays: a recipe with its own non-terminal job shows its progress state, not a
second Install button. If a global disable exists, remove it for install actions only;
runtime actions (Start, Stop, Switch to) keep their current guards.

Add one line of copy to the install dialog when another install is running:
`Another download is running. Both continue, sharing bandwidth.`

## Non-goals

- No parallel starts. The runtime semaphore stays one-slot.
- No download scheduler or bandwidth shaping.
- No change to the Activity view; it already renders multiple running jobs and the
  sidebar badge counts them.

## Acceptance

- Unit test: two fake install jobs, second `verify_disk` sees reduced availability;
  reservation released on terminal state.
- Unit test: reservation registry never counts the requesting job's own bytes.
- Mock harness: fixture with one running install; screenshot shows a second recipe
  still installable and the added dialog copy.
- `go build ./... && go vet ./... && go test ./...` green.
