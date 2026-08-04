# Spec 12: Multi-node fleet

Status: proposed implementation plan. Architecture is ADR 0016.

This plan turns the accepted direction in ADR 0005 into a fleet of two, three
or four GB10 managers controlled from one designated dashboard. It preserves
ADR 0013's job-ownership boundary: independent jobs belong to their target
manager, and distributed jobs belong to their chosen group head.

Every phase below is independently shippable. A phase may add dormant schema
or a read-only surface, but it may not expose an action whose admission and
recovery path belongs to a later phase. Every phase leaves the complete Go and
console suites green. Hardware acceptance is called out separately from fake
or loopback evidence.

## 0. Outcome and limits

The owner gets these concrete outcomes:

1. Run setup once, select one controller and up to three more GB10 machines,
   and finish with one fleet dashboard.
2. Add a later machine from that dashboard over SSH, or pair an already
   installed manager with a short-lived join code.
3. See current health, version, installed models, active deployments and jobs
   for every member without opening another console.
4. Install and run separate single-node models on named machines at the same
   time.
5. Select any proven direct-cable pair for a two-node recipe, choose its group
   head, and run an independent model on another node concurrently.
6. Run two disjoint two-node groups on four nodes when both physical pairs pass
   fabric and full recipe qualification.
7. Diagnose a failed controller from a member and recover explicitly without
   automatic leader election.

This spec does not deliver:

- a three-rank or four-rank distributed recipe;
- a switch fabric not identified and qualified by the owner;
- aggregate memory or disk admission;
- automatic rescheduling or failover;
- a shared database or a controller-owned copy of remote jobs;
- one public inference endpoint for every concurrent fleet deployment;
- a multi-node switch that replaces different active models on several nodes
  and promises to restore all of them.

The fleet maximum is four nodes including the controller. The recipe topology
maximum remains two. These are separate limits on purpose.

## 1. Existing seams this plan keeps

The implementation should extend the paths that already own each decision.

| Concern | Current authority | Fleet treatment |
| --- | --- | --- |
| Local jobs and receipts | `internal/engine`, `internal/store` | Stay on the manager that owns the job. |
| Active-model switch | Engine runtime semaphore and ADR 0003 | Remains one runtime slot per node. |
| Memory and disk | `internal/operations/host.go`, engine disk reservations | Evaluated and reserved on each selected node. |
| Recipe trust | `trustedWorkerRecipe`, exact local fingerprint | Required on every selected node. |
| Distributed steps | `FleetExecutor`, `PeerClient`, node API | Driven by the selected group head under a placement grant. |
| Discovery | `internal/discovery` | Finds candidates only. Never grants membership. |
| SSH installation | `internal/setup`, ADR 0014 adoption | Reused for every adopted member and first setup. |
| Stable inference | Manager `/v1`, ADR 0007 | Stays on an independent node or distributed group head. |
| Console summary | Fleet and Models peer polling | Replaced by one controller projection over all members. |

The current `workerLease` is not retained as a second allocator. It is
in-memory, admits one remote driver, bypasses the local engine's disk and
runtime guards, and disappears on restart. Phase C replaces it with one
persistent per-node reservation service used by local and remote work.

The current fabric probe is kept for the two-node case. It proves a TCP path
from the head fabric address to the worker fabric address before staging. It
does not become evidence for a wider NCCL topology.

## 2. Target control flow

### 2.1 Membership and health

The controller's public API serves the browser. A separate fleet listener
serves manager-to-manager mutual TLS and is never registered on the public
console mux.

One new small package, `internal/fleet`, owns identity, the fleet transport,
heartbeat envelopes, placement grants and the reservation protocol. It does
not execute Docker or inspect host resources. It calls `internal/store` for
persistence, `inventory.Provider` for snapshots, and `operations.Executor` or
the engine only through existing interfaces.

The controller polls one signed heartbeat from each member on a bounded
interval. The stored view contains the last verified envelope and controller
receipt time. The console reads that store-backed projection rather than
launching three requests per peer from the browser every ten seconds as
`Fleet.tsx` does today.

### 2.2 Independent deployment

For a single-node recipe selected for node C:

1. The controller asks C to prepare local claims for the exact recipe.
2. C runs its own preflight and persists the prepared reservation.
3. The controller signs the one-node placement grant.
4. C commits it, resolves its own recipe and creates its own engine job.
5. The controller records `node C, remote job J` in `fleet_deployments` and
   projects J into Activity.
6. C's engine owns progress, switch rollback, licence records, final model row
   and the serving `/v1` endpoint.

The controller sends recipe id, owner intent and placement grant. It never
sends an operation list or an executable recipe body.

### 2.3 Distributed deployment

For a two-node recipe selected for A and B with B as group head:

