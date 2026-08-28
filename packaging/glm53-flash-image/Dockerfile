# syntax=docker/dockerfile:1.7
# First-party vLLM image for GLM-5.3-Flash EXL3 4bpw on two DGX Sparks
# (GB10 / SM121).
#
# Why this image exists: the launcher bakes every patch into its published
# image except patch_glm_video_placeholders.py, which it bind-mounts at
# container start. basement replaces the entrypoint and mounts nothing, so a
# derivative image must apply that one patch at build.
#
# The patch does two things, and only one of them is about video. It aligns
# video timestamp blocks to the encoder grid_t, and it disables GB10
# persistent_topk in sparse_attn_indexer_kpool.py so that long-history decode
# uses top_k_per_row_decode. The second half is a text-path decode fix, so
# this image is necessary even for text-only serving.
#
# Provenance, all pinned:
# - Base: the launcher's own published runtime image, by digest. Its :exl3,
#   :latest and :20260828-dflash2 tags all resolve to this one digest.
# - patch_glm_video_placeholders.py: a byte-identical extraction from the
#   launcher at the pinned commit, MIT licensed. Verify with:
#   sha256 b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d
# - The patch itself is the launcher's own file, unchanged. The assertions
#   around it belong to this repository, because the patch accepts an
#   already-patched file and prints a message instead of stopping. A basement
#   image must fail loudly there, and never ship a silent no-op.
# - Model licences are not baked into this image. The checkpoint carries
#   ShapleyMCG 1.0 terms and the optional DFlash2 drafter carries
#   CC BY-NC-ND 4.0. Both ride the recipe, which is where basement shows and
#   gates licences.
FROM ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58

LABEL org.opencontainers.image.source="https://github.com/punkjazz-labs/basement" \
      org.opencontainers.image.description="vLLM for GLM-5.3-Flash EXL3 4bpw on two GB10 nodes, with the video placeholder and GB10 persistent_topk patch applied at build" \
      ai.basement.base.image="ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58" \
      ai.basement.patch.source="https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks" \
      ai.basement.patch.revision="bd7f55edff9e37b41e1d32e2cf37054fe66d1e58" \
      ai.basement.patch.file="patch_glm_video_placeholders.py" \
      ai.basement.patch.sha256="b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d" \
      ai.basement.patch.licence="MIT" \
      ai.basement.model.licence="not in this image; the checkpoint and drafter licences ride the recipe"

# The launcher already ships this file at /opt/glm53. This copy goes to a
# separate path on purpose, so that the step below can compare the two and
# prove that the committed bytes equal the bytes the launcher ships.
COPY patch_glm_video_placeholders.py /opt/basement/patch_glm_video_placeholders.py

# Stage 1, before any edit: check every anchor the patch depends on.
RUN python3 - <<'PYEOF'
import hashlib
from pathlib import Path

PINNED_SHA256 = "b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d"
OURS = Path("/opt/basement/patch_glm_video_placeholders.py")
THEIRS = Path("/opt/glm53/patch_glm_video_placeholders.py")
KPOOL = Path(
    "/usr/local/lib/python3.12/dist-packages/vllm/model_executor/layers/"
    "sparse_attn_indexer_kpool.py"
)
PTH = Path("/usr/local/lib/python3.12/dist-packages/glm53_video.pth")
# These two literals must stay equal to KPOOL_OLD and KPOOL_NEW in the patch.
KPOOL_OLD = "if current_platform.is_cuda() and select_k in (512, 1024, 2048):"
KPOOL_NEW = (
    "if False and current_platform.is_cuda() and "
    "select_k in (512, 1024, 2048):  # GB10 persistent_topk smem"
)

ours = OURS.read_bytes()
got = hashlib.sha256(ours).hexdigest()
assert got == PINNED_SHA256, f"patch sha256 is {got}, not the pinned value"

assert THEIRS.is_file(), "the base image no longer ships the patch at /opt/glm53"
assert THEIRS.read_bytes() == ours, (
    "the base image ships a different patch than this repository pins - "
    "extract the patch again at the new launcher commit"
)

assert KPOOL.is_file(), f"anchor file is absent: {KPOOL}"
kpool = KPOOL.read_text()
assert KPOOL_NEW not in kpool, (
    "already patched - the base image now disables persistent_topk itself, "
    "so this image is redundant and must be reviewed"
)
assert kpool.count(KPOOL_OLD) == 1, (
    f"anchor found {kpool.count(KPOOL_OLD)} times, expected exactly 1 - "
    "upstream image layout changed"
)
assert not PTH.exists(), (
    "already patched - the base image now installs glm53_video.pth itself"
)
print("glm53 preflight ok: anchor present once, nothing applied yet")
PYEOF

# Stage 2: run the launcher's patch as __main__. That copies it to
# dist-packages as glm53_video_patch.py, writes glm53_video.pth next to it,
# and edits the kpool file. site runs every .pth at interpreter start, so the
# import hook survives into every later process, including the one basement
# starts when it replaces the entrypoint.
RUN python3 /opt/basement/patch_glm_video_placeholders.py

# Stage 3, after the edit: prove the patch landed in this image layer.
RUN python3 - <<'PYEOF'
import hashlib
from pathlib import Path

PINNED_SHA256 = "b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d"
SITE = Path("/usr/local/lib/python3.12/dist-packages")
KPOOL = SITE / "vllm/model_executor/layers/sparse_attn_indexer_kpool.py"
KPOOL_OLD = "if current_platform.is_cuda() and select_k in (512, 1024, 2048):"
KPOOL_NEW = (
    "if False and current_platform.is_cuda() and "
    "select_k in (512, 1024, 2048):  # GB10 persistent_topk smem"
)

pth = SITE / "glm53_video.pth"
assert pth.is_file(), "glm53_video.pth was not written"
assert pth.read_text() == "import glm53_video_patch\n", (
    f"glm53_video.pth holds unexpected content: {pth.read_text()!r}"
)

module = SITE / "glm53_video_patch.py"
assert module.is_file(), "glm53_video_patch.py was not installed"
got = hashlib.sha256(module.read_bytes()).hexdigest()
assert got == PINNED_SHA256, f"installed module sha256 is {got}, not the pinned value"

kpool = KPOOL.read_text()
assert kpool.count(KPOOL_NEW) == 1, "persistent_topk edit did not land"
assert KPOOL_OLD not in kpool, "the original persistent_topk line is still present"
# The launcher asserts the same way after its own edits: a file that no
# longer compiles would only fail much later, at serve time.
compile(kpool, "sparse_attn_indexer_kpool.py", "exec")

# site executes the .pth at interpreter start, so the module must import
# cleanly on its own. This needs no GPU.
import glm53_video_patch  # noqa: F401

print("glm53 postflight ok: pth installed, persistent_topk disabled, module imports")
PYEOF
