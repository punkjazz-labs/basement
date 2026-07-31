# ADR 0004: Per-node memory and disk guardrails

Status: accepted for local implementation; thresholds awaiting DGX Spark qualification

## Decision

Every recipe declares a per-node minimum unified-memory capacity and a per-node host-memory reserve. The manager combines those values with the recipe's pinned vLLM GPU-memory utilization, maximum context, sequence limit, and KV-cache type.

Before installation, the manager verifies that each node has enough total system and GPU-visible unified memory and that the vLLM allocation leaves the declared host reserve. Immediately before every container start, it re-reads live system and GPU memory and requires:

- free GPU-visible memory at least equal to the planned vLLM allocation;
- available unified memory at least equal to the planned allocation plus the host reserve; and
- nominal GPU headroom at least equal to the host reserve.

During a model switch, the previous manager-owned model stops before the live-memory recheck. If the target cannot satisfy the recheck, transactional rollback restores the previous model.

Disk checks include all pinned artifact bytes, a conservative expanded-image disk budget, and a recipe safety margin. Model data and Docker storage are checked separately when they use different filesystems. The manager checks before mutation, immediately before an image pull, and during image and artifact transfer progress. A transfer fails safely while retaining resumable partial artifacts if continuing would consume the safety reserve.

Resource reports are arrays of per-node results. A future multi-Spark orchestrator must supply exactly the recipe's node count, and every node must pass independently. Cluster-wide aggregate RAM or disk is never accepted as a substitute for an under-provisioned node.

## Why

DGX Spark uses unified memory. A model that fits by weight size alone can still fail when runtime allocations, KV cache, long contexts, speculative decoding, compilation, Docker, and the host operating system are included. Likewise, a disk check performed only when the user clicks Install can become stale during a long transfer.

Per-node evaluation also avoids a common distributed-systems error: summing healthy nodes with an undersized node even though each participant must hold its own runtime, cache, or shard.

## Consequences

- Recipe resource settings are versioned and require new DGX qualification when changed.
- The current release still rejects multi-Spark recipes; this decision defines the safety contract for later orchestration but does not claim multi-node support.
- Hardware qualification must record idle memory, planned allocation, host reserve, peak memory, OOM behavior, disk headroom, and results for every node.
- Thresholds may be tightened after real GB10 measurements, but they cannot be relaxed without a recipe-version change and new evidence.
- Adding these guardrail fields bumped the embedded recipe versions (Qwen 35B v2→v3, Qwen 27B v1→v2, Laguna v1→v2). This note was added retroactively so the recipe version audit trail stays complete.
