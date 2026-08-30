# ADR 0016: Multi-node fleet control and placement

Date: 2026-08-04. Status: proposed. Design only.

## Context

Every GB10 currently runs a complete basement manager. Each manager owns its
engine, SQLite store, recipe catalogue, licence acceptances, preflight checks,
active-model record and `/v1` endpoint. A manager may store exactly one peer.
That singleton is enforced in `internal/store` by `CreatePeer`,
`migratePeersSingleton` and the `peers_singleton` index, and it is relied on by
`cmd/basement/main.go` when it chooses the only possible worker for a
two-Spark recipe.

That implementation supports two useful cases:

- a two-Spark recipe whose job is owned by its head and whose worker rank is
  driven through `internal/httpapi/node.go`;
- a single-Spark install delegated to the peer's own public API, where the
  peer creates and owns its own job as required by ADR 0013.

It does not support the owner's fleet. An owner with three or four machines
still has several dashboards and cannot express either of these placements
from one place:

- run one single-Spark model on a named machine while another single-Spark
  model runs on another machine;
- run one two-Spark model on a chosen pair while a single-Spark model runs on
  a third machine at the same time.

ADR 0005 already accepted the direction: authenticated node identity,
pairing, signed heartbeats, recipe-declared topology, recommended and advanced
placement, per-node leases, all-or-nothing reservation, separate jobs for
independent deployments, and no automatic leader failover. This ADR makes
that direction concrete for fleets of two, three or four GB10 machines.

The design has to keep three existing truths.

First, resource safety is local. ADR 0003's active-model slot and ADR 0004's
memory and disk evaluation are per node. Capacity on another machine cannot
make an undersized or occupied node safe.

Second, job ownership follows the runtime. ADR 0013 refuses to run a
single-Spark recipe step by step on behalf of another manager because doing so
would put that manager's lifecycle in the wrong database. A distributed rank
is different: its group head drives the rank as part of one distributed job.

Third, one dashboard is not the same thing as one inference endpoint. ADR
0007 makes `/v1` stable on the manager that owns the active model. Concurrent
models on different machines necessarily have different serving managers.

## Decision

### One designated control plane

One node is the fleet controller. It owns the membership directory, the
placement records and the one routine dashboard for the fleet. The controller
is selected during first setup. When a fleet grows from an existing one-node
or two-node installation, the manager from whose console the other machines
are adopted becomes the controller.

The controller is not a replacement for the managers on the other nodes. It
does not copy their model rows, licence rows or engine jobs into a second
authoritative store. It keeps a small fleet deployment record that points to
the manager and job that own each lifecycle. The controller's console reads
and controls those managers through the fleet API.

A member console becomes a managed-node view while its controller is fresh.
It shows this node's health, allocations, local job records, serving endpoint
and the address of the fleet dashboard. Routine install, start, stop, remove,
role and key actions are made through the controller. Direct browser mutations
on an enrolled member refuse with a sentence naming the controller. This
prevents a second console from scheduling around a lease the controller
already granted.

The manager and engine still exist on every member because they are the
authority for work on that machine. A single-node placement creates a job in
that member's store. A member chosen as the head of a distributed group
creates the distributed job in its store. The managed-node view is a console
policy, not a smaller agent binary.

The controller may itself run a model, act as the head or worker of a
two-node group, or remain idle. Placement does not give it a hidden hardware
role.

### What happens when the controller fails

There is no automatic controller election and no replicated SQLite database.
This keeps ADR 0005's no-leader-failover decision.

If only the controller manager is unavailable:

- independent models on other nodes keep serving from those nodes' `/v1`
  endpoints;
- a distributed group whose head is another node keeps serving, and that head
  keeps its worker lease alive;
- jobs already owned by another node keep running and keep their records;
- no new fleet placement, membership change or fleet-wide action can begin;
- the single dashboard and the controller's last fleet projection are
  unavailable.

If the controller machine was also a distributed group head, that group's
endpoint is unavailable because the serving manager is unavailable. Its
worker rank may be stopped after its driver lease expires. This is cleanup of
an unusable group, not failover to another head.

