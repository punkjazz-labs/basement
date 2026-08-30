# SGLang image for Qwen3.8-Flash-Next on GB10

This directory defines the arm64 SGLang image that the
Qwen3.8-Flash-Next two-Spark recipe pins. It exists because the stock
`lmsysorg/sglang:qwen38flashnext` image cannot serve this model on a DGX
Spark: the QSA sparse-attention backend resolves to flash-attn-4 CuTe
kernels that fail MLIR compilation on SM121. The model's launcher
repository ships a validated fix as a local docker build. Basement
recipes pin image bytes (ADR 0012), so this Dockerfile turns that fix
into a published, digest-pinned image, the same way
`packaging/comfyui-image` does for media generation.

## Pins

- Base image: `lmsysorg/sglang:qwen38flashnext` by index digest
  `sha256:12d3392bdc8be8d35e9a95f191df6aef99c5114bdbefd41bfdc7e760e6d25ec1`
  (RadixArk's model-specific build of 2026-08-26; its own OCI labels name
  sgl-project/sglang commit `d91c3682` plus overlay commits
  `3ea3a37a1,12070370f` from Qiaolin-Yu/sglang-qwen-next#38).
- Patch source: `MiaAI-Lab/Qwen3.8-Flash-Next-Dual-DGX-Sparks` at commit
  `dccb035c559f342fe8c0f65eb427671c6cf60730`, MIT licence in tree.
- `qsa_fa_fallback.py` is a byte-identical extraction of that repository's
  `start.sh` heredoc at the pinned commit, sha256
  `195cebac561e2feae9f4fa7612a0094046a82651a07e175bcc14e0721d92fb2a`.
  The `RUN` patch step is the launcher's own Dockerfile content, with the
  same two anchor assertions, so an upstream layout change fails the
  build loudly.
- NCCL: `nvidia-nccl-cu13==2.30.7` from PyPI. The launcher's validated
  runtime preloads host-staged NCCL 2.30.7 on both nodes because its
  GLM-5.2 runbook pins >= 2.30.7 for CUDA graphs plus tensor parallel on
  GB10 with ConnectX-7, and the base image bundles 2.29.7. A basement
  recipe cannot mount host libraries, so this image installs the same
  library version as a pinned wheel. The library version equals the
  validated one; the delivery path differs. Hardware qualification
  decides whether the combination holds.
- Disconnect cleanup: SGLang pull request 35936 at commit
  `764f0b95c64456b67c3aa8a344aeb8308c23c24b`, Apache-2.0. The pinned base
  removes tokenizer request state before its scheduler abort can run when an
  HTTP stream disconnects. `patch_abort_disconnected_requests.py` applies the
  proposed abort-before-discard ordering with exact anchors, so a cancelled
  Codex or OpenAI request does not keep consuming the scheduler until its token
  limit. A base that has changed or already contains the patch fails the image
  build rather than silently applying a partial edit.

## What this image is not

It is not a general SGLang build. It serves exactly one model family and
follows that model's launcher. When upstream bakes SM121 QSA support into
a published image, the recipe moves to that image and this directory is
retired.

## Rebuild and publish

CI owns the build, for the same reason the ComfyUI image does: the
published bytes stay traceable to a public build of this committed
Dockerfile. Push a tag of the form `qwen38-flash-next-image-v*` (or run
the workflow by hand) and `.github/workflows/qwen38-flash-next-image.yml`
builds it on an arm64 runner and pushes
`ghcr.io/punkjazz-labs/basement-sglang-qwen38-flash-next:<version>`.
The digest a recipe pins is printed at the end of the workflow log.