1. The controller prepares role-specific claims on A and B.
2. After both prepare, it signs one grant naming B as driver, B as rank 0 and
   A as rank 1.
3. B commits both reservations and creates the distributed engine job in B's
   store.
4. B runs local head steps and drives A's worker steps over mutual TLS. A
   accepts only the exact grant, driver identity, recipe fingerprint and
   allowlisted rank operations.
5. The controller stores B's remote job id and projects its timeline.

The controller is absent from the execution path after it has granted the
placement. If it restarts, B's job and A's rank allocation remain recoverable.

### 2.4 Fleet projection

`fleet_deployments` is an index of authority, not a second job table. Each row
points to one owner node and one owner job. The controller may persist the last
observed state and timestamp for offline display, but step receipts are read
from the owner.

When the job owner is unreachable, the UI shows the last state as stale and
does not manufacture a terminal result. When a controller database is rebuilt,
members can report local jobs and allocations; automatic import requires exact
matching deployment records, as ADR 0016 states.

## 3. Store and protocol contracts

### 3.1 Local identity and membership tables

Phase A adds these tables through additive migrations in `internal/store`:

```text
node_identity
  node_id, public_key, certificate_fingerprint, created_at

fleet_config
  fleet_id, role, controller_node_id, controller_console_url,
  controller_node_url, controller_certificate, membership_epoch, joined_at

fleet_nodes
  fleet_id, node_id, display_name, console_url, node_url, certificate,
  manager_version, manager_build_identity, catalogue_digest, membership_state,
  heartbeat_sequence, heartbeat_received_at, heartbeat_payload,
  heartbeat_signature, legacy_peer_id, created_at, updated_at

fleet_deployments
  deployment_id, recipe_id, recipe_version, recipe_fingerprint,
  topology_count, owner_node_id, owner_job_id, state,
  last_observed_at, created_at, updated_at

fleet_deployment_nodes
  deployment_id, node_id, node_role, rank, reservation_id,
  fabric_interface, primary key (deployment_id, node_id)

node_reservations
  reservation_id, deployment_id, fleet_id, controller_node_id,
  driver_node_id, recipe_id, recipe_version, recipe_fingerprint,
  state, claims_json, prepare_token_hash, grant_json, expires_at,
  created_at, updated_at

distributed_ranks
  deployment_id, recipe_id, recipe_version, recipe_fingerprint,
  rank, driver_node_id, placement_json, container_id, state,
  driver_lease_expires_at, updated_at
```

`heartbeat_payload`, `claims_json`, `grant_json` and `placement_json` have
versioned Go structs and strict decoding. They are not untyped maps passed
from HTTP to the engine.

Private identity keys remain protected files under the manager data directory.
The database stores only public material and fingerprints. Existing plaintext
legacy peer API keys remain only for the migration window.

### 3.2 Browser-facing controller API

The exact response structs are introduced with their phase. The target surface
is:

```text
GET    /api/v1/fleet
POST   /api/v1/fleet/discover
POST   /api/v1/fleet/adopt
GET    /api/v1/fleet/adopt/status
POST   /api/v1/fleet/join
DELETE /api/v1/fleet/nodes/{node_id}
POST   /api/v1/fleet/nodes/{node_id}/forget

POST   /api/v1/fleet/placements/plan
POST   /api/v1/fleet/deployments
GET    /api/v1/fleet/deployments
GET    /api/v1/fleet/deployments/{deployment_id}
POST   /api/v1/fleet/deployments/{deployment_id}/{action}
GET    /api/v1/fleet/deployments/{deployment_id}/events
```

Every mutation is a console-session action with the existing CSRF and origin
checks. `{action}` is an allowlisted path segment for start, stop, remove,
cancel, smoke-test and benchmark. It is not passed through as a remote URL.

The planner response contains eligible placements, refused nodes with their
own reasons, role-specific resource receipts, fabric evidence when relevant,
and one recommended placement with explicit reasons. The commit request names
one of those exact node sets and an idempotency key. The server recalculates
admission; it never trusts a browser-carried preflight result.

### 3.3 Fleet transport API

The fleet listener exposes a separate fixed surface under
`/internal/fleet/v1`. It accepts mutual TLS identities and no cookie or public
API bearer token:

```text
GET  /identity
POST /join/prepare
POST /join/commit
GET  /heartbeat

POST /reservations/prepare
POST /reservations/commit
POST /reservations/abort
POST /reservations/renew

POST /deployments/independent
POST /deployments/distributed
GET  /jobs/{job_id}
POST /jobs/{job_id}/{action}
GET  /jobs/{job_id}/events

POST /ranks/fabric
POST /ranks/step
POST /ranks/step/progress
```

