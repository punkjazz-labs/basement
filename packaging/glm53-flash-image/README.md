# vLLM image for GLM-5.3-Flash EXL3 on two GB10 nodes

This directory defines the arm64 vLLM image that the GLM-5.3-Flash EXL3
two-Spark recipe pins.

It exists because a launcher user's effective runtime is not the
published image alone. It is the published image **plus every patch
`start.sh` bind-mounts** at container start, which the launcher's inner
script runs before `exec vllm serve`. The launcher mounts three patches.
basement replaces the entrypoint and mounts nothing, so none of the
three would ever run. This image bakes all three at build, so that a
basement container equals a launcher container in effective patches.

This is the EXL3 line of the launcher. The publisher also ships an NVFP4
sibling with a Ray TP=2 executor. That is a different artifact and a
different method, and this image does not cover it.

## The published image is older than the pinned commit

This is the discovery that set the shape of this image, and it is worth
keeping on the record.

The Round 7 research recorded `patch_suppress_stops_in_reasoning.py` and
`patch_scheduler_decode_floor.py` as already baked into the published
image, and their runtime mounts as belt and braces. The image bytes
disagree. Both files are **absent from the image altogether**, and
neither was ever run at build.

The cause is a timing gap. The published image was created at
`2026-08-28T10:46:05+03:00`. The pinned launcher commit `bd7f55ed` is
dated `2026-08-28 20:34:54 +0300`, about ten hours later. The image was
therefore built from an earlier commit than the Dockerfile that the
research read. The launcher hides the gap completely, because it mounts
all three patches and runs them at container start whatever the image
contains. basement cannot, so the gap becomes real for basement, and
this image closes it.

Evidence, from the image's own bytes rather than from the launcher
source: the config blob's build history contains no `RUN` for any of the
three patches, and `/opt/glm53` in the image holds exactly ten files, of
which only `patch_glm_video_placeholders.py` is one of the three.

## Every runtime mount, and what this image does with it

Enumerated from `start.sh` at the pinned commit. The head block and the
worker block mount the same set; the worker copies reach `/tmp` by `scp`
first.

| Mounted at container start | Kind | Already in the image | This image |
|---|---|---|---|
| HF cache to `/root/.cache/huggingface` | cache | not applicable | not baked. basement stages artifacts per node |
| vLLM cache to `/root/.cache/vllm` | cache | not applicable | not baked |
| Triton cache to `/root/.triton/cache` | cache | not applicable | not baked |
| TileLang cache to `/root/.tilelang/cache` | cache | not applicable | not baked |
| `start.sh` to `/start.sh` | inner script | no | not baked. basement replaces the entrypoint, and the recipe carries the serve flags |
| `chat_template.jinja` to `/opt/glm53/chat_template.jinja` | data | yes, and byte-identical | not baked, because nothing differs. See below |
| `patch_glm_video_placeholders.py` | executed patch | file present, never run | **baked** |
| `patch_suppress_stops_in_reasoning.py` | executed patch | **file absent** | **baked** |
| `patch_scheduler_decode_floor.py` | executed patch | **file absent** | **baked** |
| host NCCL dir to `/nccl` with `LD_PRELOAD` | conditional library | no | not baked. Only when `USE_HOST_NCCL=1`, and the default is 0 |

Two rows deserve their reasoning in full.

**The chat template is not a gap.** The launcher mounts its template
over `/opt/glm53/chat_template.jinja`, which the image already carries.
The two are byte-identical at the pinned commit, 10,970 bytes, sha256
`96ed83160b243de213e95eb2fa19bde4ac13b676661cfec477d18e45e9fcca3a`, so
the mount changes nothing and there is nothing to bake. The separate
question of whether a basement recipe can point `--chat-template` at an
image-resident path is a schema question, and it belongs to the recipe.

**Host NCCL is deliberately not baked.** The mount happens only when
`USE_HOST_NCCL=1`, and `start.sh` defaults it to 0 so the image's own
NCCL is used. The launcher also warns that preloading the host 2.30.7
build makes DeepEP assert a duplicate NCCL. Baking it would leave the
validated default path, not match it.

## The three patches

In the order the launcher's inner script runs them, which is the order
this image applies them.

1. **`patch_glm_video_placeholders.py`.** Two effects, and only one is
   about video. It aligns video timestamp blocks to the encoder `grid_t`,
   because `glm4_1v` builds one image block for each timestamp and a one
   second clip can give three timestamps while `video_grid_thw` T is 1,
   so vLLM assigns 100 encoder tokens to 300 slots. It also disables GB10
   `persistent_topk` in `sparse_attn_indexer_kpool.py`, so long-history
   decode uses `top_k_per_row_decode` instead of a kernel that
   oversubscribes GB10 shared memory. The second effect is a text-path
   decode fix, so this patch matters even for text-only serving.
