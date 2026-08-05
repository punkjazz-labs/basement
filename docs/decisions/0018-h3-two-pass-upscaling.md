# ADR 0018: Evaluate two-pass latent upscaling for MiniMax H3

Date: 2026-08-05. Status: rejected. Measured on hardware, then rejected on quality.

## Outcome, 2026-08-05: rejected

Measured, then watched, then rejected. The speed was real and the picture was not.

| Run | Wall time | Peak memory |
|---|---|---|
| native 1920 x 1088 | 3034 s | 96.1 GB |
| two-pass 1920 x 1088 | 1753 s | 91.8 GB |

Two-pass finished in 0.58 of native's time and used slightly less memory. The
native run came in 0.4 percent from the 3021 s recorded independently in
docs/H3-MEASUREMENTS.md, so the harness was measuring the same thing the
original method measured.

The owner watched both files and rejected it: visible artifacts in the video
and in the audio. Not usable. The QHD leg was stopped rather than finished,
because a faster route to an unusable picture does not become acceptable by
being measured at a second resolution.

Two findings worth keeping.

First, the deciding measurement named at the top of this ADR was the wrong
instrument. It assumed two-pass would produce the same video more cheaply, so a
ratio would settle it. It does not. Sampling low, upscaling the latents,
re-noising and sampling again is a different denoising trajectory, so the same
seed produces a different scene: different composition, different light,
different subject pose. There is no shared content to compare, which means the
softness question this ADR was built to answer could not have been answered by
that pairing at all. A ratio was never going to decide this. Watching it did.

Second, a single frame is not enough to judge either. A still from the two-pass
run looked better than the native one, sharper and better composed. The
artifacts the owner rejected it for are temporal and audible, and neither
survives into a screenshot. Any future evaluation of a generation shortcut has
to be judged on played video with sound, by a person, not on frames or metrics.

The candidate node was never vendored, which is what made stopping cheap. It
was fetched at a pinned commit for the test and nothing depends on it. Its
licence problem, no licence at all, turned out not to matter.

NVIDIA Sol Engine remains watch-only for the reason recorded below: no released
code as of 2026-08-05.

## Question

Can MiniMax H3 produce an acceptable 1920 x 1088 or 2560 x 1440 video
faster by sampling at a smaller canvas, spatially upscaling its video latent,
and completing the noise schedule at the output canvas?

This ADR does not claim a speedup. The two-pass path has not run on a GB10.
It records the candidate implementation, the licence boundary, the hardware
test, and the result that would decide whether basement should pursue its own
implementation.

## Why this is worth one test

The current one-GB10 results are measured in
[`docs/H3-MEASUREMENTS.md`](../H3-MEASUREMENTS.md). At 124 frames, the native
1920 x 1088 run took 3021 seconds and the native 2560 x 1440 run took 7750
seconds. The latter canvas has 1.8 times the pixels and took 2.6 times as
long. Its measured peak was 99.09 GB on a machine with 127,599,384 KB of
unified memory. The clock is the constraint in the measured range.

Those results make spatial work early in the schedule a plausible target.
They do not show that latent interpolation preserves detail or that a second
sampler pass saves time. Both remain unproven.

## Candidate source reviewed