Member consoles remain available for read-only diagnosis. They do not become
writable merely because a heartbeat is late, since a temporary partition must
not create two schedulers. An explicit local recovery action can make one node
standalone. That action requires the node's local pairing authority or SSH
administrator access, revokes the old controller locally, releases prepared
reservations, stops a distributed worker rank, and restores the full local
console. An independent model may keep serving because it is that node's own
deployment.

The preferred recovery is to repair the original controller with its data
directory intact. If that store is lost, the owner may explicitly create a new
fleet from a surviving node and re-enrol every other node with a fresh join
code or SSH proof. Local model and job records survive. Controller-only names,
membership history, stale heartbeat snapshots and remote job links from the
lost store do not. A running distributed deployment is imported only when
every selected node reports the same deployment id, recipe fingerprint, ranks
and head identity. Otherwise it is shown as unmanaged and must be stopped
before those nodes can be placed again. Nothing is reconstructed by guessing
from container names alone.

### Stable node identity and fleet membership

Every manager creates one random stable node id and one Ed25519 identity key
on first start. The private key stays in the data directory with the same
local protection as the session signing material. A random fleet id identifies
the controller's membership domain. Hostnames, IP addresses and mDNS names are
labels and routes, never identity.

Fleet traffic uses a dedicated mutually authenticated TLS listener. Pairing
pins the controller and member certificates rather than trusting a public CA
or every machine on the LAN. The public console listener and its HTTP or
Tailscale choices from ADR 0014 remain separate. A stored member therefore has
both a console URL for the owner and a node URL for authenticated manager
traffic.

Members sign a canonical heartbeat payload with their identity key. It names
the fleet id, node id, manager version, monotonic sequence, local time,
inventory, current allocations and recipe catalogue digest. The controller
verifies the signature and stores only the newest sequence. A heartbeat older
than the scheduler's bounded freshness window is stale and excludes the node
from new placement. An old signed heartbeat cannot be replayed as current.
Clock disagreement is displayed, but sequence and controller receipt time are
the admission authority.

Mutual TLS authenticates the live transport. The heartbeat signature makes
the stored inventory snapshot attributable after the connection has closed.
It does not make a compromised node's inventory truthful.

Discovery from ADR 0010 remains a hint. A discovered address can start one of
these explicit join paths and nothing more:

1. Console adoption over SSH extends ADR 0014. The controller installs its own
   exact binary, receives the new manager's bootstrap token, exchanges pinned
   identities and records the member. It can be repeated until the fleet has
   four nodes.
2. An already installed manager creates a short-lived, one-use fleet join
   code in its local console. The owner enters that code and its address in the
   controller console. This code is separate from the durable owner pairing
   token used to open a console.
3. During first setup, the operator chooses the controller and up to three
   additional GB10 machines. The setup process proves SSH administration on
   every selected target, installs the same binary on each, and completes the
   same identity exchange. The result is one controller pairing card and one
   fleet dashboard. "Install once" means one operator run; every node still
   receives its own manager and local store.

The fleet admits at most four nodes including the controller in this version.
Concurrent enrolments are serialized by a store transaction, just as the
current single-peer insert has one winner.

Removing a reachable member first revokes its fleet identity and clears its
controller record, then removes it from the controller directory. A running
or queued local job must finish or be cancelled first. A node taking part in a
distributed group cannot be removed until the whole group is stopped and its
rank allocations are released. Independent installed models and their local
job history are not deleted. If an independent model was serving, it may keep
serving when the node becomes standalone.

An unreachable member can be forgotten only through a separately worded
action. The controller records a tombstone and excludes that identity from
future grants, but it cannot claim to have stopped a model or erased a
credential on a machine it could not reach. The node must be recovered
locally before it joins any fleet again.

### Store shape and two-node migration

The local database gains additive tables rather than turning every manager's
store into a shared database:

- `node_identity` holds this manager's stable node id and public identity
  metadata. Private key bytes remain in a protected file.
- `fleet_config` says `standalone`, `controller` or `member`, and records the
  fleet id, controller node id, controller node URL and pinned certificate.
