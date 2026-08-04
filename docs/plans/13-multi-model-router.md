# Spec 13: concurrent models and a fleet-wide /v1 router

Six phases, each on its own branch (`spec/13a-host-port` … `spec/13f-multi-model-ui`),
each shippable alone with the ones after it unbuilt. Do not start a later phase before
the earlier one is merged: every phase removes an assumption the next one depends on.

Read first: `docs/decisions/0003-transactional-single-active-model.md`,
`0004-per-node-resource-guardrails.md`, `0007-stable-endpoint-api-keys.md`,
`0013-delegated-placement.md`, `0015-roles-on-the-stable-endpoint.md`, and
`docs/plans/multispark-v1.md`.

## Problem

One Spark serves one model (ADR 0003). That was the right first release and it is now
the ceiling. A GB10 has 128 GB of unified memory and the catalog contains models that
use a fraction of it; the owner cannot have a small fast model and a large reasoning
model answering at the same time, and a Roles assignment that names two different models
costs a full container swap per request (`ensureServing` in
`internal/httpapi/roles.go`). Across two Sparks it is worse: the head's `/v1` serves
only what the head runs, and a model installed on the peer is reachable only from the
peer's own console (ADR 0013, Consequences).

Three concrete blockers in the code, in the order they must fall:

1. Every recipe declares `service.default_host_port: 8000` and
   `internal/operations/docker.go` `Create` binds exactly that port on `127.0.0.1`.
   Two containers cannot both be up.
2. `store.ActivateExclusively` (`internal/store/store.go:455`) deactivates every other
   row in the same transaction, and `internal/engine/engine.go` `plan` injects
   `previousStopPlans` before `verify_memory` on every install and start. Serving a
   second model is expressed in the data model as stopping the first.
3. `resourceguard.CheckMemory` sizes one model against the whole GPU
   (`runtimeBudget = GPUMemoryTotal * policy.GPUUtilization`, `guard.go:65`) and has no
   notion of memory already committed to something else that is running.

## User-visible outcome

- Several models serving at once on one Spark, each with its own row state, when the
  memory math says they fit and the recipes are qualified to share the GPU.
- `POST /v1/chat/completions` with `model: "<served_model_id>"` reaches that model
  directly with no swap, from any Spark in the fleet. The base URL never changes, which
  is the whole point of ADR 0007.
- A Roles assignment across two co-resident models stops costing a switch.
- The console shows more than one green dot, and says plainly which Spark each live
  model is on.

## Phase A: the bound port belongs to the install, not to the recipe

**Outcome:** nothing user-visible. A single active model still lands on 8000.

`service.default_host_port` becomes the preferred port. The port a container actually
bound becomes a column on the install.

1. `internal/store/store.go`: add `host_port INTEGER NOT NULL DEFAULT 0` to
   `installed_models` (additive, per ADR 0008 point 4) and to `InstalledModel`. A zero
   value means "the recipe's `default_host_port`", so every row written before this
   phase keeps resolving exactly as it does today.
2. `internal/engine/engine.go`: allocate at `create_container` planning time. Range
   `8000..8063`, first free, where free means: not held by another `installed_models`
   row with a container, and `verify_port` passes. Persist it with the container ID the
   same way `finish` already persists `ContainerID`. Allocation is a decision, so it
   goes in a step receipt (`{"host_port": N, "preferred": 8000}`) like every other
   decision this project makes.
3. `internal/operations/docker.go`: `Create` and `serveEndpointArgs` take the resolved
   port instead of reading `r.Service.DefaultHostPort`. Thread it through
   `operations.Execution` (it already carries per-job state such as `Placement` and
   `ReservedBytes`); do not put it on `recipe.Recipe`, which must stay the pinned
   document it is.
4. Every reader of `DefaultHostPort` moves to the resolved port. Grep is the checklist:
   `internal/httpapi/server.go` `proxyModel` (`target := fmt.Sprintf("127.0.0.1:%d", …)`),
   `managedPortOwner`, `runtimeMetrics`, and the `wait_http`,
   `verify_openai_inference`, `measure_throughput`, `verify_port` executors in
   `internal/operations/host.go`. Leaving one behind is a live misroute, so the report
   must list every call site found and how it was resolved.

**Tests.** Store round-trip including the zero-value default; allocator picks 8000 when
free, 8001 when 8000 is held by another install, and fails with a sentence when the
range is exhausted; `Create` receives the allocated port (the existing Docker fake in
`internal/operations/docker_test.go` asserts the request body); a proxy test where the
active model's row says 8001 and the fake runtime listens on 8001.