The existing public `/api/v1/internal/node/*` bearer surface stays only for the
one-version legacy migration and is removed from normal fleet use after Phase
E. Negative tests prove that the public mux cannot reach the new paths and the
fleet mux cannot reach console, key, role or arbitrary model endpoints.

Each transport request carries a protocol version, fleet id, caller node id,
request id and bounded body. Redirects remain refused. Responses are
size-capped and strictly decoded before they reach a store or engine.

### 3.4 Reservation states

The state machine is deliberately small:

```text
prepared -> committed -> active -> released
    |          |
    +----------+-> aborted
    +----------+-> expired
```

Only `prepared` and a committed reservation that has not started may expire
without stopping a model. `active` independent allocations persist with their
local model state. An active distributed worker has a separate renewable
driver lease; if its group head disappears beyond the bound, the node records
the expiry and stops the orphan rank through its local executor.

Prepare, commit, abort, renew and release are idempotent by reservation id and
request id. A repeated commit with different bytes is a conflict, not an
update.

### 3.5 Version and recipe agreement

The manager release identity and fleet protocol version are different fields.
Placement requires exact signed release identities and exact recipe
fingerprints. Development builds also compare a binary build fingerprint so
unrelated `dev` binaries do not read as equal. Heartbeat and upgrade status may
support the immediately previous protocol only for the rolling-upgrade path in
Phase G.

The recipe fingerprint continues to be over the strict local recipe value.
The receiving node executes its local recipe copy. A grant containing a
matching id and version but a different fingerprint is refused.

## 4. Placement and console design

This is a new visual concept. Static mockups for the N-node Fleet view, the
placement sheet, the controller-down member view and the recovery action must
be approved before their implementation phases.

### 4.1 Fleet view

The Fleet table always includes the controller. Each row shows:

- display name and stable short node id;
- Controller or Member;
- Fresh, Stale, Unreachable, Version mismatch or Reconciliation required;
- manager version;
- active independent model or distributed group and rank;
- management address and, only when known, the detected fabric interface;
- last verified heartbeat age.

The table never says a stale inventory is current. Expanding a row shows the
signed snapshot's controller receipt time, per-node resource values, local job
links and membership actions.

The first-run action remains `Add a Spark`. It offers `Find on this network`,
`Add an installed Spark` and the existing manual address path. The singleton
copy and `Pair a second Spark` language are removed only when Phase B supports
the replacement.

### 4.2 Models placement sheet

The Models view shows installed and active state per deployment, not one local
model plus one optional peer note.

For a one-node recipe the recommended line can read `Run on spark-c` and the
advanced list offers every eligible node. For a two-node recipe it can read
`Use spark-a and spark-b, serve from spark-b`. The sheet shows per-node disk,
memory, current model and what must stop before confirmation.

Changing to advanced mode never changes eligibility. A disabled node carries
the exact refusal from its own manager. There is no editable Spark count.

Submitting two placements creates two jobs. The UI may let the owner confirm
them one after another, but it does not present a batch as one atomic action.

### 4.3 Activity and deployment progress

Activity merges local and remote job projections ordered by their owner job's
creation time. Every row names the owner node and, for a distributed job, each
step receipt names its rank node and role.

Closing the deployment dialog never cancels a job. The SSE endpoint reconnects
to the owner job through the controller and falls back to polling the persisted
job when a stream breaks. An unreachable owner produces a stale state and a
retry control, not a failed job invented by the controller.

### 4.4 Member console

After Phase D supplies full controller actions, an enrolled member's normal
console opens on a compact managed-node view. It includes `Open fleet
dashboard` and read-only local details. It does not render enabled mutation
controls that the API will refuse.

`Take local control` is visually separate, requires local pairing authority or
SSH proof, and names its consequences. It is not an automatic stale-heartbeat
button.

### 4.5 Stable endpoints

Every deployment card shows `API endpoint`:

- one-node deployment: that node's manager URL plus `/v1`;
- two-node deployment: its group head's manager URL plus `/v1`.

The controller may proxy its own Playground and Generate traffic after owner
question O4 is resolved. It does not advertise the controller URL as a public
endpoint for a model served elsewhere.

## 5. Phases and evidence

Each phase runs the documented complete local suite:

```text
go build ./...
go vet ./...
go test ./...
go test -race ./...
cd webui/ui && npm ci && npm run build && npm test
```

Only phases that touch the console rebuild committed assets through the clean
source procedure in `docs/BUILDING.md`. Hardware checks do not replace the
local suite, and local fakes do not replace the named hardware checks.

### Phase A. Identity, schema and lossless migration

Add the stable local node identity, fleet tables and additive migration. Do
not add N-node actions or change current two-node execution in this phase.

Files and packages:

- `internal/store/store.go` and focused store files split from it if needed;
- `internal/store/store_test.go` for schema, concurrency and old-database
  fixtures;
