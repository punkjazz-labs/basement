# Spec 11: media generation (comfyui runtime kind)

Branch `spec/11-media-generation`. Six phases, one commit each. Phases D and E
touch files the Roles work (ADR 0015, in flight) is editing (`internal/httpapi`,
`internal/store`, `webui/ui`) and build on spec 13 phases A through C; they do
not start until both have merged.

**Intent.** basement installs and runs a media generation stack the way it runs
a text model: one click, pinned container, pinned weights, verified install. The
console gains a Generate view where a prompt goes in and a clip or a still comes
out, with a gallery of past results. ComfyUI is the runtime underneath and the
workflow graph is an implementation detail the user never sees, never edits, and
is never asked about.

---

## 0. Decision: the MiniMax H3 licence excludes the EU/UK/Korea/US, ships gated by self-attestation

Read this before any implementation work. The premise this spec was requested
under (commercial use restricted, individuals may use it freely) did not match
the licence text; the research below is why. The owner has since decided how
basement handles that mismatch (see "Decision and consequences for this spec"
below): the recipe ships everywhere, gated at install time by a checkbox in
which the user confirms they are not located in a territory the licence
excludes. basement does not verify that claim by any technical means; it is
the user's own statement, and if it is false, that is on the user.

MiniMax H3 is released under the **MiniMax H3 Community License Agreement**
(effective August 2, 2026). Its operative restriction is territorial, not
commercial. Verbatim from the licence file:

- Definition: `"Excluded Territories" means the European Union, the United
  Kingdom, the Republic of Korea and the United States of America.`
- Definition: `"Applicable Territory" means worldwide, excluding the Excluded
  Territories.`
- Definition: `"Licensee," "you," or "your" means the natural or legal person
  exercising rights and/or using the MiniMax H3 Works for any purpose in any
  field of use under this Agreement.`
- Section II (Grant of Rights): `Solely within the Applicable Territory, we
  grant you a non-exclusive, non-transferable, royalty-free, limited license to
  use, reproduce, distribute, create derivative works (including Model
  Derivatives), and modify the Materials [...]`
- Section V.4: `You may not use, reproduce, modify, distribute, or display the
  MiniMax H3 Works or any of their Outputs or results outside the Applicable
  Territory. Any such use outside the Applicable Territory is not authorized by
  this Agreement.`

Commercial use is separately addressed and is *permissive* by comparison.
Section IV.1: `You shall obtain a separate, prior written authorization from
MiniMax by contacting api@minimax.io with the subject line "MiniMax H3 licensing
- authorization request", if your commercial products and services generate more
than 20 million US dollars (or equivalent in other currencies) in yearly
revenue.` Section IV.2 additionally requires that commercial products
`prominently display "MiniMax H3"` in the user interface.

**Plain verdict on personal use in the EU.** The grant in Section II is
conditioned on the Applicable Territory, "Licensee" is defined to include a
natural person, and Section V.4 covers "use" and "Outputs" without any
personal, private, hobbyist or non-commercial carve-out. On the licence's own
words, personal use of the released weights in the EU is not authorized by this
agreement. The restriction that blocks a headline recipe is territory, not
commerce, and it does not become permissive for an individual.

**What MiniMax says officially.** MiniMax's own licence Q&A document states that
parties in the restricted regions may `apply for a formal license` after MiniMax
reviews the deployment scenario, and that the globally available route is the
hosted API rather than the open weights, because with open weights `users can
independently deploy, modify, and distribute the model`. On 2026-08-04 the
official @MiniMax_AI account posted (per docs/research/x-sweep-2026-08-04-ds4f-
minimax-h3.md): `Claims that MiniMax H3 'cannot legally be used' in certain
regions are incorrect... can be licensed for deployment in the US, EU, UK, and
South Korea through MiniMax's formal authorization process.` Both statements
describe a per-applicant authorization, not a blanket grant, and neither
contradicts the licence text above.

**Reported reason, single source, not in the licence.** A MiniMax employee
(@RyanLeeMiniMax, 2026-08-03) replied in the same thread: `This regional
carve-out stems from our ongoing generative video copyright litigation with
major Hollywood studios.` One source, no corroboration found, no mention of
litigation anywhere in the licence text. Recorded as reported, not as fact.

**Decision and consequences for this spec.**