- `fleet_nodes` exists authoritatively on the controller. It holds node id,
  display name, console URL, node URL, pinned certificate, manager version,
  membership state and the latest verified heartbeat envelope. The controller
  is a row too, so placement code does not special-case local capacity.
- `fleet_deployments` records a fleet deployment id, exact recipe id and
  version, recipe fingerprint, topology, owning node, owning job id, state and
  timestamps. It is a projection, not an engine job.
- `fleet_deployment_nodes` records the exact node ids, rank, group role,
  reservation id and fabric interface for one deployment.
- `node_reservations` exists on every node. It records prepared and committed
  disk, memory, port, runtime-slot and fabric claims with an expiry and the
  controller-signed placement grant.
- `distributed_ranks` exists on every node that can be a worker. It records
  the deployment id, exact recipe fingerprint, rank, driver node and local
  container state that the current in-memory `workerLease` cannot preserve.

Existing `jobs`, `job_steps`, `installed_models`, `accepted_licences`,
`territory_confirmations`, `roles`, `api_keys` and model metrics remain local
and keep their current meanings.

Migration is additive and staged:

1. Every database receives a node identity and the new empty tables. No
   existing row is removed or rewritten.
2. A database with one existing peer becomes the controller candidate. Its
   peer row is copied to `fleet_nodes` as `legacy-pending`, preserving the old
   peer id, URL, API key and creation time. The local node is added as the
   controller row.
3. When both managers run the fleet-capable version, the controller uses the
   existing key one last time to request the peer's stable identity and open
   the mutually authenticated channel. The legacy credential remains only for
   the one-version rollback window required by ADR 0008, and is then revoked.
4. An installed two-Spark recipe on the old head implies only one possible
   legacy pair: the local node and the preserved peer. The migration records
   that candidate deployment. The worker must prove the matching rank from
   local container and operation state before `distributed_ranks` becomes
   active. If it cannot, both nodes are marked `reconciliation-required`, the
   containers are left untouched, and new placement on them is blocked.
5. After identity conversion succeeds, the `peers_singleton` index is dropped.
   The old `peers` table remains readable for one-version rollback but no
   longer receives new fleet members. A later migration may remove it after
   the compatibility window.

An unreachable legacy peer remains visible as `legacy-pending`; it is not
deleted to make the migration look complete. Existing local jobs, model state,
licence records and peer credentials therefore survive even when fleet
conversion cannot finish immediately.

A database that already contains more than one legacy peer is not assigned a
controller automatically. The current code already treats that as a broken
two-node configuration. The new console preserves all rows and asks the owner
to resolve them before enrolment.

### Fleet deployments and placement

A fleet deployment is one model lifecycle on one exact set of nodes. There
are two forms:

- An independent deployment is one `spark_count: 1` recipe on one node. Four
  nodes may own four such deployments at once, subject to each node's local
  guardrails.
- A distributed group is one `spark_count: 2` recipe on two nodes, with one
  named group head and one named worker. Other fleet nodes are not part of the
  group and may run independent or distributed deployments of their own.

The catalogue does not gain an arbitrary node-count control. The recipe's
topology is the count. Deploying the same single-node recipe on two machines
means two independent deployments and two jobs, not a synthetic two-node
recipe.

The recommended scheduler enumerates at most four nodes and at most six pairs.
It does not need a general constraint solver. It filters out nodes that are
unpaired, stale, version-skewed, catalogue-skewed, already allocated for an
incompatible deployment, or locally unable to pass the recipe's guardrails.
For a distributed recipe it also requires a fresh fabric probe for that exact
pair. Among eligible placements it prefers already present pinned artifacts
and images, avoids an unnecessary local model switch, preserves the largest
reported disk margin, and chooses a head whose serving URL is reachable from
the controller. The chosen reasons are shown rather than hidden as a score.

The advanced placement sheet lets the owner select the exact node for a
single-node recipe, or the exact pair and serving head for a two-node recipe.
It cannot override these refusals:

- a count or topology the recipe does not declare;
- a stale, unreachable or version-skewed node;
- a recipe id, version or fingerprint absent from any selected node;
- a failed per-node memory, disk, port, secret or licence check;
- a node already committed to another serving allocation;
- a two-node pair without a proven fabric path;
- the same node in two concurrent deployments;
- a distributed activation that would replace unrelated active deployments
  on its selected nodes.