2. **`patch_suppress_stops_in_reasoning.py`.** Keeps client stop strings
   dormant until `</think>`. vLLM v1 matches `stop` against the whole
   stream, and reasoning often restates a harness stop such as
   `Question:`, which finishes the request mid-reason with empty
   content. It edits `v1/engine/detokenizer.py`.
3. **`patch_scheduler_decode_floor.py`.** Stops a decode lane sharing an
   engine step with a long sparse-MLA prefill, which drops decode from
   about 50 tok/s to about 5. This is the mixed prefill fix that closed
   the launcher's concurrency issue 6. It edits `v1/core/sched/scheduler.py`.

Patches 2 and 3 stay tunable after baking, because each reads its
environment variable at serve time and not at build time:
`GLM53_SUPPRESS_STOPS_IN_REASONING` and `GLM53_MIXED_PREFILL_CHUNK`.
Baking sets the launcher's validated default, and a recipe can still
override it.

## Pins

- Base image: the launcher's own published runtime image,
  `ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks` by digest
  `sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58`.
  The `:exl3`, `:latest` and `:20260828-dflash2` tags all resolve to this
  digest. It is a single arm64 manifest, not a multi-arch index: 52
  layers and 9,788,994,117 bytes compressed.
- Patch source: `MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks` at commit
  `bd7f55edff9e37b41e1d32e2cf37054fe66d1e58`, MIT licence in tree. All
  three files are byte-identical extractions of `overlay/<name>` at that
  commit.

| Patch file | Bytes | sha256 |
|---|---:|---|
| `patch_glm_video_placeholders.py` | 6,095 | `b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d` |
| `patch_suppress_stops_in_reasoning.py` | 8,030 | `14602ea4350bad1eb8a6e76de3e17e2d5ef1229340bcd199351e20334f5e15d7` |
| `patch_scheduler_decode_floor.py` | 5,351 | `0e117f2c8210d674e79d98a34a26e8b5dc6f956bfb566e3cd3a830b37f6e76de` |

The video patch's bytes are also inside the base image at
`/opt/glm53/patch_glm_video_placeholders.py`, and the build asserts that
equality. The other two are absent from the base image, so the build
asserts equality only if a later base starts shipping them.

Licences. The launcher scripts, and therefore all three patches, are
MIT. The model licences do not ride this image. The EXL3 checkpoint
carries ShapleyMCG License 1.0, which requires attribution and names an
excluded party, and the optional DFlash2 drafter carries CC BY-NC-ND
4.0. The recipe carries and gates those, because that is where basement
shows licences to a user.

## Build and verify locally

The base image is arm64 only, so build it on an arm64 host. The pull is
9,788,994,117 bytes compressed over 52 layers, and it needs a good deal
more than that once unpacked, so check free disk first.

```
docker build --tag basement-vllm-glm53-flash-exl3:local packaging/glm53-flash-image
```

The build fails loudly by design. A single preflight stage runs before
any edit, so a drifted anchor in the third patch cannot leave a
half-patched image behind. It stops if any patch file's sha256 is not
the pinned one, if any anchor is missing or is not unique, or if any
target already carries a patch. A postflight stage then stops if any
edit did not land, if any edited file no longer compiles, or if the
`.pth` is not exactly as expected. The assertions import each patch
module and use its own anchor constants, so an assertion cannot drift
from what the patch itself looks for.

Probe a built image. The base sets `ENTRYPOINT ["vllm", "serve"]`, so
the probe must replace it. No GPU is needed.

```
docker run --rm --entrypoint python3 basement-vllm-glm53-flash-exl3:local -c \
  "import pathlib, glm53_video_patch; \
   s = pathlib.Path('/usr/local/lib/python3.12/dist-packages'); \
   print('pth:', (s / 'glm53_video.pth').read_text().strip()); \
   k = (s / 'vllm/model_executor/layers/sparse_attn_indexer_kpool.py').read_text(); \
   print('persistent_topk disabled:', 'if False and current_platform.is_cuda()' in k); \
   d = (s / 'vllm/v1/engine/detokenizer.py').read_text(); \
   print('stops suppressed in reasoning:', '# [suppress-stops-in-reasoning]' in d); \
   c = (s / 'vllm/v1/core/sched/scheduler.py').read_text(); \
   print('decode floor:', '# [glm53-decode-floor]' in c)"
```

## Rebuild and publish

CI owns the build, for the same reason the ComfyUI and
Qwen3.8-Flash-Next images do: the published bytes stay traceable to a
public build of this committed Dockerfile. Push a tag of the form
`glm53-flash-image-v*`, or run the workflow by hand, and
`.github/workflows/glm53-flash-image.yml` builds it on an arm64 runner
and pushes
`ghcr.io/punkjazz-labs/basement-vllm-glm53-flash-exl3:<version>`. The
digest a recipe pins is printed at the end of the workflow log.

## What this image is not

It is not a general vLLM build. It serves exactly one model family and
follows that model's launcher. If upstream republishes an image that
bakes all three patches itself, the recipe moves to that image and this
directory is retired. The preflight is written to fail in exactly that
case, rather than to ship a silent no-op.
