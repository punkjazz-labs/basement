# Multispark v1 — minimal two-Spark execution path

Status: HISTORICAL IMPLEMENTATION PLAN. The minimal path described here has
since been exercised and extended on two-Spark hardware. ADR 0016 owns the
current fleet, reservation and recovery behavior; shipped recipe trust still
comes only from the qualification record.

## What the community recipe actually does

Source (the URL in the original brief, `MiaAI-Lab/...`, returns 404; this is
the working repository found by GitHub search for the same workload):

- https://github.com/tonyd2wild/Deepseek-v4-Flash-TP2-DGX-Spark-500k-CTX
- README: https://raw.githubusercontent.com/tonyd2wild/Deepseek-v4-Flash-TP2-DGX-Spark-500k-CTX/main/README.md
- Launch script: `launch/unholy-launch.sh` (same repository, `main`)

Facts taken from it and adopted here:

- One container image per node, ARM64, identical on both nodes.
- Container runtime flags: `--network host --ipc host`, `--shm-size 10g`,
  `--ulimit memlock=-1`, `--device /dev/infiniband:/dev/infiniband`, GPUs on
  both nodes. The recipe also uses `--privileged`; see "Deliberate deviations".
- Interconnect environment, identical on both nodes:
  `NCCL_IB_DISABLE=0`, `NCCL_IB_HCA=rocep1s0f0`, `NCCL_IB_GID_INDEX=3`,
  `NCCL_SOCKET_IFNAME=enp1s0f0np0`, `GLOO_SOCKET_IFNAME=enp1s0f0np0`,
  `TP_SOCKET_IFNAME=enp1s0f0np0`, `NCCL_IGNORE_CPU_AFFINITY=1`,
  `NCCL_DEBUG=WARN`.
- Distributed launch is plain `vllm serve` with
  `--nnodes 2 --node-rank <0|1> --master-addr <head RoCE IPv4>
  --master-port 29501`, plus `--headless` on the worker only. No ray, no
  torchrun.
- `--tensor-parallel-size 2` and `--distributed-executor-backend mp`.
- Weights are pre-downloaded on BOTH nodes; the worker is not fed weights
  over the fabric.
- Launch order is worker (rank 1) first, head (rank 0) second.
- Only the head serves HTTP, on port 8000. Startup is 6-7 minutes.

The device names (`rocep1s0f0`, `enp1s0f0np0`) and the GID index are
hardware facts of that person's cabling, so they live in the recipe's new
`topology.interconnect` block rather than in Go code. The manager never
guesses them.

## Chosen topology

- Head = the node whose manager runs the install/start job. It creates the
  rank-0 container, serves HTTP, and is the only node health-checked and
  inference-tested.
- Worker = exactly one configured peer (`/api/v1/peers`). If zero peers, or
  more than one, a two-Spark job refuses to start rather than choosing.
- `--master-addr` is the head's own IPv4 address on the interface named by
  `NCCL_SOCKET_IFNAME` in the recipe's interconnect block, resolved locally
  at create time. It is never guessed and never taken from the peer.
- The head's vLLM API binds `127.0.0.1` even under host networking, so
  ADR 0007 (the manager's authenticated `/v1` proxy is the only network path
  to the model) still holds. Under host networking there is no port
  publishing, so the head serves directly on `service.default_host_port`.

## Orchestration order (install)

1. Head preflight: the recipe's own `verify_*` operations, unchanged.
2. `verify_peer_node`: the head calls the worker manager's internal preflight
   endpoint and fails the job unless the worker's own preflight is ready.
   ADR 0004's per-node evaluation is satisfied by each node running its own
   guardrails, never by aggregating.
3. `pull_image` on head, then on worker.
4. `download_artifact` on head, then on worker. Byte verification is the
   existing per-node artifact verification on each node; no bytes cross the
   fabric.
5. `write_generated_config` on head, then on worker.
6. Worker `create_container`, `verify_memory`, `start_container`.
7. Head `create_container`, `verify_memory`, `start_container`.
8. Head `wait_http`, then the shared `verify_openai_inference`.

Stop tears down head then worker (stop serving first). Remove runs the
recipe's uninstall sequence on head then worker for every step.

