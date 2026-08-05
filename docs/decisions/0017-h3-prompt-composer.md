# ADR 0017: H3 prompt composer

Date: 2026-08-05. Status: proposed. Design and mockup only.

## Decision

Basement will make text-to-video the default H3 generation mode.

The default Basic mode has one path:

1. Choose or skip a starting image, then describe the video.
2. Choose or accept an aspect ratio, size, and valid length.
3. Review the estimated time.
4. Generate.

A starting image is optional. Adding one changes the request to image-to-video
and changes the prompt format with it.

Advanced mode adds a shot timeline, the documented camera vocabulary,
dialogue, scene sound, and background music. The interface describes the
benefit in one sentence:

> Advanced gives better results with shot-by-shot control.

Both modes use a deterministic composer. No rewriting model sits between the
user and the final prompt. A local prompt check must pass before an expensive
run can start. The assembled prompt is visible in Advanced only.

Size uses common aspect ratios and named size settings as the primary path.
The resolved pixel dimensions are always visible. Exact width and height stay
behind `Exact size`. Length uses every duration the node accepts, with no
arbitrary product preset or fixed clip-length control.

## What the graphs support

The graph named
[`minimax-h3-t2v.json`](../../internal/recipe/graphs/minimax-h3-t2v.json) is a
text-to-video graph even though its generation node is named
`MiniMaxH3ImageToVideo`.

That node receives:

- CLIP and VAE connections;
- the prompt;
- width and height;
- frame count.

Seed-derived noise enters through the sampler. There is no image node, image
connection, or image substitution token in this graph. It decodes video and
audio and saves both in the result.

The separate
[`minimax-h3-i2v.json`](../../internal/recipe/graphs/minimax-h3-i2v.json) adds a
`LoadImage` node and passes its output as `first_frame`. That is the graph to
use when the user adds a starting image.

The product mapping is therefore direct:

| User choice | Graph | Prompt format |
| --- | --- | --- |
| No starting image | `minimax-h3-t2v.json` | T2VA |
| Starting image added | `minimax-h3-i2v.json` | I2VA |

An image is an input option, not a prerequisite.

The embedded model mark is the verified MiniMax organisation avatar recorded in [`LOGO-PROVENANCE.md`](../reference/h3/LOGO-PROVENANCE.md).

## Length contract

The MiniMax prompt guides define prompt syntax and timestamp rules. They do
not define the generation node's duration input.