The last refusal is deliberate in the first implementation. A single-node
placement may transactionally switch the one model on its target using that
target's existing ADR 0003 engine path. Replacing different active models on
two machines would require one rollback transaction spanning two independent
engines. The owner must stop those deployments first. A later design may add
multi-node switching, but it must restore every node's own predecessor rather
than inventing one cluster-wide predecessor.

This is how the owner's three-node example is represented: one deployment for
the two-node model on nodes A and B, with A or B explicitly chosen as head,
and a separate deployment and job for the small model on node C. They may run
concurrently because no node appears in both placements. Four nodes may also
run two disjoint two-node groups.

### Per-node reservations and all-or-nothing admission

The in-memory `workerLease` in `internal/httpapi/node.go` is replaced by a
persistent per-node allocator shared by the local engine and fleet calls. A
remote rank no longer bypasses the engine's runtime slot and disk reservation.

A claim names concrete resources: reserved disk bytes, the runtime slot, host
ports the rank actually binds, and the fabric interface for a distributed
rank. Multiple download-only claims may coexist when their summed disk budget
passes the existing checks. There is still at most one committed serving
allocation on a node.

For a distributed placement the controller performs a small prepare protocol:

1. It creates a deployment id and asks every selected node to prepare the same
   exact recipe fingerprint and its own role-specific claims.
2. Each node runs its own preflight against current inventory and all local
   reservations, persists a short-lived `prepared` row, and returns an opaque
   reservation token. No image pull, download, container or model mutation has
   happened.
3. If any node refuses or times out, the controller releases every successful
   prepare. A release that cannot be delivered is completed by expiry. The
   failed placement has no engine job and no partial staging.
4. When every prepare succeeds, the controller signs one placement grant that
   names the fleet, deployment, exact nodes, ranks, driver, recipe fingerprint,
   claims and prepare tokens. The manager that will own the job asks every
   selected node to commit that grant.
5. If any commit fails, the owner asks every node to abort. Even a node that
   committed has not begun mutation and releases its claim, or expires it if
   unreachable. The engine starts only after every commit is acknowledged.
6. Committed claims become durable allocations as the owning job starts. The
   job owner renews an operation lease while it is active and releases or
   changes claims as the lifecycle changes.

This is all-or-nothing reservation, not a claim of a distributed database
transaction. The no-mutation boundary before all commit acknowledgements is
what prevents one node from downloading or stopping a model while another
node has already refused. Idempotent prepare, commit, abort and expiry make a
lost response safe to retry.

An independent deployment uses the same protocol with one selected node. Its
job then owns the local disk reservation and any serving allocation. Several
independent deployments are not combined into one transaction. A failure of
the small model on node C does not roll back the two-node model on A and B,
which is ADR 0005's separate-job decision.

The serving invariant is local and exact:

- zero or one active independent model or distributed rank owns a node's
  runtime slot;
- every selected node passes memory and disk independently;
- a two-node group consumes the runtime slot on both nodes;
- free memory or disk on an unselected node contributes nothing;
- stopped installed models and verified artifacts may remain on disk without
  owning the runtime slot.

### Distributed group ownership

The group head owns the distributed engine job even when it is not the fleet
controller. The controller sends a placement intent and signed grant to the
chosen head. The head resolves the recipe from its own catalogue, records the
job and the exact grant in its own store, commits the selected reservations,
and drives each worker rank step by step through the internal node API. Resume
uses the persisted node ids, ranks, endpoints and certificate fingerprints. It
never asks the current membership directory to choose the worker again.

The worker accepts the group head only for the deployment named in the
controller-signed grant. Its mutual TLS identity must match the grant's driver
node id, the recipe must match the worker's own exact catalogue copy, and the
operation must remain in the rank-operation allowlist. A group head therefore
does not gain general control of its worker.