1. Everything in sections 2 through 7 (the `comfyui` runtime kind, the
   container recipe, pinned file-level artifacts, the generation API, the
   Generate view) is model-independent. Build it.
2. The MiniMax H3 recipe ships, including to users in the EU, UK, Republic of
   Korea and United States, without written authorization from MiniMax.
   basement does not withhold the recipe on the user's behalf and does not
   seek that authorization itself. Install time carries an explicit
   self-attestation instead: alongside the existing licence-acceptance
   checkbox, the user ticks a second checkbox confirming they are not located
   in a territory the licence excludes. Misrepresenting that is the user's
   liability, not basement's; basement performs no geolocation, no IP check,
   and no blocking of any kind.
3. basement must not render the excluded-territory list as hardcoded product
   copy. It is recipe data (section 3.4), read by the console the same way
   the licence name and licence URL already are, so what ships is the
   licence's own words, not a maintainer's paraphrase.
4. No legal advice anywhere in product copy. The console states the licence's
   own words, names the excluded territories, links to the licence, records
   the user's confirmation, and stops. No "most people are fine", no "at your
   own risk" nudge, no pre-ticked box, no claim that ticking the box makes use
   of the model lawful.

---

## 1. Research findings that shape the design

Sources are listed in section 10. Anything not verified against a primary
source is marked.

**What MiniMax H3 is.** A `33B-parameter dense, single-stream Transformer, with
approximately 13B parameters residing in AdaLN-related branches`, in three
parts: H3-Context-IR (prompt preprocessing), H3-Base (generation), and
H3-Regenerate-2K (upscaling). It generates **video with native stereo audio**,
24 fps, 32 kHz stereo, `4-15 seconds`. It is not an image model. Still images
come out of it only by generating frames and selecting one; see open question
O2. Per the model card, H3-Context-IR is hosted rather than released, and
H3-Regenerate-2K (the 2K path) is not open-sourced, so a local install is the
base model at its native canvas only.

**Native canvas.** ComfyUI's documentation: `H3's native canvas is a 768px short
edge, capped at 768x1344 pixels and rounded to a multiple of 32`, and `The
duration input snaps to the model's 17-frame-per-block (17k+5) grid at 24fps`.
Derived from that grid, not measured: the shortest generatable clip is 22 frames
(k=1), about 0.9 seconds. Local generation is 768px short edge, not 2K.

**ComfyUI support.** Native, in ComfyUI core, from day zero, requiring
`0.30.0 or later`. Three official workflow templates ship with it: T2V, I2V and
R2V. Weights are repackaged for ComfyUI at `Comfy-Org/MiniMax-H3` into
`diffusion_models/`, `text_encoders/` and `vae/`. No community custom node is
needed for the shipped paths. (A community node pack for still-image use exists,
`astropuzzo/ComfyUI-MiniMax-H3-Image-Studio`, self-described as experimental and
entirely AI-coded. Out of scope, named only so the option is on record.)

**File sizes.** The Comfy-Org repository holds every variant and totals 385 GB;
only a subset is installed. Component sizes from a secondary source (AtlasCloud),
to be replaced with exact byte counts read from the Hugging Face manifest at
qualification time:

| File | Reported size |
|---|---|
| `diffusion_models/minimax_h3_fl2va_pruned_int8_convrot.safetensors` (T2V/I2V) | 20.97 GB |
| `diffusion_models/minimax_h3_ref2va_pruned_int8_convrot.safetensors` (R2V) | 20.97 GB |
| `text_encoders/qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors` | 15.69 GB |
| `vae/minimax_h3_video_vae_fp16.safetensors` | 5.21 GB |
| `vae/minimax_h3_audio_vae_fp32.safetensors` | 0.61 GB |

ComfyUI reports the optimized set reduces the footprint `from 123.6 GB in full
precision to 42.5 GB`. The 42.5 GB figure is the T2V/I2V set (fl2va + encoder +
both VAEs). Adding the R2V checkpoint brings it to 63.4 GB (AtlasCloud).

**GB10 fit: verdict is yes, with a qualification caveat.** A Spark has 128 GB
coherent unified memory; existing recipes declare
`per_node_minimum_memory_bytes: 120000000000` with a 16 GB reserve, so roughly
104 GB is the working budget after reserve. The 42.5 GB optimized set fits with
very large headroom; the 63.4 GB both-checkpoint set fits; the 123.6 GB bf16 set
does not fit inside the budget and is not a candidate. Independent corroboration
that the model runs on one Spark: at least four DGX Spark users report doing it
(@ivanfioravanti with ComfyUI templates, @aijoey, @tonbistudio, @Tech2Wild), per
the X sweep. The caveat is not memory, it is the aarch64 + sm_121 software
stack; see below.