- `internal/auth` or new `internal/fleet/identity.go` for protected identity
  key creation and loading;
- `cmd/basement/main.go` only to initialize identity before the API starts;
- `docs/BUILDING.md` if the data-directory contract needs the new key path
  recorded.

Implementation rules:

- Generate identity once. A restart returns the same node id and public key.
- Apply only additive schema changes. Older tables and rows remain readable.
- Copy one peer to `legacy-pending` without deleting or modifying the source
  row or its credential.
- Preserve a database with zero peers unchanged apart from the new local
  identity.
- Preserve a database with more than one peer and mark it for repair rather
  than selecting one.
- Detect a legacy active two-node recipe and write only a candidate placement.
  Do not claim a worker rank was reconciled without worker evidence.

Evidence:

- Opening the same database twice yields the same node id and key fingerprint.
- A pre-fleet database fixture retains byte-equivalent jobs, job steps,
  installed models, licences, territory confirmations, roles, API keys,
  metrics and peer data after migration.
- A one-peer fixture produces one controller candidate, one local node and one
  `legacy-pending` member.
- A multi-peer fixture opens successfully, preserves all rows and exposes a
  repair state.
- A simulated failure after each migration statement can reopen without a
  half-authoritative fleet.
- The existing `TestCreatePeerAdmitsExactlyOneWinner` and singleton migration
  tests continue to pass because behavior has not switched yet.

This phase is shippable because all new state is dormant and the existing
manager path is unchanged.

### Phase B. Secure membership, N-node adoption and one fleet summary

Build mutual TLS membership, signed heartbeats, four-node capacity, repeated
console adoption and first-setup enrolment. Deliver the first useful
single-dashboard surface: all nodes, versions, health and active models in one
Fleet view. Placements remain read-only at N nodes.

Files and packages:

- new `internal/fleet/transport.go`, `identity.go`, `membership.go`,
  `heartbeat.go` and their tests;
- `internal/config` for a separate fleet listener address;
- `cmd/basement/main.go` to start the public and fleet listeners and the
  heartbeat loop;
- `internal/httpapi/server.go`, `fleet.go`, and new focused
  `fleet_membership.go` for browser routes;
- `internal/discovery` for candidate fingerprints without changing trust;
- `internal/setup/install.go`, `wizard.go`, SSH runners and setup assets for
  controller selection and multi-target enrolment;
- `cmd/basement-setup` for the one-run controller and member flow;
- `packaging/systemd/basement.service`, `internal/setup/assets/basement.service` and
  installer parity when the fleet listener needs a service argument;
- `webui/ui/src/api.ts`, `App.tsx`, `views/Fleet.tsx` and `styles.css` after
  mockup approval.

Implementation rules:

- Use the standard library TLS and Ed25519 implementations. Add no service
  discovery or certificate authority dependency.
- The fleet listener has its own mux. It never accepts cookies or public API
  keys.
- A join code is one-use and expires. The durable console pairing token is not
  reused as a fleet join code.
- A heartbeat with the same or lower sequence never replaces a newer one.
- Scheduler freshness is based on controller receipt time. Report remote clock
  skew separately.
- Adoption retains ADR 0014's pinned-IP, password scrubbing, key cleanup and
  exact-binary rules.
- A new enrolment is refused when controller plus members already total four.
  Concurrent final commits cannot exceed that count.
- Keep a designated compatibility worker for an existing two-node deployment.
  Adding read-only members must not make `cmd/basement/main.go` reject the old
  pair merely because more nodes are visible.
- Before Phase C, local member consoles keep their current mutations with a
  fleet banner. The controller cannot schedule them yet, so blocking their
  only working controls would be a regression.

Evidence:

- Mutual TLS accepts only the exact pinned controller and member certificates.
  An unpaired certificate, public bearer key, cookie and redirected request are
  refused.
- Heartbeat tests cover a valid sequence, replay, altered payload, wrong fleet,
  stale receipt, clock skew and manager-version mismatch.
- Four concurrent enrolment commits at the final slot admit exactly one.
- Re-enrolling the same node id is idempotent; the same address with a
  different identity is a conflict shown to the owner.
- Adoption failure after remote membership prepare cleans up or reports the
  exact orphaned identity, with the typed password absent from every status,
  row and log-shaped string.
- A first-setup fake installs the exact same binary on three and four targets,
  selects one controller and returns one controller pairing card.
- UI mock fixtures show four fresh nodes, stale, unreachable, version mismatch,
  legacy pending and reconciliation required. The report contains screenshots.
- On real hardware, one controller discovers or reaches each owner-approved
  target, installs the same build and sees signed fresh inventory from all of
  them. If four machines are not available, the release may claim only the
  node count actually exercised.