The group head also owns the serving liveness proof. It renews the worker's
runtime reservation from committed staging through active serving. Once a
worker rank has started, renewal verifies that exact container is still
running; once the group is active and ready, the same maintenance pass also
checks the head runtime's `/health` route with a bounded request. A failure
sustained for the normal 30-second heartbeat freshness window closes
inference admission before the worker's 90-second reclaim deadline. The head
marks the model failed, stops both ranks, releases their runtime claims only
after both stops succeed, and records one ordinary whole-group start job for
automatic recovery. That job carries the trigger and the normal step receipts.
It is attempted once; a failed recovery remains failed for an operator to
inspect. This is recovery of the same exact group, not leader failover,
rescheduling, rank replacement or permission to keep one rank serving alone.

`/healthz` remains the manager-process health check used by installation and
fleet management. Runtime serving health is a separate proof and must not make
a healthy manager look down merely because its model needs recovery.

The controller stores the group head's node id and remote job id and projects
that job into the fleet Activity view. The full step receipts remain on the
group head. The controller may cache the last observed state so an unreachable
head can be shown honestly, but the cache never becomes an editable copy.

This preserves ADR 0013's distinction:

- a single-node model on node C is node C's own job and is delegated as an
  intent;
- a distributed model on A and B is one job on its chosen head, and that head
  drives B's rank mechanically;
- the fleet controller owns neither remote job merely because its dashboard
  asked for it.

ADR 0013's old console consequence is refined. The owner no longer has to open
the job owner's console for routine progress, lifecycle actions, Playground,
Generate, roles or API key management. The controller proxies those
console-authenticated actions over the fleet transport while the target
manager remains authoritative. A created API key is still stored only as a
hash on the serving manager and is shown once through the controller.

External inference does not pass through the controller. An independent
deployment uses its node's `/v1`; a distributed deployment uses its group
head's `/v1`. The controller shows and copies the correct endpoint. A
fleet-wide `/v1` router, federated roles and automatic request routing between
concurrent deployments are outside this decision.

### Fabric is a property of a distributed group

The management fleet and the model fabric are different networks with
different requirements.

Independent single-node deployments need only the management path between the
controller and each member. Model weights are downloaded by each node and
inference stays on that node. No tensor traffic crosses a high-speed cable.

Ranks of one distributed model need the ConnectX fabric declared by the
recipe. The current implementation is narrower than the phrase "NCCL
preflight" can suggest:

- `recipe.Topology` and the validator admit only `spark_count` 1 or 2;
- `operations.Deployment` contains exactly `Head`, `Worker` and one `Peer`;
- `detectFabricLink` requires one unambiguous linked and addressed fabric
  interface;
- `verify_fabric` opens a one-use TCP listener on the worker's fabric address
  and dials it from the head's fabric address before any staging;
- the later container start, health check and inference check are what prove
  that the runtime and NCCL path actually work together.

The preflight proves that the two ranks can rendezvous over the intended
fabric interface. It does not run an NCCL collective and does not prove
all-to-all bandwidth for three or four ranks.

What NVIDIA actually documents is wider than what basement implements, and the
two must not be confused. Each DGX Spark carries a ConnectX-7 NIC behind two
QSFP ports rated up to 200 Gb/s each. NVIDIA's clustering guide and the Sync
Cluster Assistant document three supported arrangements: two systems joined by
one direct cable, three systems joined by three direct cables in a ring where
every device connects to the other two, and two to four systems joined through
a switch with one cable per device. Direct cabling is documented for two and
three systems only; a four-system cluster requires the switch. The two styles
may not be mixed within one cluster. NCCL 2.30u1 records added support for the
three-system ring.

So the ceiling on distributed ranks in basement is a software limit, not a
hardware one. `recipe.Topology` admits only `spark_count` 1 or 2,
`operations.Deployment` is literally a `Head`, a `Worker` and one `Peer`, and
`detectFabricLink` resolves a single unambiguous interface. Nothing in that
shape can address a third rank or choose between two fabric ports on the same
machine. This ADR keeps the limit at two because that is what the code
expresses and what has been qualified on hardware, and it says plainly that
the limit is ours.

