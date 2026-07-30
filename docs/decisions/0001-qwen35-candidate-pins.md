# ADR 0001: Qwen 35B candidate pins

Status: superseded by ADR 0002 before DGX Spark acceptance verification
Date: 2026-07-30

## Decision

The first embedded recipe pins:

- model repository `nvidia/Qwen3.6-35B-A3B-NVFP4` at Hugging Face commit `bafac91da4448b4c81b820e90a089d4ae953f5f8`;
- model snapshot size `23,462,477,838` bytes, calculated from the revision API with blob metadata;
- multi-platform image `vllm/vllm-openai@sha256:4cebac8c03f2cd9f5fabe72ac7c2a0b3aaa8450ef8f0e47429425fd1bfb83d42`;
- ARM64 child manifest `sha256:45e77edd10a4dae1040c27770892d9048db96a88ece07e9ca449e921af049374`;
- ARM64 compressed layer size `10,686,016,979` bytes.

The Docker registry returned the pinned multi-platform digest with an ARM64/Linux child on 2026-07-30. The model revision API returned the same requested immutable commit and per-blob SHA-256 metadata. The launch settings mirror NVIDIA's DGX Spark recommendation in the pinned model card.

## Trust state

The recipe is deliberately `runonspark-candidate`, not `runonspark-verified`. Registry existence and upstream metadata are supply inputs; they are not evidence that installation, restart recovery, vLLM readiness, inference, and removal all pass on a DGX Spark. Promotion requires the complete receipts in PRD section 15.1 and a new immutable recipe version if any pin or setting changes.

## Consequences

- The manager can exercise the full typed workflow during development.
- The UI must label the recipe as a candidate.
- Release packaging must not describe it as verified until real-device validation is recorded.
- The image is historical nightly content addressed immutably. `nightly` is never stored or pulled by the recipe.

The operator subsequently selected a different Qwen 35B artifact and runtime source. This record remains as the rationale for recipe version 1; it is not an active embedded recipe.