This phase is shippable as a secure read-only fleet. Existing local and legacy
two-node actions remain the only mutation paths.

### Phase C. One persistent allocator for local and remote work

Replace the in-memory worker lease and engine-only disk/runtime accounting with
the persistent node reservation state machine. Do not expose general placement
UI yet.

Files and packages:

- new `internal/fleet/reservation.go`, `claims.go` and client/server transport;
- `internal/store` methods for reservation transactions and expiry;
- `internal/engine/engine.go` to prepare and claim disk, runtime and port
  resources through the allocator;
- `internal/httpapi/node.go` to remove `workerLease` and route legacy rank work
  through committed reservations;
- `internal/operations/host.go`, `fleet.go` and `fabric.go` for role-specific
  claims and local execution after admission;
- `cmd/basement/main.go` wiring and startup reconciliation.

Implementation rules:

- The allocator is per node and is the only authority for a runtime slot.
  Local browser jobs and fleet jobs enter through the same service.
- Preserve concurrent download behavior by summing disk-only claims. Do not
  turn the runtime slot into a global one-job-at-a-time lock.
- Claim a port only on ranks for which `RankBindsHostPort` says it applies.
- A reservation is persisted before it is acknowledged.
- Prepare and commit perform no operation from a recipe.
- Expiry can release prepared and unstarted committed claims. It never stops
  an active independent model.
- Startup reconciles active installed models and distributed rank records
  before accepting a new serving claim.
- The legacy two-node path receives a compatibility grant internally so the
  hardware behavior remains available during migration.

Evidence:

- Table tests cover every reservation state transition, idempotent retry,
  conflicting retry, expiry and restart.
- Two local installs whose summed disk claims exceed the safety margin cannot
  both prepare; two that fit retain today's concurrent transfer behavior.
- A local start and a remote rank commit racing for the runtime slot admit one
  winner. Run under the race detector.
- A worker reservation survives manager restart and blocks a competing local
  start until it is released or safely expired.
- Failure preparing node B releases node A. Failure committing B aborts A
  before either executor records an operation.
- The old two-node orchestration tests still show `verify_fabric` first and no
  staging before every selected reservation is committed.
- No test reads real memory, Docker, interfaces or RDMA devices; all existing
  seams remain stubbed.

This phase is shippable because it strengthens admission under existing
actions without yet offering a new placement promise.

### Phase D. Independent placement and remote lifecycle from the controller

Ship named-node placement for `spark_count: 1`, separate job ownership and
remote job projection. This is the earliest phase that delivers concurrent
heterogeneous models and routine one-dashboard control.

Files and packages:

- new `internal/fleet/scheduler.go`, `deployments.go` and remote job client;
- `internal/store` fleet deployment methods;
- `internal/httpapi/fleet_deployments.go`, `server.go`, delegated intent and
  SSE relay;
- `internal/engine` only for accepting a committed local placement grant when
  creating the existing job;
- `webui/ui/src/api.ts`, `App.tsx`, `views/Models.tsx`, `views/Activity.tsx`,
  `views/Deployment.tsx`, `views/Fleet.tsx` and `styles.css` after placement
  mockup approval;
- console unit tests and mock-harness fixtures for every placement state.

Implementation rules:

- The scheduler enumerates at most four one-node candidates and returns its
  filters and reasons.
- A remote target resolves its own recipe, preflight, licence and territory
  records and creates its own job. Preserve ADR 0013.
- The controller stores only deployment id, node id and remote job id plus
  last observed state.
- Start, stop, remove, cancel, smoke-test and benchmark are fixed fleet
  intents. The target runs its existing handler or engine path locally.
- A target's own ADR 0003 switch and rollback remain the activation path.
- Concurrent placements on disjoint nodes are independent jobs. A failure on
  one does not cancel another.
- Once all routine actions are available through the controller, member
  browser mutations switch to managed-node refusal and the member view from
  section 4.4.

Evidence:

- A controller placing a model on C creates no engine job in the controller
  store and exactly one job in C's store.
- A retried controller request reaches the same remote job through one
  idempotency key.
- Two placements on disjoint fake nodes run concurrently and each node has at
  most one active model. A third placement aimed at an occupied node either
  uses that node's local switch path or is refused with the local reason.
- Per-node memory and disk failures name only the node that reported them.
  Aggregate capacity never changes the answer.
- Remote licence and territory acceptance is recorded on the target and not
  copied into the controller's local acceptance tables.
- An unreachable remote job is shown stale with its last observation. The
  controller does not mark it failed.
- Public API keys cannot call fleet deployment intents. A fleet member cannot
  create a placement grant.
- Playwright-style screenshots cover recommended one-click placement,
  advanced exact-node selection, a disabled stale node, concurrent models,
  remote progress, remote failure and managed-member view.