**ARM64 / sm_121 caveat.** Stock PyTorch wheels ship kernels through sm_120, so
GB10 (compute capability 12.1, aarch64) needs a CUDA 13 build. No official
ComfyUI container image for linux/arm64 with CUDA was found. What exists:
`mmartial/ComfyUI-Nvidia-Docker` has a `build-dgx` target for linux/arm64;
`howeszy/comfyui-arm64` is a community image; and several non-container Spark
distributions exist (`ecarmen16/SparkyUI`, `Triplany/comfyui-dgx-spark`,
`AEON-7/comfyui-aeon-spark`). Reported hazard, single source
(`Sggin1/DGX-SPARK`): PyPI's `onnxruntime` silently overwrites custom sm_121 GPU
wheels. This is exactly the situation ADR 0011 anticipated for the llama.cpp
kind, and it gets the same answer: basement builds and pins its own aarch64
CUDA 13 image rather than depending on an unaudited community tag.

**Generation time on a Spark: slow, and the console must say so.** All Spark
timings below come from one user (@ivanfioravanti) via the X sweep. Community
reports, single source, not corroborated across users, not measured by us:

| Output | Reported time |
|---|---|
| 960x544, 10 s, ComfyUI T2V template | 16 min 57 s |
| 896x1184, 8 s, ComfyUI I2V template | 36 min |
| 1280x736, 15 s, ComfyUI T2V template | 1 h 20 min |

A same-hardware comparison from another Spark user (@YRSM_Simon, single source):
864x480/5 s took 270 s for H3 versus 154 s for LTX 2.3 Eros. For scale on other
hardware, again community reports: RTX PRO 6000 96GB, BF16, 960x544, 124 frames,
20 steps, `~5 minutes... Peak usage was approximately 80.5GB`; RTX 4090, full
15 s clip, 9.6 min after tuning.

**The 5-6 GB low-VRAM path is not the path we ship.** @cocktailpeanut reports
`5-6GB of VRAM only for 5s (124 frames)... 8-9GB of VRAM for 15s at 832x480`,
credited to @deepbeepmeep. That is the WanGP runner with aggressive offloading,
not ComfyUI. Community report, single source. It is recorded here because it
tells us the memory floor keeps dropping, and because a future `wangp` kind is
conceivable, but it does not change this spec: a Spark has memory to spare, so
trading memory for wall-clock on a machine that is already slow per clip would
be the wrong trade. Do not implement offloading in v1.

---

## 2. What ships

1. A new runtime **kind** `comfyui`, added to the ADR 0011 adapter model.
2. A ComfyUI container recipe pinned by image digest, built for aarch64/sm_121.
3. **File-level artifact pinning**, because whole-repository download is not
   viable for a 385 GB repository (section 3.3). This is a downloader change,
   not a weakening of pinning: it strengthens it from snapshot-total to
   per-file.
4. Pinned workflow graphs shipped inside basement, parameterized by the four
   things the user controls. Never exposed, never editable.
5. A generation API basement owns, proxying to ComfyUI's own API.
6. A console **Generate** view: prompt, a small number of controls, honest
   progress with an honest time estimate, and a gallery read from local disk.

Naming note, deliberate deviation from the request. The request called this a
"media" runtime kind. ADR 0011 names kinds after the runtime (`vllm`, `sglang`,
planned `llamacpp`), because the adapter's four responsibilities are properties
of the runtime, not of the media type. The kind is therefore `comfyui`. "Media"
stays as the recipe *class* used for console grouping and for the ADR 0003 rule
in section 6.

---

## 3. Schema (`internal/recipe`)

### 3.1 Kind allowlist

`allowedRuntimeKinds` gains `"comfyui": true`. Per the comment already on that
map, it may only be added in the same commit that supplies the kind's command,
memory model, health wait and metric mapping (phase B).

### 3.2 `service.comfyui`

`validateRuntimeBlocks` learns the third block and keeps the exactly-one rule.