Two caveats stay attached to the NVIDIA material. First, it describes DGX
Spark. Port count and layout vary across GB10 OEM products, so a DGX Spark
diagram is not a fact about an MSI or ASUS machine, and the owner's exact
inventory has to be read from the machines rather than assumed. Second, a
documented topology is not a qualification: a supported cabling arrangement
still has to pass a real all-rank collective on the owner's hardware before
any recipe claims it.

This ADR therefore decides:

- A fleet may contain up to four nodes.
- A distributed group in this version contains exactly two nodes and uses a
  direct fabric path that passes the existing pre-staging rendezvous probe and
  the recipe's full hardware qualification.
- Four nodes may form two independent two-node groups with two separate
  direct cables. This is an architectural inference from disjoint nodes and
  interfaces, not a claim that every OEM port layout has been qualified. Each
  pair has its own head, worker, fabric probe and job; reusing the recipe's
  master port is safe because the groups bind on different hosts.
- A third or fourth node may run independent models beside a two-node group
  using only the management LAN.
- A three-node or four-node distributed model is not offered in this version,
  because basement cannot express one. It is not refused on the grounds that
  the hardware cannot do it, and this ADR does not tell the owner that a ring
  is unsupported by NVIDIA when NVIDIA documents one.

A later wider-fabric ADR is a real and reachable piece of work rather than a
speculative one, and it requires: an interface and fabric-id model rather than
`Head` plus `Worker`, recipe support for a wider rank count, all-rank
reservation, a fabric probe that proves every edge of a ring rather than one
link, and a real NCCL collective qualification before staging. Three-rank
support needs a ring with both QSFP ports used on every machine. Four-rank
support additionally needs a switch providing one common RDMA-capable Ethernet
fabric with the link mode, addressing, MTU and congestion settings the
ConnectX hardware requires. This ADR names no switch model and makes no claim
about hardware the owner has not identified.

### Version agreement and fleet upgrades

Every heartbeat reports the manager version, build identity and recipe
catalogue digest. Placement requires the exact same signed release identity on
the controller, job owner and every selected worker, plus an exact recipe
fingerprint on the nodes that will run it. Development builds whose version is
`dev` compare a binary build fingerprint rather than treating every `dev`
binary as equal. A skewed node remains visible and continues serving, but it
is excluded from new reservations and fleet mutations.

Adoption copies the controller's own binary as ADR 0014 already does, so a new
member joins without skew. During first setup the same installer artifact is
used on every selected node.

Until ADR 0008's signed apply and rollback path exists, an upgrade remains an
operator action on every node. The fleet dashboard lists the exact versions,
blocks new placement during skew, and gives the owner the order to follow.

After ADR 0008 is implemented, a fleet upgrade is a maintenance operation:

1. Stop admission of new jobs and wait for mutating jobs to finish.
2. Stage and independently verify the same signed release on every node.
3. Upgrade idle members first, then each distributed worker before its group
   head, one group at a time.
4. Upgrade the controller last. If it is also a group head, its group-head
   restart and dashboard interruption are stated before confirmation.
5. Leave maintenance only when every reachable member reports the target
   version and health check.

Existing model containers are not updated with the manager. A manager restart
may briefly remove its console and `/v1` proxy even while a container keeps
running. The local updater owns binary rollback. The fleet protocol keeps a
narrow current-version and previous-version compatibility surface for
heartbeat, upgrade status and rollback only; model placement remains blocked
until exact versions agree.

### Security boundary

Network proximity grants nothing. Discovery can reveal that an SSH or
basement service exists, but membership requires one of the explicit owner
proofs: SSH administrator access during adoption or setup, or a short-lived
join code created on the target manager.

Fleet certificates are not ordinary `/v1` API keys. The receiving manager
binds them to a fleet id, node id and scope. Only the controller certificate
may change membership or issue placement grants. A group head may call rank
operations only for a committed deployment that names it as driver. Every
node still resolves and fingerprints the recipe from its own catalogue before
running anything, preserving `trustedWorkerRecipe`'s defence against a
caller-supplied image or command.

A compromised member can lie about its own inventory, damage or delete its
own local state, refuse work, and break any distributed group in which it is a
rank. If selected as a group head, it can drive the exact allowlisted steps of
the exact deployment grant on that group's workers. It cannot enrol another
node, mint a new grant, run a single-Spark recipe through the rank API, choose
an arbitrary recipe body, or control an unrelated member.