- On two or more real nodes, install and serve different single-node recipes
  concurrently, prove each node's own `/v1`, stop one from the controller and
  confirm the other remains serving.

This phase is shippable without distributed scheduling. It already resolves
the separate-dashboard problem for independent models.

### Phase E. Selected two-node groups anywhere in the fleet

Generalize the existing two-Spark path from `the one peer` to one exact
controller-granted pair. A group head may be any selected node. Keep recipe
topology limited to two.

Files and packages:

- `internal/fleet/scheduler.go`, `fabric_matrix.go`, placement grants and
  two-node commit coordinator;
- `internal/operations/operations.go`, `fleet.go` and `fabric.go` to replace
  the single `Peer` deployment with exact node placements while retaining
  head and worker roles;
- `internal/engine/engine.go`, especially `deployment`,
  `distributedPlans`, `peerFor`, concurrent downloads, teardown and rollback;
- `internal/httpapi/node.go` rank endpoints moved to the fleet listener and
  bound to grants;
- `internal/store` distributed rank and deployment-node methods;
- `cmd/basement/main.go` removal of the singleton worker directory;
- `webui/ui/src/views/Models.tsx`, `Fleet.tsx`, `Activity.tsx`,
  `Deployment.tsx`, `api.ts` and styles for pair and head selection;
- recipe types and validator only if a topology-kind field is needed. Do not
  widen `spark_count` beyond 2 in this phase.

Implementation rules:

- Probe each candidate pair from the proposed head's fabric address to the
  proposed worker's fabric address. Cache only a short-lived result with both
  node identities, interfaces, addresses and controller receipt time.
- Run `verify_fabric` again as job step zero. Scheduler evidence never replaces
  execution-time preflight.
- Reserve both runtime slots, role-specific ports, disk and fabric interfaces
  before the group head creates or starts the job.
- The group head owns the engine job and its store record. The controller may
  be neither group node.
- Persist the full placement grant in the owning job before any step. Resume
  resolves ranks and endpoints from that grant, never from the current fleet
  directory.
- The worker trusts the group head only for the exact committed grant. Keep the
  local recipe fingerprint and operation allowlist checks.
- The group head renews the worker driver lease. Controller outage alone does
  not expire it.
- Refuse a pair whose nodes hold unrelated active deployments. Support a
  switch only when the previous distributed deployment has the same exact
  group ownership needed by the existing rollback plan.
- Keep teardown head first and start worker first.
- Preserve direct external inference through the group head.

Evidence:

- A controller C places a two-node recipe on A and B with B as head. Exactly
  one engine job exists, in B's store; A has a worker rank record; C has only
  the fleet projection.
- Reversing the selected head reverses rank and endpoint without changing
  membership.
- All prepare and commit failure points show zero recipe operations before
  full commit and release every successful reservation.
- A hostile or mistaken A cannot use its rank grant against C, another
  deployment id, another recipe fingerprint or a single-node recipe.
- Controller restart during a B-owned job does not stop the job. B restart
  resumes from its persisted job and rank assignment. B loss expires A's
  orphan driver lease and records the cleanup.
- The distributed plan still records node and role on every duplicate step,
  starts worker before head, tears down head before worker and restores the
  correct previous group on rollback.
- A resume after membership labels or addresses change still targets the
  persisted deployment. A switch away from an older distributed deployment
  uses that predecessor's persisted node set, not the incoming placement, and
  `peerFor` selects by deployment identity rather than recipe id alone.
- One two-node deployment and one independent deployment on a third fake node
  run concurrently. Attempts to reuse any node are refused.
- Two disjoint two-node groups on four fake nodes run concurrently without
  sharing reservation, peer, port or receipt state.
- The UI screenshots show recommended pair, advanced pair and head selection,
  no-fabric refusal, busy-node refusal, per-node receipts and a remote-head
  Activity timeline.
- Hardware evidence for each supported direct pair includes: persistent
  fabric addresses after reboot, job step zero fabric receipt, full image and
  artifact staging on both nodes, worker-first start, health, inference,
  per-node memory and disk receipts, stop, restart and forced failure teardown.
- Claiming two simultaneous two-node groups requires a real four-node run with
  two separate cables. Without that run the code may ship as candidate support
  but the product report must say the scenario is unqualified.

This phase stops at two distributed ranks because that is what the recipe
topology, the deployment shape and the fabric probe can express, not because
NVIDIA lacks a wider topology. Nothing about a ring, mesh or switch path is
inferred from two-node success either way.

### Phase F. Removal, controller recovery and complete dashboard proxy

Finish the operational edges: safe member removal, offline forget, local
takeover, explicit new-controller recovery, and the remaining one-dashboard
actions selected in owner question O4.

Files and packages:

