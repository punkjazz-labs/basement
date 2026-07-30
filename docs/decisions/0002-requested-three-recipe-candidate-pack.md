# ADR 0002: Requested three-recipe candidate pack

Status: candidate, awaiting complete DGX Spark acceptance verification
Date: 2026-07-30

## Decision

The embedded catalogue uses the operator-selected upstream sources without executing their shell scripts. Each source is translated into typed recipe fields and allowlisted Docker/vLLM arguments.

### Qwen 3.6 35B-A3B

- Source: `MiaAI-Lab/Unsloth-Qwen3.6-35b-NVFP4-DGX-Spark` at `79c9e6f359f6101cdacd0dfd6fe9861ae2493a4d`.
- Model: `unsloth/Qwen3.6-35B-A3B-NVFP4` at `739af1e7aac320af1682ed1e0cce369af4c5265d`, 26,529,057,596 bytes.
- ARM64 image: `ghcr.io/miaai-lab/mia-vllm-gb10-linear-b12x@sha256:19627342e1da2607f4db50745dca30e57d7dd0ebff06062f03fd69b43a252931`, 9,220,559,293 compressed layer bytes.
- Important settings: FlashInfer B12X linear backend with automatic MoE selection, FP8 KV cache, MTP two-token speculation, 80% GPU utilization, and a persistent manager-owned compilation cache.

This is recipe version 2 because it replaces the NVIDIA model and stock vLLM image recorded in ADR 0001.

### Qwen 3.6 27B

- Source: `MiaAI-Lab/Qwen3.6-27B-NVFP4-vLLM` at `f2e8f07f3813e62beb04761cf8715998f911df10`.
- Model: `nvidia/Qwen3.6-27B-NVFP4` at `0893e1606ff3d5f97a441f405d5fc541a6bdf404`, 21,941,623,844 bytes.
- Multi-platform image: `vllm/vllm-openai@sha256:251eba5cc7c12fed0b75da22a9240e582b1c9e39f6fbc064f86781b963bd814f`, whose Linux ARM64 child is `sha256:32445b36556244d8a721cd21a2b47a7915bc6408432d05aaeab205bb223ced8b` and has 10,617,617,438 compressed layer bytes.
- Important settings: the pinned source's `v0.24.0` launch profile, model-provided chat template, FlashInfer attention, Marlin MoE, MTP three-token speculation, and 40% GPU utilization.

The upstream README mentions a nightly image in places while its pinned `start.sh` selects `v0.24.0`. The typed recipe follows the executable source at the pinned commit and replaces the tag with its immutable digest.

### Laguna S 2.1

- Source and primary model: `poolside/Laguna-S-2.1-NVFP4` at `07614121b31898586430f189d27a25a0be310843`, 71,938,632,401 bytes.
- Drafter: `poolside/Laguna-S-2.1-DFlash-NVFP4` at `4cdcc6e9b29105e8ff5790885cadccbeb4f33f54`, 2,229,973,462 bytes.
- Multi-platform image: `vllm/vllm-openai@sha256:e4f88a835143cd22aee2397a26ec6bb80b3a4a6fe0c882bcbc63822904766089`, whose Linux ARM64 child is `sha256:2cc49b81319f7a66a33dd8bd63a7bfddae079122b33ce51989b6828a1f038c37` and has 10,238,912,364 compressed layer bytes.
- Important settings: vLLM 0.25.1, DFlash with 15 speculative tokens, poolside parsers, 32 maximum sequences, 85% GPU utilization, `MAX_JOBS=4`, and a persistent compilation cache.

The manager downloads and verifies both Laguna artifacts, mounts each read-only at a distinct container path, and calculates required disk space from their combined 74,168,605,863 bytes plus runtime and safety margin.

## Trust state

All recipes are `runonspark-candidate`. The Hugging Face revisions, byte totals, image manifests, and ARM64 availability were checked remotely on 2026-07-30. These checks do not establish that the images contain functioning GB10 kernels or that the complete lifecycle works on a DGX Spark.

Promotion to `runonspark-verified` requires the real-device receipts in PRD sections 15.1 and 15.2. If any image, model revision, argument, resource setting, or workaround changes during qualification, the recipe version must change.

## Consequences

- No upstream `start.sh`, remote shell, package installer, or mutable tag enters the execution path.
- The UI displays exact source provenance, combined artifact storage, and the candidate trust label.
- Laguna's cold compilation is capped and cached, but the selected stock ARM64 image may still need replacement with a purpose-built digest after device testing.
- Remote media fetching remains disabled even though the Qwen sources allow arbitrary media domains; clients can use inline image content without opening a server-side SSRF path.