```yaml
service:
  internal_port: 8188
  default_host_port: 8188
  served_model_id: MiniMaxAI/MiniMax-H3
  comfyui:
    graphs:
      text_to_video: minimax-h3-t2v.json
      image_to_video: minimax-h3-i2v.json
    output_directory: /output
    input_directory: /input
    default_short_edge: 768
    max_short_edge: 768
    max_long_edge: 1344
    frame_block: 17          # frames = 17k + 5
    frame_offset: 5
    frames_per_second: 24
    min_blocks: 1
    max_blocks: 21
    default_blocks: 7
    concurrent_generations: 1
```

Every numeric limit above is a fact about the model that the console reads to
build its controls, so the console can never offer a size or duration the model
does not accept. Values shown are the ComfyUI-documented grid and canvas;
`max_blocks` and `default_blocks` are placeholders to be set at qualification
and must not ship as guesses.

`graphs` names files under `internal/recipe/graphs/`, embedded with the recipes.
They are ComfyUI API-format JSON with named placeholder tokens for prompt, seed,
frame count, width and height. The validator checks at load time that every
named graph file exists and parses, and that every placeholder the adapter
substitutes is present. A recipe whose graph is missing a placeholder is
rejected, so a graph can never silently ignore the user's prompt.

### 3.3 `artifact.files`

The current downloader (`internal/operations/huggingface.go`) downloads the whole
repository snapshot and fails unless the snapshot total equals
`expected_bytes`. Against a 385 GB repository that is both wrong and impossible
on a Spark's disk. Add an optional file list:

```yaml
artifacts:
  - role: primary
    repository: Comfy-Org/MiniMax-H3
    revision: <pinned commit sha>
    expected_bytes: <sum of the files below, exact>
    files:
      - name: diffusion_models/minimax_h3_fl2va_pruned_int8_convrot.safetensors
        expected_bytes: <exact>
      - name: text_encoders/qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors
        expected_bytes: <exact>
      - name: vae/minimax_h3_video_vae_fp16.safetensors
        expected_bytes: <exact>
      - name: vae/minimax_h3_audio_vae_fp32.safetensors
        expected_bytes: <exact>
    licence: MiniMax H3 Community License Agreement
    licence_url: https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/main/LICENSE
```

Rules, all enforced in the validator and again in the downloader:

- `files` absent keeps today's behaviour exactly. No existing recipe changes.
- `files` present: `Download`, `Complete` and `CheckAccess` restrict the
  manifest to the named files, in the declared order.
- Every declared file must exist in the pinned revision's manifest, and each
  declared `expected_bytes` must equal the manifest size for that file.
  A mismatch fails the install; it is never rounded, warned about, or ignored.
- The sum of declared file sizes must equal the artifact's `expected_bytes`.
- The existing per-file hash verification and the completion marker are
  unchanged; the marker records only the declared files.
- A repository resolving to a different revision still fails, as today.

Two artifacts are declared for the H3 recipe: the weights above (role
`primary`) and the licence acceptance is driven from `primary`'s licence
fields, as today.

### 3.4 Territory self-attestation

New optional artifact-level field, alongside the existing `licence` and
`licence_url` (section 3.3), set on any artifact whose licence restricts
territory:

```yaml
artifacts:
  - role: primary
    repository: Comfy-Org/MiniMax-H3
    revision: <pinned commit sha>
    ...
    licence: MiniMax H3 Community License Agreement
    licence_url: https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/main/LICENSE
    licence_territory_exclusions:
      - European Union
      - United Kingdom
      - Republic of Korea
      - United States of America
```

The strings are copied from the licence, not composed, the same rule the
validator already enforces for `licence` and `licence_url`. When an artifact
carries this field, the install licence screen must render, above the accept
control, the excluded-territory list read from this field and the licence's
own sentence about it, plus the licence link (section 5.3). Its presence also
requires a second explicit confirmation before install can proceed, separate
from and in addition to licence acceptance: the install API's
`confirm_territory_eligibility` flag (section 5.3). basement does not
geolocate the user, does not ask where they are by any technical means, and
does not decide for them; the checkbox records a self-attestation, nothing
more. No legal advice, no reassurance, no risk framing. See section 5.3 for
the copy rules and the persistence treatment.

Validator rule: `licence_territory_exclusions`, when present, must be a
non-empty list of non-blank strings. It carries no meaning without `licence`
and `licence_url`, which every artifact already requires unconditionally.