A compromised controller is more serious because the owner intentionally
gave it fleet control. It can place, start, stop and remove catalogue recipes,
consume disk and interrupt serving across reachable members. Local catalogue
matching, per-node guardrails, licence gates and operation allowlists still
prevent it from turning that authority into arbitrary host commands. This is
damage containment, not a claim that a hostile controller is safe.

Mutual TLS protects fleet credentials and responses on a plain LAN. Console
transport remains governed by ADR 0014; typing SSH credentials into a console
served over plain HTTP retains the warning recorded there.

### Relationship to existing decisions

ADR 0005 is kept and refined point by point:

1. Advertisement remains discovery only and never grants membership.
2. Pairing becomes a short-lived join code or SSH-backed adoption and pins
   persistent node identity with mutual TLS.
3. Signed heartbeats, sequence checks and staleness exclusion are kept.
4. Recipes still declare topology. This ADR deliberately keeps supported
   counts at one and two.
5. The scheduler recommends a placement and the advanced sheet selects only
   eligible exact nodes.
6. Every selected node prepares a persistent resource lease and runs its own
   evaluator.
7. A distributed job mutates nothing until all reservations commit; any
   failed prepare or commit releases the rest.
8. Independent placements have separate jobs on their target managers.
9. There is one designated controller and no automatic leader failover.

ADR 0013's ownership rule and refusal of single-Spark recipes on the internal
rank API are kept. Its requirement to open the peer console is superseded by
the controller's authenticated proxy and job projection, not by moving the
job.

ADR 0014's SSH proof, pinned address, password handling and exact-version
binary copy are kept. The singleton check is replaced by a four-node capacity
check, and the identity exchange replaces the general-purpose fleet API key.

ADR 0003 and ADR 0004 are unchanged and become per-node scheduling
invariants. ADR 0007 remains per serving manager. ADR 0010 remains the
discovery and setup seed; it does not become a trust mechanism.

## Deliberately refused or deferred

- No Raft, shared SQLite, consensus service, replicated controller or
  automatic leader election. Four home or studio machines do not earn that
  failure surface.
- No arbitrary Spark count in the console and no topology invented by the
  scheduler.
- No aggregate memory or disk admission.
- No controller-owned copy of a remote engine job.
- No single-Spark recipe through the distributed rank API.
- No three-node or four-node distributed group in this version. The blocker is
  that the recipe topology, the deployment shape and the fabric probe describe
  exactly two ranks, not that NVIDIA lacks a documented topology.
- No automatic rescheduling, live migration or replacement of a failed rank.
- No multi-node transactional switch away from different predecessor models.
- No fleet-wide public `/v1`, federated roles or request router.
- No membership from mDNS, IP range, hostname, shared API key or physical
  cable alone.

## Consequences

- The owner can install managers across up to four machines in one setup run,
  or add them later from one controller console.
- The Fleet, Models, Activity, Playground, Generate, Roles and Connect views
  can operate from the controller without making the controller authoritative
  for remote jobs or model state.
- Three nodes can run a two-node model and a single-node model concurrently.
  Four can run four independent models, one two-node group plus two
  independent models, or two disjoint two-node groups when the physical
  fabric supports the chosen pairs.
- A member remains a complete recoverable manager. This costs more local code
  than a thin agent but preserves the store, guards and rollback authority
  already proved on that machine.
- The controller is a real availability dependency for management. Losing it
  loses the one dashboard and new placement until repair or explicit manual
  recovery. It does not by itself stop deployments owned by other nodes.
- External clients need the endpoint of the independent node or distributed
  group head they want. Concurrency across machines does not produce one
  magic endpoint.
- Removing an independent member does not delete its models. Removing a
  distributed member requires stopping the group first.
- Exact version agreement makes upgrades temporarily reduce schedulable
  capacity. The fleet shows skew rather than pretending mixed managers are
  safe.
- The first release solves a four-node management fleet without claiming a
  four-rank fabric.

## Open questions for the owner