- `internal/fleet/membership.go`, `recovery.go`, tombstones and epoch handling;
- `internal/store` membership tombstone, recovery import and controller
  projection methods;
- `internal/httpapi/fleet_membership.go`, recovery and console proxy routes;
- `internal/engine` and `internal/operations` only for explicit rank cleanup
  through their existing paths;
- `webui/ui/src/views/Fleet.tsx`, member landing view, `Playground.tsx`,
  `Generate.tsx`, `Connect.tsx`, `Roles.tsx`, `Activity.tsx` and `api.ts` as
  selected by the approved mockup and O4;
- `internal/setup` for SSH-backed re-enrolment after controller loss.

Implementation rules:

- Reachable removal revokes on the member before deleting the controller row.
- Independent model data and job history remain. A distributed member must be
  drained first.
- Offline forget records a tombstone and never claims remote cleanup.
- Staleness alone never promotes a member or enables local writes.
- Local takeover requires local pairing or SSH authority, invalidates the old
  fleet identity locally and stops only distributed rank state that cannot
  serve alone.
- New-controller recovery creates a new fleet id and epoch. Old-controller
  grants do not carry across.
- Import a distributed deployment only from matching records on every rank.
- If Playground and Generate proxying is approved, use a fixed streaming
  fleet endpoint to the serving manager. Do not expose a general URL proxy.
- Remote API key creation returns the secret once through the controller and
  persists only its hash on the serving manager.

Evidence:

- Removing a reachable independent member leaves its model and local job rows
  unchanged and makes its full local console available.
- Removing a node in an active distributed group is refused until the group is
  stopped.
- Forgetting an unreachable node records a tombstone, displays the unverified
  cleanup warning and causes reachable members to reject its old grants.
- A temporary controller heartbeat failure does not enable a member mutation.
- Local takeover with the wrong console authority is refused; a valid takeover
  revokes the controller, releases prepared claims and cleans an orphan rank.
- A new controller imports matching independent and distributed records and
  refuses a mismatched rank set without changing containers.
- If implemented, streamed Playground and Generate responses preserve
  cancellation, size bounds and target errors; no arbitrary endpoint can be
  reached through the proxy.
- Screenshots cover remove with a running independent model, distributed drain
  refusal, offline forget, controller-down member view, local takeover and new
  controller recovery.
- A hardware exercise powers off the controller while another node serves an
  independent model and while a remote-head group serves. Both endpoints
  continue as designed; the dashboard and new placement do not. Repairing the
  original controller restores the projection.

### Phase G. Fleet rolling upgrade after ADR 0008

Do not improvise remote binary replacement in the fleet feature. Start this
phase only after ADR 0008's signed release, privileged updater, versioned slots
and local rollback are implemented and qualified.

Files and packages:

- new `internal/update` package and the local updater command established by
  ADR 0008;
- `internal/fleet/upgrade.go` and the narrow previous-version protocol;
- `internal/store` maintenance and upgrade-run records;
- `internal/httpapi` fleet update status and actions;
- `webui/ui/src` update banner, Fleet version state and maintenance progress;
- `packaging/systemd/basement-updater.service`, its matching setup asset, and
  installer parity required by ADR 0008.

Implementation rules:

- The controller chooses one exact signed release. Every node independently
  verifies it before staging.
- Maintenance blocks new placement and membership changes. Existing serving
  containers are not model-updated or removed.
- Upgrade idle nodes, then distributed workers before group heads, and the
  controller last.
- Current and immediately previous fleet protocols may exchange only
  heartbeat, upgrade status and rollback. Placement remains exact-version.
- A failed node uses its own updater rollback. The controller records the skew
  and keeps maintenance active until the owner resolves it.
- A controller or group-head restart is named as endpoint downtime before the
  owner confirms.

Evidence:

- Corrupted, unsigned, wrong-architecture and wrong-version releases never
  enter an updater slot on any node.
- A fake four-node run proves the order and that no placement endpoint admits
  work between the first and last upgrade.
- Loss after each stage, swap and health response produces either local
  rollback or an honest skewed maintenance state.
- A previous-version node can report upgrade status but cannot prepare a model
  placement.
- The controller-last path survives the controller restart and reconstructs
  the upgrade run from member and local records.
- Real hardware qualification repeats ADR 0008's kill-during-swap and failed
  health checks on a member and on the controller, then proves models and
  recipes were not changed by the manager upgrade.

This phase is separately shippable. Until it lands, the Fleet view remains a
version-skew detector and a manual upgrade guide.

## 6. Failure cases that must stay visible

The following are product states, not generic 500 errors:

