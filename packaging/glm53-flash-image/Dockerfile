# syntax=docker/dockerfile:1.7
# First-party vLLM image for GLM-5.3-Flash EXL3 4bpw on two DGX Sparks
# (GB10 / SM121).
#
# Why this image exists: a launcher user's effective runtime is the
# published image PLUS every patch start.sh bind-mounts at container
# start. The launcher mounts three patches and runs them in its inner
# script. basement replaces the entrypoint and mounts nothing, so none of
# the three would ever run. This image bakes all three at build, so that
# a basement container equals a launcher container in effective patches.
#
# The published image is also older than the launcher commit pinned
# below, by about ten hours. Two of the three patches are therefore not
# merely unexecuted in it: their files are absent from it altogether.
# See the README for the evidence.
#
# The three patches, in the order the launcher's inner script runs them:
#
# 1. patch_glm_video_placeholders.py. Two effects, and only one is about
#    video. It aligns video timestamp blocks to the encoder grid_t, and
#    it disables GB10 persistent_topk in sparse_attn_indexer_kpool.py so
#    that long-history decode uses top_k_per_row_decode. The second is a
#    text-path decode fix.
# 2. patch_suppress_stops_in_reasoning.py. Keeps client stop strings
#    dormant until </think>, so a request whose reasoning restates a
#    harness stop does not finish with empty content.
# 3. patch_scheduler_decode_floor.py. Stops a decode lane sharing an
#    engine step with a long sparse-MLA prefill. This is the mixed
#    prefill fix that closed the launcher's concurrency issue 6.
#
# Patches 2 and 3 stay tunable after baking, because each reads its
# environment variable at serve time, not at build time:
# GLM53_SUPPRESS_STOPS_IN_REASONING and GLM53_MIXED_PREFILL_CHUNK.
#
# Provenance, all pinned:
# - Base: the launcher's own published runtime image, by digest. Its
#   :exl3, :latest and :20260828-dflash2 tags all resolve to this digest.
# - The three patch files are byte-identical extractions from the
#   launcher at the pinned commit, MIT licensed. Their sha256 values are
#   in the labels below and in the README.
# - The patches are the launcher's own files, unchanged. The assertions
#   around them belong to this repository, because all three accept an
#   already-patched target and exit 0. A basement image must fail loudly
#   there, and never ship a silent no-op.
# - The assertions import each patch module and use its own anchor
#   constants, so an assertion cannot drift from what the patch itself
#   looks for.
# - Model licences are not baked into this image. The checkpoint carries
#   ShapleyMCG License 1.0 terms and the optional DFlash2 drafter carries
#   CC BY-NC-ND 4.0. Both ride the recipe, which is where basement shows
#   and gates licences.
FROM ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58

LABEL org.opencontainers.image.source="https://github.com/punkjazz-labs/basement" \
      org.opencontainers.image.description="vLLM for GLM-5.3-Flash EXL3 4bpw on two GB10 nodes, with all three launcher runtime patches applied at build" \
      ai.basement.base.image="ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58" \
      ai.basement.patch.source="https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks" \
      ai.basement.patch.revision="bd7f55edff9e37b41e1d32e2cf37054fe66d1e58" \
      ai.basement.patch.video.sha256="b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d" \
      ai.basement.patch.suppress_stops.sha256="14602ea4350bad1eb8a6e76de3e17e2d5ef1229340bcd199351e20334f5e15d7" \
      ai.basement.patch.scheduler.sha256="0e117f2c8210d674e79d98a34a26e8b5dc6f956bfb566e3cd3a830b37f6e76de" \
      ai.basement.patch.licence="MIT" \
      ai.basement.model.licence="not in this image; the checkpoint and drafter licences ride the recipe"

# The launcher already ships the video patch at /opt/glm53. These copies
# go to a separate directory on purpose, so that the preflight can
# compare the two and prove the committed bytes equal the shipped bytes.
COPY patch_glm_video_placeholders.py /opt/basement/patch_glm_video_placeholders.py
COPY patch_suppress_stops_in_reasoning.py /opt/basement/patch_suppress_stops_in_reasoning.py
COPY patch_scheduler_decode_floor.py /opt/basement/patch_scheduler_decode_floor.py

# Stage 1, before any edit: check every anchor all three patches depend
# on. This runs before the first edit on purpose. A missing anchor in the
# third patch must not leave a half-patched image behind.
RUN python3 - <<'PYEOF'
import hashlib
import sys
from pathlib import Path

sys.path.insert(0, "/opt/basement")
# Importing each patch gives us its own anchor constants, so these
# assertions cannot drift from what the patch will look for.
import patch_glm_video_placeholders as vid
import patch_scheduler_decode_floor as sch
import patch_suppress_stops_in_reasoning as sup

SHA256 = {
    "patch_glm_video_placeholders.py":
        "b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d",
    "patch_suppress_stops_in_reasoning.py":
        "14602ea4350bad1eb8a6e76de3e17e2d5ef1229340bcd199351e20334f5e15d7",
    "patch_scheduler_decode_floor.py":
        "0e117f2c8210d674e79d98a34a26e8b5dc6f956bfb566e3cd3a830b37f6e76de",
}

for name, want in SHA256.items():
    ours = Path("/opt/basement") / name
    got = hashlib.sha256(ours.read_bytes()).hexdigest()
    assert got == want, f"{name} sha256 is {got}, not the pinned value"
    # The base image ships only the video patch today. If a newer base
    # ships any of them, its bytes must equal what this repository pins.
    theirs = Path("/opt/glm53") / name
    if theirs.is_file():
        assert theirs.read_bytes() == ours.read_bytes(), (
            f"the base image ships a different {name} than this repository "
            "pins - extract the patches again at the new launcher commit"
        )

