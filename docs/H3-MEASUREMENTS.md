# MiniMax H3 on one GB10: measured, not estimated

Machine: one GB10 Spark, 127,599,384 KB total unified memory (about 121.7 GiB).
Runtime: basement's own pinned ComfyUI image
`ghcr.io/punkjazz-labs/basement-comfyui@sha256:8e6715f3e133c03b12f7730c4d66124554952bf9dae81263a153be05f96d23a9`
(ComfyUI v0.30.0, commit b1693ecba9f5b65f8c80ab36b195ab963ec92413).
Weights: the optimized set, 42,470,585,471 bytes, from Comfy-Org/MiniMax-H3
revision 0543966fbdce5ba05709a8f2031c94bdba629b4a.
Graph: basement's own converted API-format text-to-video graph
(internal/recipe/graphs/minimax-h3-t2v.json).
Wiring: identical to the comfyui runtime kind (artifact read only at /model,
generated model paths file in the cache mount, comfyUIArgs command line).

Method: sample MemAvailable every 15 seconds from container start through the
generation, take the peak. Memory on a GB10 is unified, so this is the whole
footprint, weights included, not a separate device reading. Every run below
produced a real playable file, and a frame from each was inspected rather than
only checked for existence.

## Results

| Run | Canvas | Frames | Wall time | Peak memory | Output |
|---|---|---|---|---|---|
| default | 1344 x 768 | 124 (5.17 s) | 1061 s (17 min 41 s) | 93.27 GB | 2,093,446 bytes |
| max duration | 1344 x 768 | 362 (15.08 s) | 5524 s (92 min 4 s) | 96.74 GB | 6,595,361 bytes |
| HD | 1920 x 1088 | 124 (5.17 s) | 3021 s (50 min 21 s) | 95.19 GB | 3,131,382 bytes |

All three succeeded. All three produced h264 video with an AAC audio track:
the model generates picture and sound together and the graph saves both.

Host baseline before any container: about 4.0 GB. Idle with the server up and
the model not yet loaded: about 4.9 GB.

## What the numbers say

Memory barely responds to what is asked for. Nearly tripling the frame count
cost 3.5 GB. Doubling the pixel count cost 1.9 GB. The footprint is dominated
by the weights and a fixed working set, so every configuration tested sits
between 93 and 97 GB against 121.7 GiB of unified memory, leaving 25 GB or
more free at the busiest moment.

Time is the real cost, and it grows faster than the request does. 2.9 times
the frames took 5.2 times as long. 2.0 times the pixels took 2.9 times as
long. A generation is minutes to hours, not seconds, and that is what bounds
what is practical to offer, not the machine's memory.

Quality holds outside the default canvas. The node accepts width and height
from 32 to 16384 in steps of 32 and carries no trained-range warning for
resolution, unlike its frame count input which does. At 1920 x 1088 the
output shows none of the duplication or anatomical drift that usually marks a
diffusion model pushed past its training canvas. At 362 frames, frame 330 is
as coherent as frame 60.

## What these numbers are for

- `memory_model` in the recipe: the measured total, since ComfyUI has no
  device memory fraction to declare (see memoryPlan in
  internal/operations/host.go). The worst case observed is the 92-minute run
  at 96.74 GB.
- `max_blocks`: 21 blocks, 362 frames, is proven to fit and to stay coherent.
  That is the top of the range the node itself documents as trained.
- `min_blocks`: 7 blocks, 124 frames, is the bottom of that documented range.
- The canvas: 1920 x 1088 is proven. A 2560 x 1440 run is outstanding.
- Duration estimates in the console: still nothing. Three samples is not a
  model of generation time, and the console shows elapsed time only.
