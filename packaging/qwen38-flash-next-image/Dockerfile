# syntax=docker/dockerfile:1.7
# First-party SGLang image for Qwen3.8-Flash-Next-NVFP4 on DGX Spark
# (GB10 / SM121).
#
# Why this image exists: the stock lmsysorg/sglang:qwen38flashnext build
# cannot serve this model on SM121. Its QSA (Qwen sparse attention) backend
# resolves packed varlen attention to flash-attn-4's CuTe kernels, and those
# fail MLIR compilation on SM121. The launcher repository for this model
# ships a validated fix as a local docker build; basement recipes pin image
# bytes (ADR 0012), so this Dockerfile reproduces that build as a published,
# digest-pinned image instead.
#
# Provenance, all pinned:
# - Base: the model-specific SGLang build RadixArk publishes, by index
#   digest (multi-arch; the arm64 manifest is what a GB10 runs).
# - qsa_fa_fallback.py: byte-identical extraction from the launcher's
#   start.sh heredoc at the pinned commit (MIT licensed). Verify with:
#   sha256 195cebac561e2feae9f4fa7612a0094046a82651a07e175bcc14e0721d92fb2a
# - The RUN patch below is the launcher's own, unchanged: it inserts an
#   SM121-only import redirect into the QSA backend, guarded by the same
#   anchors, so a changed upstream layout fails the build instead of
#   silently shipping an unpatched image.
# - NCCL: the launcher's validated runtime preloads host-staged NCCL
#   2.30.7 on both nodes (its GLM-5.2 runbook pins >= 2.30.7 for CUDA
#   graphs + tensor parallel on GB10 + ConnectX-7; the base image bundles
#   2.29.7). Basement cannot mount host libraries into a recipe container,
#   so this image delivers the same library version as a pinned wheel.
#   Same bytes-of-interest, different delivery path; hardware
#   qualification confirms the combination.
FROM lmsysorg/sglang:qwen38flashnext@sha256:12d3392bdc8be8d35e9a95f191df6aef99c5114bdbefd41bfdc7e760e6d25ec1

LABEL org.opencontainers.image.source="https://github.com/punkjazz-labs/basement" \
      org.opencontainers.image.description="SGLang for Qwen3.8-Flash-Next-NVFP4 with reliable disconnect cleanup, the SM121 QSA Triton fallback and NCCL 2.30.7" \
      ai.basement.base.image="lmsysorg/sglang:qwen38flashnext@sha256:12d3392bdc8be8d35e9a95f191df6aef99c5114bdbefd41bfdc7e760e6d25ec1" \
      ai.basement.patch.source="https://github.com/MiaAI-Lab/Qwen3.8-Flash-Next-Dual-DGX-Sparks" \
      ai.basement.patch.revision="dccb035c559f342fe8c0f65eb427671c6cf60730" \
      ai.basement.patch.licence="MIT" \
      ai.basement.disconnect-patch.source="https://github.com/sgl-project/sglang/pull/35936" \
      ai.basement.disconnect-patch.revision="764f0b95c64456b67c3aa8a344aeb8308c23c24b" \
      ai.basement.disconnect-patch.licence="Apache-2.0"

RUN pip install --no-cache-dir --no-deps nvidia-nccl-cu13==2.30.7

COPY patch_abort_disconnected_requests.py /tmp/patch_abort_disconnected_requests.py
RUN python3 /tmp/patch_abort_disconnected_requests.py \
    && rm /tmp/patch_abort_disconnected_requests.py

COPY qsa_fa_fallback.py /sgl-workspace/sglang/python/sglang/srt/layers/attention/qsa_fa_fallback.py
RUN python3 - <<'PYEOF'
p = "/sgl-workspace/sglang/python/sglang/srt/layers/attention/qwen_sparse_attn_backend.py"
s = open(p).read()
anchor = "    try:\n        from flash_attn import flash_attn_varlen_func"
patch = (
    "    from sglang.srt.utils import is_sm100_supported\n"
    "    if not is_sm100_supported():\n"
    "        from sglang.srt.layers.attention.qsa_fa_fallback import triton_varlen_attn_func\n"
    "        return triton_varlen_attn_func\n"
) + anchor
assert anchor in s, "anchor not found - upstream image layout changed"
assert "qsa_fa_fallback" not in s, "already patched"
open(p, "w").write(s.replace(anchor, patch, 1))
print("qwen_sparse_attn_backend.py patched for SM121")
PYEOF