| Failure | Owner-visible result | Mutation allowed |
| --- | --- | --- |
| Heartbeat stale | Node named as stale with last verified time | No new placement on it |
| Version mismatch | Both exact versions shown | Upgrade/status only |
| One reservation prepare fails | Failing node's local reason and all releases | No job created |
| Commit response lost | Placement stays committing until idempotent retry or abort | No recipe step starts |
| Remote job owner unreachable | Last job state shown as stale | Retry read, no invented failure |
| Controller unavailable | Member recovery view and local endpoints continue | No fleet mutation |
| Group head unavailable | Group endpoint unavailable; worker lease expires | No head failover |
| Independent member removed | Model remains local unless owner stopped it | Local console regains control |
| Distributed member removal requested | Whole group named as blocker | Stop group first |
| Offline member forgotten | Tombstone and cleanup-unverified warning | Other nodes reject old grants |
| Fabric probe fails | Exact pair and interfaces named | Independent placements still allowed |
| Three-rank recipe proposed | Unsupported topology refusal | No override |

## 7. Hardware boundary and evidence

The current code and recipes are two-rank only. `Topology.SparkCount` accepts
1 or 2, `Deployment` has one head and worker, and `verify_fabric` proves one
head-worker TCP rendezvous path. The hardware qualification in
`docs/DGX-QUALIFICATION.md` proves that path and the full runtime on one
two-node pair.

NVIDIA's public DGX Spark documentation goes further than the two-node case:
it describes two systems on one direct cable, three systems in a direct ring
of three cables, and two to four systems through a switch, with four requiring
the switch. See the sources in ADR 0016. What it does not do is make every OEM
GB10 port layout identical, or qualify any of those arrangements on a
particular set of machines. The implementation must therefore keep reading the
real interfaces and carrier state as it does today, and the two-rank ceiling
must be described to the owner as basement's current limit rather than as a
hardware one.

For four nodes without a switch, the supported shape is two separate direct
pairs. This is possible in the architecture because each node belongs to one
group and each group has its own job, addresses and hosts. It still needs a
real four-machine qualification before the product calls it verified.

A wider group is a new project, and a reachable one rather than a speculative
one. A three-rank ring needs both QSFP ports used on every machine and a probe
that proves every edge of the ring, not one link. A four-rank group needs a
switch. Before design begins, record:

- exact GB10/OEM models and their high-speed port layout;
- exact switch model and software version, for the four-rank case;
- cable, optic or transceiver part numbers;
- link mode and negotiated speed on every port;
- addressing, MTU and any required RoCE congestion settings;
- an all-rank NCCL collective result, not only a TCP listener result;
- the runtime and recipe's real three-rank or four-rank support.

Without those receipts, the scheduler must say that the topology is
unsupported rather than guessing that physical link lights imply a usable
distributed model.

## 8. Open questions for the owner

**O1. Controller choice in first setup.** Recommendation: default to the first
selected machine and offer one explicit change before installation. Choose the
machine with the most stable power and management address, not necessarily the
machine intended for the largest model.

**O2. Wider fabric hardware.** NVIDIA documents a three-system direct ring and
a switched cluster of up to four, so this is a question about these machines
rather than about whether the topology exists. Provide the exact OEM port
inventory, and any switch, cable and transceiver models. Recommendation: keep
the first release at two-node direct groups and two disjoint pairs. This
question blocks only a future three-rank or four-rank phase, not Phases A
through G as written.

**O3. Removal of a serving independent member.** Recommendation: preserve the
running model and return the machine to standalone control, matching current
peer removal. If the preferred policy is drain-first, Phase F copy and tests
change but job ownership does not.

**O4. Controller proxy for Playground and Generate.** Recommendation: proxy
the controller console's own authenticated interactive sessions and key
management through fixed fleet routes, while public clients use the serving
node's `/v1`. If opening the target console is acceptable, omit those proxy
routes from Phase F and keep the controller focused on deployment control.

## 9. Sources and related work

The architecture source of truth is
`docs/decisions/0016-multi-node-fleet.md`. Its repository and public NVIDIA
source list applies to this plan.

Implementation must also read:

- ADR 0003 for per-node transactional switching;
- ADR 0004 for non-aggregating resource checks;
- ADR 0005 for the accepted fleet direction;
- ADR 0007 for per-manager stable endpoints;
- ADR 0008 before Phase G;
- ADR 0010 for discovery and setup;
- ADR 0013 for delegated versus driven job ownership;
- ADR 0014 for console adoption and exact-version installation;
- `docs/plans/multispark-v1.md` for the current two-rank execution order;
- `docs/BUILDING.md` for host seams, race tests and committed console assets;
- `docs/DGX-QUALIFICATION.md` for the existing two-node hardware receipts.

No phase may weaken recipe pinning, local recipe fingerprint checks, licence
gates, per-node resource guards, fabric preflight, inference verification,
typed receipts or rollback in order to make fleet placement pass.
