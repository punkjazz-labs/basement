# 0011 — Multi-runtime support

Date: 2026-08-03. Status: accepted (direction), not yet implemented.
Decided by the owner: basement must be able to run any model independently
of the inference runtime, including SGLang.

## Context

The manager is vLLM-only today (`runtime.kind` exists in the recipe schema
but the validator admits only `vllm`). The August 2026 community sweep
(docs/MODEL-CANDIDATES-2026-08.md) showed no single runtime covers the
models people actually want on GB10 hardware:

- Inkling-Small (Thinking Machines, 276B/12B, 1M ctx) runs on 2x Spark via
  SGLang; vLLM's official recipe does not list GB10 for it.
- DeepSeek V4 Flash on ONE Spark is llama.cpp/GGUF or antirez's ds4 engine;
  the clean vLLM NVFP4 artifact (168 GB) needs two Sparks.
- Step 3.7 Flash on one Spark is GGUF-only; its vLLM path needs two nodes.
- The Qwen family remains vLLM-native and well served today.

NVIDIA itself ships DGX Spark playbooks for vLLM, SGLang, llama.cpp and
TensorRT-LLM. All candidate runtimes expose an OpenAI-compatible HTTP
endpoint and a health route, which is the surface the manager already
depends on.

## What stays invariant

These recipe-system guarantees do not weaken with new runtimes:

- Pinned artifacts: HF repository + revision + expected bytes, verified.
- Pinned runtime: container image by digest. No `:latest`, no drift.
- Lifecycle receipts for every step; licence acceptance where required.
- OpenAI-compatible serve behind the manager's authenticated `/v1` proxy.
- Per-node resource guardrails evaluated before install and before start.
- Hardware qualification on real GB10 devices before a recipe leaves
  candidate status.

## Current vLLM coupling (measured, small)

- `internal/recipe/validator.go`: kind allowlist (one line).
- `internal/operations/docker.go`: entrypoint + `vllmArgs` builder.
- `internal/operations/host.go`: memory model reads `service.vllm` fields.
- `internal/httpapi/server.go`: Prometheus sampler with `vllm:` prefixes.

Everything else (download, image pull by digest, container lifecycle,
health wait, inference test, benchmark, receipts, console) is already
runtime-neutral.

## Design

A runtime kind is a small adapter with four responsibilities:

1. Command: entrypoint + args derived from the recipe's per-runtime
   service block (`service.vllm` today; add `service.sglang`, later
   `service.llamacpp`). Unknown fields remain schema errors.
2. Memory model: how planned memory is computed for guardrails
   (vLLM: gpu_memory_utilization; SGLang: mem_fraction_static;
   llama.cpp: model bytes + ctx KV estimate).
3. Health + readiness: path and success criteria (all three expose
   /health or equivalent; the OpenAI inference test stays shared).
4. Metrics: Prometheus prefix mapping (vllm:* today; sglang:* exists;
   llama-server has /metrics behind --metrics). Console fields stay the
   same; absent metrics render as n/a, never invented.

## Phasing

1. SGLang kind. Container-first like today; NVIDIA's own playbook uses
   `lmsysorg/sglang:latest-cu130` (we pin a digest). Upstream GB10 support
   is still in bring-up (sgl-project/sglang#11658: CUDA 13 nightly torch,
   sm_121a Triton issues, FP8 CUTLASS fallbacks), so recipes pin known-good
   community/NVIDIA images and qualification carries the risk, as designed.
   First recipe: Inkling-Small-NVFP4 2s (after multispark plumbing exists).
2. llama.cpp kind. Unlocks GGUF and therefore single-Spark DeepSeek V4
   Flash and Step 3.7 Flash. NVIDIA's playbook is native-compile only, so
   this requires building and pinning our own aarch64 CUDA container image
   (community images exist but are unaudited). llama-server is OpenAI
   compatible with /health and /metrics.
3. Watch: ds4/DwarfStar (antirez). Today's best single-Spark DeepSeek
   numbers and stability reports, but days old and single-family. Revisit
   once it stabilizes; a kind is only worth adding when we can pin it.
4. Not planned: TensorRT-LLM (no demand signal yet), Ollama (wraps
   llama.cpp; we would pin the underlying thing instead).

## Consequences

- Recipe schema gains per-runtime service blocks; version bump, validator
  learns kinds incrementally, old recipes stay valid.
- The console needs no redesign: status, telemetry tiles and measured
  speeds are already presented runtime-neutrally; missing metrics show n/a.
- Multi-Spark work (ADR 0005 placement + this) is what Inkling and the
  DeepSeek flagship recipes are gated on; single-node SGLang can land first
  if a compelling single-node SGLang recipe appears.