### 3.5 Memory model

`Service.MemoryFraction` has no answer for `comfyui`: ComfyUI has no static
device-memory fraction. `memoryPlan` therefore branches before calling it. For
`comfyui` the plan is `memory_model.weights_bytes` plus
`memory_model.runtime_overhead_bytes`, both from the recipe's existing
`memory_model` block, with `kv_bytes_per_token: 0`. `runtime_overhead_bytes`
is measured on hardware at qualification (peak resident during a generation at
default settings) and must not ship as a guess. The console's memory estimate
already renders `n/a` when `memory_model` is absent, so an unqualified media
recipe degrades honestly.

---

## 4. Runtime adapter (`internal/operations`)

Four responsibilities, per ADR 0011.

**Command.** `runtimeCommand` gains a `comfyui` case returning
`["python3", "main.py"]` with `--listen 0.0.0.0`, `--port <internal_port>`,
`--output-directory`, `--input-directory`, and the three model directory flags
pointing at the mounted artifact subdirectories. Exact flag names are verified
against the pinned image during phase B and recorded in the phase report; do not
copy them from this spec without checking.

**Mounts.** The artifact directory mounts read-only, as today. Two new
read-write host mounts: `<dataDir>/generations/<model_id>` at the container's
output directory, and `<dataDir>/generations/<model_id>/.input` at the input
directory (for I2V source images). Both are created by
`create_container` and owned by basement.

**Health.** `wait_http` against `GET /queue`, which is a documented ComfyUI
route and returns the queue state as JSON. Do not use `/` (it serves the ComfyUI
web app, which we never expose to the user).

**Install verification.** `verify_openai_inference` does not apply. Add
`verify_media_generation`: submit the recipe's smallest legal generation
(`min_blocks`, default short edge, fixed seed) through `POST /prompt`, poll
`GET /history/{prompt_id}`, and require a non-empty output file on disk. It
replaces `verify_openai_inference` in the H3 recipe's operations list. This is
a real generation and therefore expensive; see open question O3.

**Metrics.** ComfyUI exposes no Prometheus endpoint we can rely on. The
telemetry tiles that read `vllm:*` / `sglang:*` prefixes render `n/a` for a
media model. Do not invent throughput numbers for a model that does not produce
tokens. Queue depth and the current generation's progress come from ComfyUI's
`/queue` and websocket, and belong to the Generate view, not to the token
telemetry tiles.

---

## 5. Generation API and licence copy

### 5.1 API (`internal/httpapi`, phase D)

Generations are not engine jobs. Jobs are installs: planned, rollback-capable,
receipted. A generation is a request against an already-running service, so it
gets its own small surface backed by ComfyUI's own queue.

```
POST   /api/v1/generate                    {model_id, mode, prompt, blocks, short_edge, seed?}
                                           -> 202 {generation_id}
GET    /api/v1/generations                 -> list, newest first, from store + disk
GET    /api/v1/generations/{id}            -> {status, queue_position, percent, started_at, finished_at, error?}
GET    /api/v1/generations/{id}/file       -> the output bytes
DELETE /api/v1/generations/{id}            -> removes the row and the files
POST   /api/v1/generations/{id}/cancel     -> POST /interrupt upstream
GET    /api/v1/generations/events          -> SSE, bridged from ComfyUI's /ws
```

- `POST /api/v1/generate` returns 409 with a sentence the UI shows verbatim when
  the addressed model is not the active model, when it is not a `comfyui` model,
  or when a generation is already running and
  `concurrent_generations` is 1.
- Request validation is against `service.comfyui`: `blocks` within
  `[min_blocks, max_blocks]`, `short_edge` within the canvas, resulting
  dimensions rounded to a multiple of 32. Out-of-range is 400 with the limit
  named. The server never silently clamps.
- Seed absent means basement generates one and records it. The recorded seed is
  shown in the gallery so a result can be reproduced.
- The graph is loaded from the recipe, placeholders substituted, and posted to
  `POST /prompt`. The graph JSON never appears in any API response.
- Auth uses the existing read/model auth middleware; ComfyUI's own port is bound
  to loopback and is never proxied wholesale. `/v1/` continues to mean
  OpenAI-compatible text and is untouched by this spec.

### 5.2 Persistence (`internal/store`, phase D)