# --- 1. video placeholders and GB10 persistent_topk ---------------------
assert vid.KPOOL.is_file(), f"anchor file is absent: {vid.KPOOL}"
kpool = vid.KPOOL.read_text()
assert vid.KPOOL_NEW not in kpool, (
    "already patched - the base image now disables persistent_topk itself, "
    "so this image is redundant and must be reviewed"
)
assert kpool.count(vid.KPOOL_OLD) == 1, (
    f"kpool anchor found {kpool.count(vid.KPOOL_OLD)} times, expected 1 - "
    "upstream image layout changed"
)
assert not Path(
    "/usr/local/lib/python3.12/dist-packages/glm53_video.pth"
).exists(), "already patched - the base image now installs glm53_video.pth"

# --- 2. suppress stops while reasoning ---------------------------------
assert sup.P.is_file(), f"anchor file is absent: {sup.P}"
detok = sup.P.read_text()
assert not (sup.MARK in detok and "_maybe_enable_reasoning_stop_guard" in detok), (
    "already patched - the base image now suppresses stops in reasoning, "
    "so this image is redundant and must be reviewed"
)
for label, anchor in (
    ("import", sup.IMPORT_OLD),
    ("factory", sup.FACTORY_OLD),
    ("init", sup.INIT_OLD),
    ("stop", sup.STOP_OLD),
):
    assert detok.count(anchor) == 1, (
        f"detokenizer {label} anchor found {detok.count(anchor)} times, "
        "expected 1 - upstream image layout changed"
    )

# --- 3. scheduler decode floor -----------------------------------------
assert sch.P.is_file(), f"anchor file is absent: {sch.P}"
sched = sch.P.read_text()
assert sch.MARK not in sched, (
    "already patched - the base image now carries the decode floor, "
    "so this image is redundant and must be reviewed"
)
assert "def _glm53_mixed_prefill_policy(" not in sched, (
    "already patched - the decode floor helper is present"
)
# This needle is a literal inside the patch's main(), not a constant.
NEEDLE = "from vllm.compilation.cuda_graph import CUDAGraphStat\n"
for label, anchor in (
    ("running-prefill", sch.RUNNING_OLD),
    ("waiting-prefill", sch.WAITING_OLD),
    ("helper insert point", NEEDLE),
    ("import", sch.IMPORT_OLD),
):
    assert sched.count(anchor) == 1, (
        f"scheduler {label} anchor found {sched.count(anchor)} times, "
        "expected 1 - upstream image layout changed"
    )

print("glm53 preflight ok: 3 patches, every anchor present once, none applied")
PYEOF

# Stage 2: run the three patches, in the order the launcher's inner
# script runs them.
#
# The video patch persists through site-packages rather than through an
# edit alone. Run as __main__ it copies itself to dist-packages as
# glm53_video_patch.py and writes glm53_video.pth beside it. site runs
# every .pth at interpreter start, so the import hook reaches every later
# process, including the one basement starts when it replaces the
# entrypoint. The other two edit vLLM on disk, so they persist directly.
RUN python3 /opt/basement/patch_glm_video_placeholders.py
RUN python3 /opt/basement/patch_suppress_stops_in_reasoning.py
RUN python3 /opt/basement/patch_scheduler_decode_floor.py

# Stage 3, after the edits: prove all three landed in the image layers.
RUN python3 - <<'PYEOF'
import hashlib
import sys
from pathlib import Path

sys.path.insert(0, "/opt/basement")
import patch_glm_video_placeholders as vid
import patch_scheduler_decode_floor as sch
import patch_suppress_stops_in_reasoning as sup

SITE = Path("/usr/local/lib/python3.12/dist-packages")

# --- 1. video placeholders and GB10 persistent_topk ---------------------
pth = SITE / "glm53_video.pth"
assert pth.is_file(), "glm53_video.pth was not written"
assert pth.read_text() == "import glm53_video_patch\n", (
    f"glm53_video.pth holds unexpected content: {pth.read_text()!r}"
)
module = SITE / "glm53_video_patch.py"
assert module.is_file(), "glm53_video_patch.py was not installed"
got = hashlib.sha256(module.read_bytes()).hexdigest()
assert got == "b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d", (
    f"installed module sha256 is {got}, not the pinned value"
)
kpool = vid.KPOOL.read_text()
assert kpool.count(vid.KPOOL_NEW) == 1, "persistent_topk edit did not land"
assert vid.KPOOL_OLD not in kpool, "the original persistent_topk line is still present"

# --- 2. suppress stops while reasoning ---------------------------------
detok = sup.P.read_text()
assert sup.MARK in detok, "suppress-stops marker did not land"
assert "_maybe_enable_reasoning_stop_guard" in detok, (
    "suppress-stops guard method did not land"
)
assert sup.FACTORY_OLD not in detok, "the original detokenizer factory is still present"

# --- 3. scheduler decode floor -----------------------------------------
sched = sch.P.read_text()
assert sch.MARK in sched, "decode floor marker did not land"
assert "def _glm53_mixed_prefill_policy(" in sched, "decode floor helper did not land"
assert sch.RUNNING_OLD not in sched, "the original running-prefill block is still present"
assert sch.WAITING_OLD not in sched, "the original waiting-prefill block is still present"

# A file that no longer compiles would only fail much later, at serve
# time. The launcher asserts the same way after its own edits.
for path, text in ((vid.KPOOL, kpool), (sup.P, detok), (sch.P, sched)):
    compile(text, path.name, "exec")

# site executes the .pth at interpreter start, so the module must import
# cleanly on its own. This needs no GPU.
import glm53_video_patch  # noqa: F401

print("glm53 postflight ok: 3 patches applied, all targets compile, module imports")
PYEOF