**Open question (owner).** Range `8000..8063` is invented here. It must not collide with
anything else the owner runs on a Spark. Confirm the range, or confirm that the manager
should instead ask the kernel for an ephemeral port and record it.

## Phase B: an active set instead of an active model

**Outcome:** still nothing user-visible by default. The store and engine can represent
more than one live model; policy in phase C decides whether a second one is admitted.

1. `internal/store/store.go`: add `Activate(ctx, model)` next to `ActivateExclusively`,
   writing the row active without touching any other row. `ActivateExclusively` stays
   and stays the default; it is what a swap is. Add `ActiveModels(ctx)` returning every
   `active=1, status='ready'` row.
2. `internal/engine/engine.go`: the one-slot `runtime chan struct{}` becomes two things.
   A per-recipe runtime lock (reuse the `recipeLocks` map idiom) serializes container
   mutations for one model. A separate process-wide `admission sync.Mutex` is held only
   across the phase C decision plus the memory verification and the container start, so
   two installs cannot both look at the same free memory and both conclude yes. Keep the
   comment on `runtimeOperations` accurate: it currently says the lock covers "the
   single active slot", which will stop being true.
3. `plan` stops injecting `previousStopPlans` unconditionally. It injects them only when
   the admission decision says the target does not fit alongside what is live. The
   rollback machinery (`failSwitch`, `rollbackPlans`, `ErrSwitchTargetChanged`) is
   unchanged and still covers that path, because that path is still a switch.
4. `ReconcileActiveModel` restarts every recovering row, not the first one.

**Do not weaken anything.** When the decision is "does not fit", the behaviour is
byte-for-byte today's transactional switch, including rollback. A test must pin that.

**Tests.** Two installs admitted concurrently both end active; an admitted install does
not stop the incumbent; a rejected install produces exactly the step sequence today's
switch produces (assert the plan, not the outcome); `ErrSwitchTargetChanged` still
fires when the incumbent changed under a long install; the admission mutex is proved by
a fake guard that records the order it was called in.

## Phase C: memory budgeting across loaded models

**Outcome:** the manager can answer "can this model start without evicting that one",
with numbers it can defend.

This is the phase with the real constraint, and it is not a code constraint. Every
shipped recipe pins `service.vllm.gpu_memory_utilization` at a value that claims most of
the GPU, because vLLM pre-reserves that fraction for weights plus KV cache. Two such
models cannot co-exist, and lowering the pinned fraction to make room is exactly what
ADR 0004 forbids without a recipe-version change and new hardware evidence.

So co-residency is a recipe property, declared and qualified, not a runtime override.

1. `internal/recipe/types.go`: new optional block.

   ```yaml
   co_residency:
     gpu_memory_utilization_min: "0.35"   # qualified floor, same string form as the pin
     qualified_with: ["qwen36-27b-nvfp4-1s"]  # ids this was measured alongside
   ```

   A recipe without the block never co-resides; it is admitted only when nothing else is
   live. The values are measured by maintainers during qualification. **The executor
   does not fill them in for existing recipes** (same rule as spec 05's `memory_model`).
   Record in `docs/DGX-QUALIFICATION.md` what a co-residency qualification must measure.
2. `internal/resourceguard/guard.go`: new `CheckMemorySet(nodes, expectedNodes,
   []MemoryPolicy) ([]MemoryResult, error)`. It sums the runtime budgets of every policy
   in the set and requires, per node, that the sum plus one host reserve (the maximum
   across the set, not the sum, and say why in a comment) fits inside
   `SystemMemoryAvailable`, and that the sum fits inside `GPUMemoryFree`. Per-node
   evaluation still never pools across nodes. `CheckMemory` stays as the single-model
   case and keeps its tests untouched.
3. The engine's admission decision: build the policy set from the live models plus the
   target, using each recipe's `co_residency.gpu_memory_utilization_min` when present and
   its pinned `gpu_memory_utilization` otherwise. If every member declares co-residency
   and the set passes, admit. Otherwise switch. The live re-read before start (ADR 0004)
   is unchanged and still runs.
4. When admitted, the container is created with the recipe's co-residency floor rather
   than its solo pin. That value comes from the recipe, so pinning is intact; write both
   values into the `create_container` receipt so the receipt shows which one was used.

**Tests.** Set of one behaves exactly like `CheckMemory` today; a set that fits is
admitted; a set that overflows unified memory is refused with the guard's own sentence;
a recipe with no `co_residency` block is never admitted into a non-empty set; the
host reserve is taken as a max and a test pins that a sum would have been wrong.

**Open questions (owner).**
- Is a declared floor plus a qualification run the right contract, or should
  co-residency be expressed as an explicit qualified pair list only (no computed sets)?
- vLLM's fraction is of total GPU memory, and two vLLM processes on one GB10 both
  compute it from the same total. Whether the floors are additive in practice is a
  hardware fact nobody here has measured. Nothing in this phase may claim they are.

## Phase D: /v1 routes by model field, on this Spark

**Outcome:** a request naming an installed model reaches that model, with no swap when
it is already live.

`internal/httpapi/roles.go` already reads the top-level `model` field without buffering
the body (`peekModelField`, `modelFieldSpan`) and already rewrites it
(`replaceModelField`). Extend `inferenceTarget`, do not write a second router.

1. A `model` value matching an installed recipe's `service.served_model_id` (or its
   recipe id) resolves to that model. Live: route straight to its port. Installed and
   idle: same `ensureServing` path roles already use, which after phase C may admit it
   alongside instead of swapping.
