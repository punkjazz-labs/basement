# vLLM image for GLM-5.3-Flash EXL3 on two GB10 nodes

This directory defines the arm64 vLLM image that the GLM-5.3-Flash EXL3
two-Spark recipe pins.

It exists for one reason. The launcher bakes every patch into its
published image except `patch_glm_video_placeholders.py`, which it
bind-mounts at container start. basement replaces the entrypoint and
mounts nothing, so a derivative image must apply that one patch at
build.

This is the EXL3 line of the launcher. The publisher also ships an NVFP4
sibling with a Ray TP=2 executor. That is a different artifact and a
different method, and this image does not cover it.

## What the patch does

The name says video, but only one half of the patch is about video.

1. **Video placeholder alignment.** `glm4_1v` builds one image block for
   each timestamp. A one second clip can give three timestamps while
   `video_grid_thw` T is 1, so vLLM assigns 100 encoder tokens to 300
   slots. The patch aligns the timestamp list to the encoder `grid_t`.
2. **GB10 `persistent_topk` disable.** The patch edits
   `sparse_attn_indexer_kpool.py` so that the decode path does not take
   the `persistent_topk` branch, and uses `top_k_per_row_decode`
   instead. The `persistent_topk` kernel oversubscribes GB10 shared
   memory on long sequences.

The second half is a text-path decode fix. This image is therefore
necessary for long-history text serving, not only for video prompts.

The patch persists because of how it installs itself. Run as `__main__`,
it copies itself to `dist-packages/glm53_video_patch.py`, writes
`dist-packages/glm53_video.pth` holding `import glm53_video_patch`, then
edits the kpool file and clears the stale `.pyc`. `site` runs every
`.pth` file at interpreter start, so the import hook reaches every later
process. This is why one `RUN` line at build time replaces the
launcher's three runtime bind mounts.

## Pins

- Base image: the launcher's own published runtime image,
  `ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks` by digest
  `sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58`.
  The `:exl3`, `:latest` and `:20260828-dflash2` tags all resolve to this
  digest. It is a single arm64 manifest, not a multi-arch index: 52
  layers and 9,788,994,117 bytes compressed.
- Patch source: `MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks` at commit
  `bd7f55edff9e37b41e1d32e2cf37054fe66d1e58`, MIT licence in tree.
- `patch_glm_video_placeholders.py` is a byte-identical extraction of
  `overlay/patch_glm_video_placeholders.py` at that commit, 6,095 bytes,
  sha256
  `b41f87832968a63000c9b56ac12948958ad36d1d0f93c031a2969243031aa82d`.
  The same bytes are already inside the base image at
  `/opt/glm53/patch_glm_video_placeholders.py`, and the build asserts
  that equality.

Licences. The launcher scripts, and therefore this patch, are MIT. The
model licences do not ride this image. The EXL3 checkpoint carries
ShapleyMCG License 1.0, which requires attribution and names an excluded
party, and the optional DFlash2 drafter carries CC BY-NC-ND 4.0. The
recipe carries and gates those, because that is where basement shows
licences to a user.

## What this image does not fix

At this base digest the image does **not** contain
`patch_suppress_stops_in_reasoning.py` or
`patch_scheduler_decode_floor.py`. Both files are absent from
`/opt/glm53`, and neither was ever run at build. The published image was
created at 2026-08-28T10:46:05+03:00, and the pinned launcher commit is
about ten hours later the same day, so the image is older than the
Dockerfile that commit holds.

This matters for basement and not for the launcher. The launcher mounts
both patches at container start and its inner script runs them.
basement replaces the entrypoint, so neither would ever run. The
scheduler patch is the mixed prefill fix that closed the launcher's
concurrency issue. Whether the recipe needs a newer base image, or these
two patches baked here as well, is an owner decision and a qualification
question. It is deliberately out of scope for this image, which covers
the one patch that the launcher never bakes at any commit.

## Build and verify locally

The base image is arm64 only, so build it on an arm64 host. The pull is
9,788,994,117 bytes compressed over 52 layers, and it needs a good deal
more than that once unpacked, so check free disk first.

```
docker build --tag basement-vllm-glm53-flash-exl3:local packaging/glm53-flash-image
```

The build fails loudly by design. It stops before any edit if the kpool
anchor is absent, if the anchor appears more than once, or if the base
image already carries the patch. It stops after the edit if the `.pth`,
the installed module or the kpool edit is not exactly as expected.

Probe a built image. The base sets `ENTRYPOINT ["vllm", "serve"]`, so
the probe must replace it. No GPU is needed.

```
docker run --rm --entrypoint python3 basement-vllm-glm53-flash-exl3:local -c \
  "import pathlib, glm53_video_patch; \
   s = pathlib.Path('/usr/local/lib/python3.12/dist-packages'); \
   print('pth:', (s / 'glm53_video.pth').read_text().strip()); \
   k = (s / 'vllm/model_executor/layers/sparse_attn_indexer_kpool.py').read_text(); \
   print('persistent_topk disabled:', 'if False and current_platform.is_cuda()' in k); \
   print('module:', glm53_video_patch.__file__)"
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
follows that model's launcher. If upstream adds the missing `RUN` line
to its own Dockerfile and republishes, the recipe moves to that image
and this directory is retired.