One table: id, model_id, mode, prompt, blocks, short_edge, seed, status,
error, output_path, bytes, created_at, finished_at. Files live under
`<dataDir>/generations/<model_id>/<generation_id>/`. Nothing is auto-deleted.
The Storage view gains a "Generations" group that reports the total and lets a
user delete a generation or the whole group, using the existing in-place
storage actions.

### 5.3 Territory self-attestation: install flag, persistence and copy rules

The existing install endpoint (`POST /api/v1/models/{id}/install`) carries
`confirmed` and `accept_licence` today. It gains a third boolean,
`confirm_territory_eligibility`, required precisely when the recipe's primary
artifact carries `licence_territory_exclusions` (section 3.4): omitted or
false on such a recipe is a 400 naming the missing confirmation, the same
shape as today's licence-acceptance 400. Recipes without the field are
unaffected; the flag is simply absent from their request handling.
Peer-delegated install (`peerInstall`) relays the field to the peer untouched,
exactly as it relays `accept_licence` today, because the peer owns its own
licence and territory record.

Persistence mirrors `accept_licence` exactly. Today `AcceptLicence` and
`LicenceAccepted` write and read one row in
`accepted_licences(recipe_id, recipe_version, accepted_at)`, keyed by recipe
id and version, and the install handler writes the row only when the flag was
true. Territory confirmation gets the same treatment: a new
`territory_confirmations(recipe_id, recipe_version, confirmed_at)` table and
`ConfirmTerritoryEligibility` / `TerritoryEligibilityConfirmed` store methods,
written only when `confirm_territory_eligibility` is true. The console reads
it back the same way it reads licence acceptance today, so a reinstall or a
later visit to the install dialog does not ask the user to reconfirm what is
already on record. Recording the confirmation is not basement verifying the
claim; it is the same honest paper trail basement already keeps for licence
acceptance, nothing more.

The install licence screen for a territory-gated recipe shows, in this order:

1. The licence name, verbatim: "MiniMax H3 Community License Agreement".
2. The excluded territories, read from the artifact's
   `licence_territory_exclusions`, introduced by a plain sentence stating that
   the licence does not grant rights in them. The licence's own words carry
   the meaning; basement adds none.
3. The commercial term as the licence states it: authorization is required
   above 20 million US dollars in yearly revenue, and products using the model
   must display "MiniMax H3" in their interface.
4. A link to the licence and a link to MiniMax's licence Q&A.
5. Two unticked controls: the existing "I accept the model licence", and a
   second, plainly worded as what the user is asserting, e.g. "I confirm I am
   not located in [the territories named above]." It states what the licence
   says and what the user confirms, nothing more: not a request for
   permission, not a legal opinion, not a promise that basement checked.