2. A `model` value matching nothing installed keeps today's behaviour exactly: the
   active model answers, and the runtime decides what to say about the name. Do not turn
   an unknown name into an error in this phase; existing clients depend on the current
   forgiveness.
3. `GET /v1/models` returns every installed model in OpenAI list shape, with the live
   ones first. Same auth as the rest of `/v1` (`authorizeInference`).
4. Roles resolution is unchanged and takes precedence: `role/` prefix first, then a
   concrete name.

**Tests.** Extend `internal/httpapi/roles_test.go`: concrete served_model_id routes to
its own port while another model is active; unknown name falls through to the active
model unchanged, body byte-for-byte; `/v1/models` lists installed models and requires
auth; the body is not rewritten when the request already names the real served id.

## Phase E: /v1 routes across Sparks

**Outcome:** one endpoint for the whole fleet.

This phase reverses a recorded decision. ADR 0013 states that the head does not proxy
inference to the peer and that "the peer's `/v1` endpoint belongs to the peer". Do not
build this phase until a new ADR supersedes that consequence. Write the ADR first, as
part of this phase, and have the owner accept it before any code.

Sketch, so the ADR has something to argue with:

1. The head learns the peer's models from the summary it already fetches
   (`internal/httpapi/fleet.go`, `fetchPeerJSON` against `/api/v1/models`). It caches
   nothing beyond the poll it already does; ADR 0013's "no cross-machine model registry"
   consequence should survive this phase.
2. When `inferenceTarget` finds no local match and the peer summary shows one, the
   request is forwarded to the peer's `/v1` with the peer's API key from
   `peers.api_key`, through the same allowlist gate every outbound peer call passes.
   `/v1/*` becomes a new allowlist entry, and it is the first non-`/api/v1` one.
3. Streaming must survive two proxies. `FlushInterval: -1` on both hops.
4. Failure copy names the machine: the console and the API error both say which Spark
   did not answer, because "the model runtime did not respond" is useless in a fleet.

**Open questions (owner).**
- Does forwarding inference to a peer widen the API key's meaning too far? A key on the
  head becomes a key that can spend the peer's GPU. ADR 0013 chose narrow scope
  deliberately.
- One peer only, today (`peers` is a singleton table by schema). Routing across more
  than two machines is a different spec and needs the membership work ADR 0005 deferred.

## Phase F: the console shows several live models

**Outcome:** the owner can see and control an active set.

Mockup-gated. Every state below is a new visual concept, not an incremental edit, so
produce a static mockup and get the owner's approval before implementing.

States the mockup must cover: two or more rows with the live dot; a row that is live on
the peer rather than here; a memory bar showing the set's committed total against the
budget (the spec 05 `memory.ts` math is the same math, so reuse it, do not fork it); the
refusal a user sees when starting a third model would not fit, phrased as what to stop
rather than as a failure; and Roles assignments that now cost no switch versus ones that
still do.

**Tests.** Playwright mock harness per the conventions, with fixtures for each state
above. No new dependency.

## Non-goals for all six phases

Autoscaling or unloading models on idle; per-model API keys; scheduling across more than
two Sparks; splitting one model across Sparks (that is `multispark-v1.md`); changing
what a recipe pins; request-level fairness or queueing between co-resident models.