The pinned graph supplies `length` to node 104 and creates the result at 24
fps in node 91. The official
[ComfyUI H3 tutorial](https://docs.comfy.org/tutorials/video/minimax/minimax-h3)
documents the 17-frame-per-block `17k+5` grid at 24 fps. The pinned runtime
contract recorded in [`H3-MEASUREMENTS.md`](../H3-MEASUREMENTS.md) identifies
7 blocks as the node's documented trained minimum and 21 blocks as its
documented trained maximum. The console's recipe contract therefore derives
encoded length as:

```text
frames = 17 * blocks + 5
blocks = 7, 8, 9, ..., 21
```

The real accepted duration control is therefore quantized. It has 15 stops:

```text
124, 141, 158, 175, 192, 209, 226, 243,
260, 277, 294, 311, 328, 345, 362 frames
```

At 24 fps these run from 5.1667 to 15.0833 seconds. Basic shows them as 5.2
through 15.1 seconds, rounded to one decimal for selection. The estimate and
serializer retain the exact encoded length. A slider moves one block per
detent. The adjacent numeric field snaps a typed value to the nearest valid
stop, so an invalid duration cannot reach generation.

Duration labels use explicit decimal-point formatting equivalent to
`toFixed(1)`. They never use locale-dependent number formatting. Prompt
timestamps continue to use exact frame-derived `MM:SS.mmm` values.

## Prompt formats

The two local MiniMax guides and their provenance are under
[`docs/reference/h3`](../reference/h3/FETCH-RECORD.md). The base guide covers
T2VA, I2VA, FL2VA, and L2VA. The full-reference guide is for richer workflows
with labelled reference assets and six output sections. This composer does not
use the full-reference format.

### Text-to-video

T2VA has no image-alignment instruction. It begins directly with the three
core fields in this order:

```text
integrated_multimodal_description: [Shot 1] ...

overall_soundscape: ...

non_diegetic_music: ...
```

Basic mode places the user's description in Shot 1, uses natural scene sound,
and uses no background music. Those are plain defaults, not claims inferred
from the description. Advanced mode exposes the fields needed to change them.

### Image-to-video

When a starting image is present, the composer adds the base guide's required
first line and one blank line before the same three core fields:

```text
For the target video, at 0.00 seconds into the target video, <Picture 1> (from [Shot 1]) is fully referenced.
```

Shot 1 then begins from Picture 1 and preserves its visible subjects,
composition, setting, and style before describing what changes.

The six-section full-reference format is not emitted merely because one image
is present.

## Basic mode

Basic is the landing state. It contains:

- one optional `Starting image` control;
- one `Describe your video` field;
- aspect ratio and size controls with resolved pixel dimensions;
- a detented length slider and numeric seconds field;
- the live time estimate;
- the prompt check and Generate action.

There are no starter templates, step numbers, camera terms, sound forms, or
dialogue fields in Basic.

### Size pattern

The selected pattern combines the two controls mature generation and export
interfaces use most often: aspect-ratio buttons first, then a named size
setting with resolved pixel dimensions. Exact width and height are a secondary
`Exact size` disclosure. This fits the console because the primary choices are
recognizable, every click has a visible time cost, and nobody has to calculate
a canvas.

The default is `16:9` plus `Recommended`, resolving to 1536 x 864. Both are
preselected and the combination carries the Recommended marker. Basic exposes
16:9, 9:16, 1:1, 4:3, and 3:4 without typing.

| Aspect ratio | Compact | Recommended | Large |
| --- | --- | --- | --- |
| 16:9 | 1024 x 576 | 1536 x 864 | QHD, 2560 x 1440 |
| 9:16 | 576 x 1024 | 864 x 1536 | 1440 x 2560 |
| 1:1 | 576 x 576 | 864 x 864 | 1440 x 1440 |
| 4:3 | 768 x 576 | 1152 x 864 | 1920 x 1440 |
| 3:4 | 576 x 768 | 864 x 1152 | 1440 x 1920 |

Every offered dimension is divisible by 32. `Exact size` accepts any positive
width and height on the same 32-pixel grid and keeps the estimate live.

Length starts at the first valid stop, displayed as 5.2 seconds. Basic never
shows the encoded frame count.

## Advanced mode

Advanced keeps the same image, size, length, estimate, assembled prompt,
prompt check, and Generate action. Starting image remains the first choice. It
replaces the single description field with compact controls for:

- addable and removable shots;
- duration for every shot;
- action in each shot;
- camera movement, amplitude, and speed;
- spoken language and exact dialogue;
- scene sound;
- background music;
- an optional seed.

Advanced allows 1 through 8 shots. Eight is a product usability bound, not a
MiniMax limit. It comes from the guide's instruction that a cut should
introduce new information, combined with the 15.1-second maximum: eight shots
already allows roughly one new shot every two seconds at the longest length.

Every shot has a duration in seconds. Durations are stored as frame counts so
their sum equals the encoded total exactly. The interface shows each shot's
start time and duration, the video total, and `Timeline matches N.N s`.
One-decimal shot labels use largest-remainder allocation, so the displayed
shot durations also add to the displayed total instead of exposing a rounding
mismatch.

The timeline self-corrects:

- adding a shot splits the longest shot;
- removing a shot gives its time to the preceding shot;
- changing a shot takes time from the following shots, then earlier shots if
  needed;
- changing total length scales the shot durations proportionally and assigns
  rounding remainders deterministically.

No edit can leave a silent mismatch. Shot 1 has no prompt timestamp. Every
later shot starts at the exact cumulative duration of the preceding shots and
uses the frame-derived `MM:SS.mmm` timestamp required by the guide. Interface
timing is expressed in seconds. Raw frame counts do not appear.

The camera selector reserves enough width for the longest vocabulary item and
does not clip or ellipsize selected text.

The camera selector is closed to MiniMax's documented vocabulary:

| Motion | Prompt expression |
| --- | --- |
| Zoom In | zooms in |
| Zoom Out | zooms out |
| Push In | pushes in |
| Pull Out | pulls out |
| Pan Left | pans left |
| Pan Right | pans right |
| Truck Left | trucks left |
| Truck Right | trucks right |
| Tilt Up | tilts up |
| Tilt Down | tilts down |
| Pedestal Up | moves upward |
| Pedestal Down | moves downward |
| Arc Shot | moves in an arc around the subject |
| Tracking Shot | follows the moving subject |
| Static Shot | holds a static shot |
| Shake Slightly | shakes slightly |
| Shake Strongly | shakes strongly |
| POV | shows the subject's point of view |
| Roll Clockwise | rolls clockwise |
| Roll Counterclockwise | rolls counterclockwise |

Small and Large add the documented amplitude phrase. Slow and Fast add the
documented speed phrase. Normal emits no extra phrase.

The composer assigns speaker identifiers by first vocal event and preserves
the user's exact words inside `<d>[Language] ...</d>`. It adds the documented
voiceover, cross-cut, and cutoff syntax only when the matching control is used.

## Generation-time estimate

The UI shows cost instead of restricting choices. The four measurements in
[`H3-MEASUREMENTS.md`](../H3-MEASUREMENTS.md) on one GB10 are:

| Canvas | Frames | Clip length | Measured time |
| --- | ---: | ---: | ---: |
| 1344 x 768 | 124 | 5.17 seconds | 1,061 seconds |
| 1344 x 768 | 362 | 15.08 seconds | 5,524 seconds |
| 1920 x 1088 | 124 | 5.17 seconds | 3,021 seconds |
| 2560 x 1440 | 124 | 5.17 seconds | 7,750 seconds |

The observed time grows faster than pixel count or frame count. At the same
length, 3.6 times the pixels took 7.3 times the time.

The first estimate uses a single empirical work curve:

```text
work_ratio = (width * height * frames) / (1344 * 768 * 124)
estimated_seconds = 1061 * work_ratio^1.5425
```

The curve is anchored to the 1344 x 768, 124-frame measurement. The exponent
is a least-squares fit in log space to the other three measured points. At the
four calibration points, its error is under 5 percent. For 2560 x 1440 at 124
frames it shows about 2 hours 5 minutes.

The visible label is short:

> Estimate from 4 measured runs

This is a rough cost signal from four successful runs on one device. It is not
a performance model, completion deadline, countdown, guarantee, or claim about
other hardware. Values between the measured points are interpolation. Values
outside the measured canvas and frame ranges are extrapolation and carry no
independent validation.

The UI rounds the estimate to minutes and uses `About`. It updates immediately
when width, height, or length changes. Once a generation starts, the estimate
is replaced by real elapsed time and reported progress.

## Assembled prompt and check

The assembled prompt and its Mode, Structure, Timing, Request, format, and copy
metadata stay visible beside the controls in Advanced only. Basic does not show
raw prompt syntax. The prompt is read-only in this design. The controls remain
the source of truth, which avoids a second manual state and the risk of silent
overwrites.

The check is local and deterministic. Generation is disabled when it finds:

- missing user description or shot action;
- missing, repeated, or out-of-order core fields;
- an I2VA alignment line without an image, or an image without that line;
- a timestamp on Shot 1;
- skipped or repeated shot numbers;
- a cut outside the requested frame count or cuts that do not increase;
- a camera expression outside the closed vocabulary;
- empty or unbalanced dialogue tags;
- an empty sound or music field;
- size or frame values rejected by the graph or recipe;
- a prompt over the API limit.

The check proves prompt structure and request validity. It does not judge
whether the scene is physically plausible or whether the generated video will
look good.

In Basic, the check is silent while the form is untouched and whenever the
request is valid. It appears only when it has a specific error to report.

## Why no rewriting model

A rewriting model would add runtime, placement, download, and failure cost
without becoming the syntax authority. It also cannot prove that a rewrite is
better before generation. The deterministic composer is immediate, offline,
repeatable, and sufficient for the documented formats. A rewriting model may
be reconsidered later as an optional suggestion, never as a requirement.

## Removed from the rejected design

This decision deletes:

- the required first-frame image;
- the three starting structures;
- the six-step interaction;
- the prompt-insurance layer and terminology;
- preset-only size entry and arbitrary duration entry;
- the long worked prompt;
- manual prompt mode and its duplicate state management;
- local draft behavior specific to that manual mode;
- the companion-model candidate and deployment analysis;
- explanatory paragraphs in the interface.

The shot timeline, camera controls, dialogue, and sound controls survive only
in Advanced.

## Consequences

- First use is optional image, one text field, size, length, and Generate.
- Text-to-video uses the graph and prompt format already measured.
- Image-to-video remains available without controlling the default path.
- Users see the time consequence of larger or longer requests before starting.
- The estimate can improve as comparable completed runs are recorded, without
  changing the prompt composer.
- The serializer and checker become testable product contracts.

## Verification before implementation

- The owner approves the revised static mockup.
- Serializer fixtures cover T2VA with no alignment line and I2VA with the exact
  required line.
- Fixtures cover field order, camera expressions, frame-derived timestamps,
  speaker identifiers, dialogue tags, scene sound, and background music.
- Estimate tests reproduce the formula at all four measured points and verify
  coarse display rounding.
- Browser checks cover Basic first use, Advanced, optional image selection,
  every aspect and size combination, exact size, every duration detent,
  add/remove shot behavior, timeline reconciliation, responsive layout,
  keyboard order, and screen-reader labels.