The candidate is
[`Tr1dae/ComfyUI-MiniMaxH3_LatentUpscaler`](https://github.com/Tr1dae/ComfyUI-MiniMaxH3_LatentUpscaler).
The following files were fetched from `raw.githubusercontent.com` and read at
commit `2b4f7d6e2edf5ac3c1c553efac9d373aeafa59bd` on 2026-08-05:

- [`__init__.py`](https://raw.githubusercontent.com/Tr1dae/ComfyUI-MiniMaxH3_LatentUpscaler/2b4f7d6e2edf5ac3c1c553efac9d373aeafa59bd/__init__.py)
- [`nodes.py`](https://raw.githubusercontent.com/Tr1dae/ComfyUI-MiniMaxH3_LatentUpscaler/2b4f7d6e2edf5ac3c1c553efac9d373aeafa59bd/nodes.py)
- [`utils.py`](https://raw.githubusercontent.com/Tr1dae/ComfyUI-MiniMaxH3_LatentUpscaler/2b4f7d6e2edf5ac3c1c553efac9d373aeafa59bd/utils.py)
- [`README.md`](https://raw.githubusercontent.com/Tr1dae/ComfyUI-MiniMaxH3_LatentUpscaler/2b4f7d6e2edf5ac3c1c553efac9d373aeafa59bd/README.md)

The GitHub API reported 101 stars, 6 forks, 8 commits, a repository size of
28 KB, and a last push on 2026-08-03 when checked on 2026-08-05. It reported
no licence. There is no licence grant in the source files. This code cannot
be copied into basement, added to its image, or vendored into this repository.
The evaluation script downloads the pinned source transiently for the test
and removes its copy when the remote runner exits.

If the experiment passes, basement must write an independent implementation
of the technique. It must not derive that implementation by copying this
source.

## What the node actually does

### Latent layout and spatial interpolation

The implementation assumes that a MiniMax H3 audio-video `NestedTensor` has
the video tensor first and the audio tensor second:

```text
video [B, 24, T, H/16, W/16]
audio [B, 32, 2, T_audio]
```

These channel counts and positions are comments and assumptions. The code
does not validate them. It treats member zero as video. If at least two
members exist, it treats members zero and one as the video and audio streams
for re-noising.

For a five-dimensional video latent, the code folds batch and time into one
batch dimension, calls `torch.nn.functional.interpolate` on the last two
dimensions, and then restores the original layout. It offers nearest,
bilinear, and bicubic interpolation. Bilinear and bicubic use
`align_corners=False`. The node default is bilinear.

The requested latent height and width are rounded after multiplication by
`scale_by`, then rounded upward to an even value. The even grid is required
by MiniMax's `(1, 2, 2)` DiT patch size. Only the video member is spatially
interpolated. The audio member and any later members keep their shapes. A
video-shaped noise mask is interpolated by the same path.

The optional conditioning path separately scales visual latents in
`minimax_refs` and `minimax_keyframes`. It updates reference `latent_h`,
`latent_w`, and `latent_t` metadata when present. Audio-only reference fields
are left alone. The caller must build a new guider from the returned
conditioning for pass two.

### Re-noising and the second pass

The workflow takes pass one's `denoised_output`, not its ordinary output. The
node creates fresh noise through the supplied ComfyUI `NOISE` object. For
every stream selected for re-noising it calls the model's
`noise_scaling(sigmas[0], noise, latent)` at the first sigma in the remaining
schedule.

It then applies `inverse_noise_scaling(sigmas[0], mixed)` to every member when
the model provides that method. This includes a clean member that received no
fresh noise. That inverse step prepares the handoff for a second
`SamplerCustomAdvanced` using `DisableNoise`. The code also applies the
model's latent input and output transforms and replaces NaN or infinite
values with zero.

The video member always receives full fresh noise. `audio_denoise` controls
the audio member:

- `0` excludes audio from fresh noise. It still passes through the model
  latent transforms and inverse noise scaling.
- `1` gives audio full fresh noise at `sigmas[0]`.
- A value between them multiplies only the audio noise tensor by that value
  before `noise_scaling`.

After this preparation, the node moves the latent and noise mask to CPU. It
runs garbage collection and ComfyUI's `soft_empty_cache`. It does not unload
the models. The README warning against a force unload matches the code.

### README comparison

The README matches the implementation. It correctly describes spatial
`F.interpolate`, video re-noising at `sigmas[0]`, the `audio_denoise` range,
conditioning scale, a new guider, CPU handoff, and a soft cache clear.

One phrase needs a precise reading. At `audio_denoise=0`, the audio tensor is
not spatially interpolated and receives no fresh noise, but it is not copied
byte for byte through the whole function. Inverse noise scaling and the model
output transform still run. The input transform runs when any latent member
is nonzero. The README's warning about garbled audio is real but not enforced
by code. The node cannot detect whether too little of the schedule ran in
pass one.

There is no source contradiction to record.

## Hardware evaluation

[`scripts/h3-two-pass-evaluation.sh`](../../scripts/h3-two-pass-evaluation.sh)
runs four cases in fresh containers:

| Label | Pass one canvas | Encoded output | Frames |
| --- | --- | --- | --- |
| `native-1920x1088` | 1920 x 1088 | 1920 x 1088 | 124 |
| `two-pass-1920x1088` | 960 x 544 | 1920 x 1088 | 124 |
| `native-2560x1440` | 2560 x 1440 | 2560 x 1440 | 124 |
| `two-pass-2560x1440` | 1312 x 736 | 2560 x 1440 | 124 |

The HD scale is exactly two. For QHD, the first-pass latent is 82 x 46. A
scale of `160 / 82` makes the output latent width 160. The node's even-grid
rounding makes its height 90. The decoded output is therefore 2560 x 1440.

Every case uses the same operator-supplied prompt and seed, the committed
20-step scheduler, and bilinear interpolation for the two-pass cases. The
upstream README says that pass one should run the majority of the schedule,
but gives no split number. The script therefore requires
`H3_EVAL_SPLIT_STEP` instead of inventing one. It records that value and
`audio_denoise` in `manifest.json`.

The target host comes only from `H3_EVAL_HOST`. It has no default. The model
directory, results directory, prompt, seed, and split are also explicit. The
script uses the pinned ComfyUI image from the existing measurements unless an
equally explicit image is supplied. It downloads only the three Python files
from the pinned candidate commit. They are mounted as an evaluation custom
node. They are not built into the image.

The runner samples `/proc/meminfo` every 15 seconds from container launch
through generation completion. Each case emits the directly comparable line:

```text
RESULT label=... status=... wall_seconds=... peak_used_kb=... idle_available_kb=... baseline_available_kb=... total_kb=...
```

It retains every video and the exact submitted graphs. `ffprobe` must confirm
the expected width, height, and 124 encoded frames. It must also find a
non-empty AAC stream and record its duration:

```text
AUDIO label=... status=... codec=... duration_seconds=... garble_check=...
```

That probe proves that an audio track survived structurally. It cannot prove
that speech, music, or effects are intelligible. A two-pass run with
`audio_denoise` above zero is marked `listen_for_garbling`. This is a required
listening check, not a warning that can be cleared by `ffprobe`.

## Decision rule

The deciding measurement is the paired QHD wall-time ratio:

```text
two-pass-2560x1440 wall_seconds / native-2560x1440 wall_seconds
```

Writing a basement-owned node is justified only if all of these are true:

- the QHD two-pass wall time is no more than half of its native control;
- the HD pair shows the same direction rather than a QHD-only anomaly;
- all four runs complete at the requested dimensions and frame count;
- every output has a non-empty AAC track with a recorded duration;
- the product owner accepts both two-pass clips in a blind side-by-side
  review before seeing labels or timings.

The owner reviews each full clip at its encoded size and listens with sound.
The review covers fine detail, visible softness, subject identity, motion
coherence, temporal seams, duplicated features, audio clarity, and audio
sync. Either visible softening or garbled audio is a veto. A pipeline that
halves time but visibly softens the image does not pass.

The idea is killed if the paired QHD run does not halve wall time, if either
two-pass case fails structurally, or if the owner rejects picture or sound.
Peak memory is recorded for safety and comparison, but a lower peak does not
rescue a slow or visibly worse result.

Until those receipts and the owner's review exist, the status remains
unproven and basement implements nothing.

## NVIDIA Sol Engine is watch-only

NVIDIA's
[`Sol Engine H3 page`](https://nvlabs.github.io/Sana/Sol-Engine/H3/) claims a
3.95x end-to-end speedup for H3 on 8x GB200. It names kernel fusion, sparse
attention, and a cross-step cache, with a 4.5-hour optimization step.

A GitHub search on 2026-08-05 found no released Sol Engine code. It is
watch-only for that reason. The cross-step cache is the one named technique
that is not inherently multi-GPU. Nothing can be evaluated without code.
This ADR makes no claim about transfer to one GB10.

## Consequences

- No unlicensed third-party code enters the runtime image or repository.
- One reproducible hardware run can reject the technique before product work
  begins.
- A passing timing result still needs an independent implementation and a
  separate design decision before it can ship.
- Quality and audio remain acceptance gates. They are not inferred from file
  existence, codec metadata, or timing.
