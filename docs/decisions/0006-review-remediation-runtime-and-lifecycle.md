# ADR 0006: Review remediation — runtime exposure, IPC, cancellation, and removal safety

Status: accepted for local implementation; behavior awaiting DGX Spark qualification

## Context

A full code review (2026-07-31) of the candidate release surfaced defects that
were invisible to local tests: a systemd sandbox directive that would block GPU
detection on real hardware, unconditional LAN exposure of the unauthenticated
model endpoint, a hardcoded host IPC namespace that silently disabled the
recipes' `shm_bytes` field, a cancellation race during transactional switches,
a global job mutex that let long downloads block unrelated stop requests, and
removal logic that never actually checked artifact sharing.

## Decisions

1. **GPU visibility**: `PrivateDevices=yes` is removed from the systemd unit.
   Inventory shells out to `nvidia-smi`, which requires the real `/dev/nvidia*`
   nodes. Container GPU access continues to flow through the NVIDIA container
   runtime and is unaffected.
2. **Model endpoint exposure** (resolves part of PRD §18's binding question):
   the model container port is published on the same interface the manager
   listens on, derived from `--listen`. Loopback stays the default; exposing
   the unauthenticated OpenAI endpoint to the LAN is now the same deliberate
   operator action as exposing the manager. An unparseable or hostname listen
   address falls back to loopback. The manager's own health/inference probes
   follow the published address.
3. **IPC isolation**: containers no longer run with `IpcMode: host`. `/dev/shm`
   is sized explicitly from the recipe's validated `shm_bytes`, making the
   field real instead of decorative. Qwen 27B, which declared `shm_bytes: 0`
   under the old host namespace, now declares the same 32 GiB as its siblings
   (version bump v2→v3).
4. **Cancellation**: `Cancel` no longer writes the terminal `cancelled` state
   while the job goroutine is running. It records a non-terminal `cancelling`
   state (guarded so it can never overwrite a terminal state) and the running
   goroutine — which may still perform a full switch rollback — remains the
   only writer of the final state. SSE streams therefore stay open until the
   true outcome, including rollback results, is persisted. After a manager
   restart an in-flight `cancelling` job is recovered as `interrupted` and
   re-run to a safe completion; the cancellation intent is not replayed.
5. **Job concurrency**: the single engine-wide mutex is replaced by a
   per-recipe lock plus one shared runtime lock taken only for operations that
   touch containers, live memory, or the active slot. Long downloads and
   preflight no longer block stopping an unrelated running model. Switch
   rollback always executes while the runtime lock is held.
6. **Removal safety**: `remove_artifact_if_unshared` now receives the set of
   artifact identities (repository@revision) and artifact paths referenced by
   every other installed model, retains shared data, and reports retained
   bytes in the receipt. Activation after install/start is a single
   transaction (`ActivateExclusively`), so two models can never both persist as
   active.
7. **Licence links**: a recipe artifact's `licence_url` must reference the
   artifact's own repository (validator-enforced). The Laguna drafter
   previously linked the base model's licence file; it now links the drafter's
   own pinned tree (version bump v2→v3). Upstream ships no licence file in the
   drafter repository at the pinned revision — the family licence text lives in
   the base repository; revisit with poolside evidence during qualification.

## Consequences

- All three embedded recipes changed version and require fresh DGX
  qualification (Qwen 35B stays v3; Qwen 27B v3; Laguna v3).
- Copying the endpoint from another device requires the operator to have bound
  the manager (and therefore the model port) to a reachable interface.
- The `cancelling` and `rolling_back` states are part of the public job state
  machine and are rendered by the console.