**O1. Which machine should first setup choose as controller?** Recommendation:
default to the first selected machine but let the owner change it before any
installation. The best controller has stable power and a stable management
address. It does not need to be reserved from model work. Confirm whether the
setup should describe that choice or simply use the first machine.

**O2. What high-speed hardware do the owner's machines actually have?**
NVIDIA documents a three-system direct ring and a switched cluster of up to
four, so the question is no longer whether such a topology exists. It is
whether these particular machines can form one. The exact OEM models, the
number of usable QSFP ports per machine, cable or transceiver part numbers,
and any switch model and configuration are not facts in this repository, and
a ring needs both ports on every machine. Recommendation: ship two-node direct
groups and two disjoint pairs first, then read the real port inventory off the
machines before deciding whether a three-rank ring is worth the topology
rewrite it needs. Do not widen a recipe until an all-rank NCCL collective
passes on the owner's own hardware.

**O3. Should removing an independent member leave its active model serving?**
Recommendation: yes, matching today's peer removal, because membership is not
model ownership and stopping a working endpoint is a larger consequence than
removing a dashboard row. The confirmation must say that the model remains
available only through that node's local console and endpoint. The alternative
is a drain-first policy that is simpler to explain but creates downtime.

**O4. How much interactive traffic should the controller console proxy?**
Recommendation: proxy the console's own Playground and Generate sessions and
remote key management so the owner does not need another dashboard, while
keeping public `/v1` traffic direct to the serving node. Confirm whether even
console inference should instead open the serving node in a new browser tab.

## Sources

Repository sources:

- `internal/store/store.go`, especially `CreatePeer`,
  `migratePeersSingleton`, jobs, installed models and licence persistence.
- `cmd/basement/main.go`, where a distributed plan currently requires exactly
  one peer.
- `internal/httpapi/node.go`, including the worker operation allowlist,
  in-memory worker lease, local recipe fingerprint and single-Spark refusal.
- `internal/httpapi/server.go` and `internal/httpapi/fleet.go`, including fleet
  summaries, delegated placement, peer path allowlisting and console adoption.
- `internal/engine/engine.go`, including the per-node runtime semaphore,
  distributed planning, `peerFor`, rollback and teardown.
- `internal/operations/fleet.go` and `internal/operations/fabric.go`, including
  the `Head` and `Worker` deployment shape and the fabric-address TCP probe.
- `internal/discovery`, `internal/setup`, `webui/ui/src/views/Fleet.tsx` and
  `webui/ui/src/views/Models.tsx`.
- `docs/DGX-QUALIFICATION.md`, which records the two-node direct-cable hardware
  run and persistent link-local addressing used by current recipes.
- ADRs 0003, 0004, 0005, 0007, 0008, 0010, 0013, 0014 and 0015.

Public NVIDIA sources:

- DGX Spark hardware overview, including the ConnectX-7 NIC and the two rear
  QSFP ports: https://docs.nvidia.com/dgx/dgx-spark/hardware.html
- ConnectX-7 networking and clustering, including the two-system direct, the
  three-system direct ring and the switched playbooks:
  https://docs.nvidia.com/dgx/dgx-spark/spark-clustering.html
- NVIDIA Sync Cluster Assistant, which states the supported range of two to
  four devices and that direct cabling covers two or three while four requires
  a switch: https://docs.nvidia.com/sync/latest/cluster-assistant.html
- Connect three DGX Spark systems in a ring topology:
  https://build.nvidia.com/spark/connect-three-sparks
- NCCL environment variables, including interface and HCA selection used by
  the shipped recipes:
  https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html

These sources establish the two-system direct case, the three-system direct
ring and the switched cluster of up to four. They do not describe the owner's
specific OEM machines, do not name a switch the owner has, and do not
substitute for an all-rank NCCL qualification on the owner's own hardware.
Those remain the explicit hardware question O2.

An earlier draft of this ADR cited a page at
`docs.nvidia.com/dgx/dgx-spark/connect-two-sparks.html` and concluded from it
that NVIDIA documents only a two-system direct connection. That URL returns
404 and that conclusion was wrong. Every URL above was fetched before it was
recorded here.