Forbidden, explicitly: pre-ticked boxes, "recommended" styling on either
control, any sentence advising what the user may or may not do, any
comparison to other licences, any softening ("mostly", "generally", "in
practice"), any hardening beyond the licence's words, any claim that ticking
the box makes use of the model lawful. Both controls are `quiet` or `ghost`
pills; the primary in that dialog is the install action itself, and it stays
disabled until both are ticked.

---

## 6. Engine interaction: what happens to the text model

**Decision for v1: a media model is the active model.** ADR 0003 holds
unchanged. Installing and starting the ComfyUI recipe is a switch, with the
same transactional path, the same rollback target, and the same console
language ("Switch", plus the existing explanation that the current model will
stop). If a text model is serving and the user opens Generate on an installed
but stopped media model, the console offers the switch and states plainly that
the text model stops. Generating an image or a clip while a text model serves is
not possible in v1, and the console must say that rather than implying
otherwise.

Why not concurrent, given the memory clearly allows it: the concurrency work is
already specced (spec 13, phases B and C: an active set, then a memory budget
across loaded models) and is being built right now. Duplicating it here would
mean two competing notions of "active". When spec 13 phase C lands, a media
model becomes an ordinary member of the active set and this decision is revisited
in one place: its memory claim is `weights_bytes + runtime_overhead_bytes`
(section 3.5), which is exactly the shape phase C's budget expects.

Two constraints that survive into a concurrent world and should be written into
the phase C follow-up rather than assumed:

- A running generation is not interruptible without losing work. Any future
  eviction policy must refuse to evict a media model mid-generation, or must
  cancel it explicitly and say so.
- The port. ComfyUI binds 8188, not 8000, so it does not contend with the text
  endpoint. Spec 13 phase A ("the bound port belongs to the install") is
  compatible; nothing here needs port 8000.

---

## 7. Console: the Generate view (mockup-gated)

New visual concept, therefore a static mockup approved by the owner before
implementation, per the conventions.

- New tab `Generate`, placed after `Playground`. Tab description follows the
  existing one-line pattern.
- Empty state when no media model is installed: a single line naming what is
  missing and a link to Models. No hero, no marketing.
- Composer: a prompt field (the familiar composer pattern from Playground), a
  duration control stepping in blocks and labelled in seconds derived from
  `frames = 17k + 5` at `frames_per_second`, a size control offering only
  canvas-legal options, and an optional source image for I2V. One `primary`
  pill: `Generate`.
- Progress: queue position, percent from the SSE bridge, elapsed time, and a
  `Cancel` (`quiet`). The estimate line is the honest one and it is derived,
  not invented: until basement has measured this machine, it shows nothing, or
  the phrase agreed in the mockup. After the first completed generation on this
  machine, it shows a range derived from that machine's own measured
  seconds-per-frame, labelled as an estimate. Never show a number sourced from
  someone else's hardware.
- Copy must not hide the wall clock. Community reports on Spark hardware run
  from about 17 minutes to about 80 minutes per clip (section 1). The view
  should be usable as a thing you start and come back to, not as a thing that
  pretends to be fast.
- Gallery: reverse-chronological cards read from `GET /api/v1/generations`,
  each with an inline player or still, the prompt, the seed, the settings, the
  measured duration, and `Delete`. Playback of a local file only; no external
  requests, consistent with the console's offline posture.
- Attribution follows the existing rules: the model line states what the recipe
  data says and nothing more. Quantization lines state the format only.
- No emoji. No em dashes. `n/a` for absent values.

---

## 8. Phases and test strategy

Each phase is reviewable on its own and leaves the tree green
(`npx tsc --noEmit && npm run build`, `go build ./... && go vet ./... && go
test ./...`).

**Phase A. Schema.** `artifact.files` (types, validator, downloader restriction),
`licence_territory_exclusions`, `service.comfyui` types and validation, graph
embedding and placeholder validation. Kind stays disallowed. This phase also
carries the `confirm_territory_eligibility` install flag and its persistence
(section 5.3), since both are generic recipe-schema features independent of
the `comfyui` kind: the install handler, `peerInstall`, and the new
`territory_confirmations` store table and methods. Tests: table tests for
file-list validation (missing file, wrong size, sum mismatch, absent list
unchanged); validator tests for `licence_territory_exclusions` (empty list
rejected, blank entries rejected, absent field unchanged); downloader tests
against the existing manifest fixture proving only declared files are fetched
and that today's whole-repo behaviour is byte-for-byte unchanged; validator
tests for the one-block rule extended to three kinds; install-handler tests
for `confirm_territory_eligibility` (missing or false on a territory-gated
recipe is 400 naming the confirmation, absent on an ungated recipe is a
no-op, true persists and is read back on a later preflight).

**Phase B. Runtime adapter.** `comfyui` in `allowedRuntimeKinds`, command
builder, mounts, `wait_http` on `/queue`, `verify_media_generation`,
`memoryPlan` branch. Tests: command-builder golden test; a fake ComfyUI HTTP
server (the pattern `internal/operations` already uses for Docker and HF) driving
`/prompt` and `/history` for the verification step, including the failure path
where history reports an error and the path where no output file appears.

**Phase C. Container image and recipe.** Build the pinned aarch64 CUDA 13
ComfyUI image, record its digest, and write
`internal/recipe/recipes/minimax-h3-comfyui-1s.yaml` with `trust:
basement-candidate` and `verification: candidate`. Every byte count, digest and
revision in it is read from the real registry and the real manifest. Nothing in
this phase runs on hardware; qualification is the owner's, on a Spark, and it is
what moves the recipe past candidate. Section 0 records the shipping decision,
so this phase is not blocked on it; the recipe file must set
`licence_territory_exclusions` (section 3.4) on the primary artifact.

**Phase D. API and persistence.** Section 5. Starts after the Roles work (ADR 0015) and spec 13 phases A through C merge.
Tests: handler tests against the fake ComfyUI server covering 409 for
not-active, not-media and already-running; 400 with the named limit for
out-of-range blocks and sizes; seed recording; SSE bridge emitting progress and
terminating on completion and on cancel.

**Phase E. Generate view.** Section 7, after mockup approval. Verified with the
Playwright mock harness: empty state, composing, queued, running with progress,
completed with gallery, error, and cancel. Screenshots in the report.

**Phase F. Storage integration.** Generations group in the Storage view, delete
in place, totals correct. Small, and last because it depends on D.

**Hardware seams, stubbed.** Nothing in phases A, B, D, E, F touches a GPU. The
seams are: the Docker client (already an interface with a test double), the HF
client (already faked), and the ComfyUI HTTP API (new fake, phase B). No test
downloads weights, pulls an image, or generates anything. Timing-dependent tests
use an injected clock, as elsewhere.

---

## 9. Open questions for the owner

**O1. Image in v1, video in v1, or both?** H3 is a video model with native
audio; there is no still-image mode. A still is a frame from a generated clip.
Derived from the documented 17k+5 grid, the shortest clip is 22 frames (about
0.9 s), which should be far cheaper than the 8 to 15 second clips all the
community timings cover, but no measured time for a minimum-length generation
was found. Recommendation, contingent on that measurement at qualification: ship
video as the primary output with a "save this frame" affordance in the gallery,
and only add an explicit image mode if a minimum-length generation measures fast
enough to feel like an image request rather than a video request.

**O2. How expensive may install-time verification be?** `verify_media_generation`
is a real generation. At the smallest setting it is the cheapest honest proof
the install works, but its cost is unmeasured and the surrounding timings are
tens of minutes. Alternatives: verify only that the graph validates upstream
(cheaper, weaker, and it would be the first install verification in basement
that does not prove the thing actually works), or make the smoke generation a
declared, visible step with its own timeout and console copy stating what it is
doing and roughly how long it will take once measured. The conventions say
safety machinery is not optional, so the recommendation is the real generation
with an explicit timeout and honest copy; confirm.

**O3. Does the R2V checkpoint ship?** Adding it takes the install from 42.5 GB
to 63.4 GB and unlocks reference-to-video. Both fit on a Spark. It is a product
call about scope in v1, not a technical constraint.

**O4. Audio.** H3 produces stereo audio natively. The gallery player and the
saved files carry it by default. Confirm that is wanted, and confirm there is no
mute-by-default expectation.

---

## 10. Sources

Primary:

- Model card: https://huggingface.co/MiniMaxAI/MiniMax-H3
- Licence text: https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/main/LICENSE
- MiniMax licence Q&A: https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/main/docs/QA-about-License.md
- ComfyUI day-0 announcement: https://blog.comfy.org/p/minimax-h3-day-0-support-in-comfyui
- ComfyUI H3 tutorial (canvas, frame grid, filenames, 0.30.0):
  https://docs.comfy.org/tutorials/video/minimax/minimax-h3
- ComfyUI repackaged weights: https://huggingface.co/Comfy-Org/MiniMax-H3
- ComfyUI server routes: https://docs.comfy.org/development/comfyui-server/comms_routes
- ComfyUI on DGX Spark: https://blog.comfy.org/p/comfyui-on-nvidia-dgx-spark

Secondary and community, marked as such where used:

- Component sizes, 42.5 / 63.4 / 123.6 GB breakdown, excluded territories:
  https://www.atlascloud.ai/blog/guides/minimax-h3-open-source-weights
- Spark generation times, licence discussion, official MiniMax statement, the
  litigation claim, the 5-6 GB WanGP path:
  docs/research/x-sweep-2026-08-04-ds4f-minimax-h3.md
- aarch64 / sm_121 ComfyUI builds: https://github.com/mmartial/ComfyUI-Nvidia-Docker,
  https://github.com/ecarmen16/SparkyUI, https://github.com/Triplany/comfyui-dgx-spark,
  https://github.com/AEON-7/comfyui-aeon-spark, https://github.com/Sggin1/DGX-SPARK
- Community still-image node pack (out of scope, on record):
  https://github.com/astropuzzo/ComfyUI-MiniMax-H3-Image-Studio

Related decisions: ADR 0003 (single active model), ADR 0011 (multi-runtime),
ADR 0012 (curated model trust), spec 13 (concurrent models and /v1 router).