Every step receipt carries `node` (the node's name) and `node_role`
(`head`/`worker`), so a two-node job's timeline is unambiguous.

The worker peer is resolved once, when the job is planned, and pinned for
every later step including teardown. Re-resolving per step would let a peer
edited mid-job receive a stop meant for a different machine.

## What a worker will accept

The node endpoints (`/api/v1/internal/node/preflight`, `/step`,
`/step/progress`) take a fleet API key and no cookie, so a browser can never
be walked into calling them.
They are bounded further: a delegated step runs only when this Spark already
holds that exact recipe id and version, byte for byte identical to the copy
the head sent, and the LOCAL copy is what executes. An ordinary API key
already authorizes installs of this Spark's own catalogue; delegation must
not widen that into running a caller-chosen image with host networking, RDMA
devices and the GPU. Delegated steps also execute against the local executor
directly, never back through the fleet executor, which would forward them
onward instead of putting a rank on this machine.

One delegated job holds a worker at a time through the persistent node
allocator. Its preparation window is 45 minutes. Once active, the head renews
the worker every heartbeat and the worker permits nine missed heartbeats before
reclaiming the rank. Worker liveness is persisted after `start_container`, so
a renewal can distinguish long staging from a rank that started and died.

## Live progress on a worker step

A delegated step answers only when it is finished, and it runs outside the
worker's own engine, so nothing there records it. The worker keeps the
running step's latest receipt in memory and `/step/progress` reports it to
the job that owns it, and to no one else. The head polls that every two
seconds while its own step call is blocked and republishes what comes back
through the same progress callback a local step uses, stamped with the node
it came from. The console therefore renders a forwarded weight download or
image pull exactly as it renders a local one. A worker that cannot answer
the poll leaves the step without live detail; it never fails it.

## Failure model

Any step failing on either node tears both nodes down to stopped, then the
job fails. This includes failures that are not the model's fault: a state
write that cannot be persisted goes down the same path, and the teardown
issues both container stops even when its own receipts cannot be written. A
database problem must never be the reason a rank keeps holding memory.

Serving has the same group boundary. The head proves the worker reservation
and, after readiness, the head runtime's bounded `/health` response on every
maintenance pass. A failure sustained through the 30-second freshness window
marks the model failed before either container is stopped. Once both stops and
claim release succeed, the engine creates one durable whole-group start job.
That job is the automatic recovery receipt; it never restarts an isolated rank,
and a failed recovery stays failed instead of looping.

Switching away from a model stops every rank THAT model runs on, read from
its own topology rather than the incoming one, and a rollback brings every
one of its ranks back (worker first, then head). Teardown issues `stop_container` on the worker and on the head,
recorded as `teardown_stop_container` receipts naming each node, and is
best-effort: a teardown failure is reported, never swallowed, but does not
mask the original cause.

Single-node recipes take exactly the path they took before this change: the
expansion is only reached when `topology.spark_count == 2`.

## Container launch per node

Same image and same weights on both nodes. Both containers get:

- `NetworkMode: host`, `/dev/infiniband` device, `IPC_LOCK`, `memlock`
  unlimited, the recipe's `shm_bytes`. (Amended 2026-08-13: `IpcMode: host`
  was in the original launch set but contradicted ADR 0006 and made
  `shm_bytes` inert; distributed containers now keep an isolated IPC
  namespace with `/dev/shm` sized from the recipe.)
- The recipe's existing `runtime.environment`, plus
  `topology.interconnect.shared_environment`, plus the per-role
  `head_environment` / `worker_environment`.

Head arguments add: `--nnodes 2 --node-rank 0 --master-addr <resolved>
--master-port <interconnect.master_port>`, and `--host 127.0.0.1 --port
<default_host_port>`.

Worker arguments add the same distributed flags with `--node-rank 1` and
`--headless`.

## Deferred from ADR 0005 (explicitly not built)

- Pairing with short-lived local codes, persistent node identity, mutual TLS.
  This reuses the existing peer API key, which is what exists today.
- Peer-scoped credentials. Any unrevoked API key can drive the node
  endpoints; what bounds the damage is the catalogue match described above,
  not the identity of the caller. A key dedicated to one paired peer, and a
  way to tell one head from another, are still missing.
- Real admission control on a worker. The single-flight lease is a coarse
  stand-in: delegated steps run outside the worker's own engine, so they take
  neither its runtime lock nor its disk reservation, and a local install
  started on the worker itself can still race a delegated one. Routing
  delegated work through the worker's engine is the actual fix.
- Signed heartbeats, staleness exclusion, controller/agent split.
- Controller-signed group reservations. The persistent allocator prevents
  overlapping runtime claims, but this compatibility path still authenticates
  its head with an ordinary API key rather than a deployment-scoped grant.
- A scheduler or placement sheet. The worker is "the one configured peer",
  not a chosen node.
- Independent multi-model placement across four nodes.
- Leader failover.
- Recipe-declared topology counts above 2.

## Deliberate deviations from the community recipe

- No `--privileged`. The narrower set (`/dev/infiniband`, `IPC_LOCK`,
  unlimited `memlock`, host network) is what RDMA documentation actually
  requires. (Amended 2026-08-13: host IPC was dropped from this set per ADR
  0006; an explicit `shm_bytes` covers shared memory. Isolated IPC on the
  two-Spark path is not yet re-verified on hardware: if NCCL cannot open the
  HCA or complains about shared memory, this is the first thing to revisit.)
- The API server binds loopback instead of `0.0.0.0`, to keep ADR 0007.
- Worker weight download is the manager's own verified download on the worker
  node, not a manual `hf download`.

## Untested pending hardware

Everything above that touches real devices: RDMA reachability, the
`--master-addr` resolution from the interface name, whether `--headless`
plus `mp` backend behaves as the recipe reports, startup timing, and whether
the worker container's exit is visible to the head's health wait. The tests
in this change are deterministic fakes of orchestration order, receipts and
rollback, not of vLLM.

Also untested on hardware: whether the lease TTL is long enough for a real
168 GB download on a slow link, and how a two-node timeline reads in the
console.
