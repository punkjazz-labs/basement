# ADR 0017: H3 prompt composer for image-to-video

Date: 2026-08-05. Status: proposed. Design and mockup only.

## Decision summary

Basement should ship a deterministic H3 image-to-video prompt composer before
it ships prompt rewriting by another model.

The first version combines four ideas:

1. A short motion plan written in ordinary language.
2. A shot timeline that derives legal cut times from the chosen frame count.
3. An always-visible H3 prompt assembled from MiniMax's documented I2VA
   format.
4. A prompt check that blocks generation when it can prove the prompt is
   malformed or incomplete.

Three starting structures remove the structural blank page: Gentle motion,
Reveal something, and One spoken moment. They set the camera, shot, dialogue,
sound, and music structure without inventing facts about the uploaded image.

The optional companion model is deferred. A concrete small candidate exists,
but same-machine coexistence is not qualified, basement currently gives one
runtime the active slot on each machine, and a rewrite has no reliable
pre-generation measure of semantic improvement.

## Context

MiniMax H3 generation is expensive on one GB10. The measurements recorded in
[H3-MEASUREMENTS.md](../H3-MEASUREMENTS.md) are:

| Canvas | Frames | Clip duration | Measured wall time |
| --- | ---: | ---: | ---: |
| 1344 x 768 | 124 | 5.17 seconds | 1,061 seconds |
| 1344 x 768 | 362 | 15.08 seconds | 5,524 seconds |
| 1920 x 1088 | 124 | 5.17 seconds | 3,021 seconds |

The same source says three samples are not enough to estimate a new
generation's duration. The console should therefore not turn these values into
a countdown or a predicted finish time.

The current graph sends one free-text prompt to the
MiniMaxH3ImageToVideo node. That does not mean the model expects unstructured
prose. MiniMax documents a precise I2VA structure, camera vocabulary, shot
timing, speaker notation, dialogue tags, and audio fields.

The product problem is not teaching that syntax. The product problem is
making it difficult to submit an avoidably bad prompt without requiring the
user to learn the syntax first.

## Source research

The research copy and fetch receipts are under
[docs/reference/h3](../reference/h3/FETCH-RECORD.md). Both guides were fetched
from MiniMax's own MiniMax-H3 repository on 2026-08-05.

### Which MiniMax guide governs this mode

The filename distinction matters.

The
[base guide](../reference/h3/VIDEO_PROMPT_WRITING_GUIDE_base_en.md) explicitly
covers T2VA, I2VA, FL2VA, and L2VA. For I2VA it requires this fixed first line:

    For the target video, at 0.00 seconds into the target video, <Picture 1> (from [Shot 1]) is fully referenced.

After one blank line, it requires three core fields in this order:

    integrated_multimodal_description: [Shot 1] ...

    overall_soundscape: ...

    non_diegetic_music: ...

The
[reference guide](../reference/h3/VIDEO_PROMPT_WRITING_GUIDE_ref_en.md) is
titled Full-Reference Mode Rewrite Output Format Guide. It defines a
six-section format for richer reference-asset workflows. It uses
detailed_description instead of integrated_multimodal_description and points
back to the base guide for ordinary I2VA shot, camera, dialogue, and sound
rules.

The current one-image graph should use the base guide's I2VA format. It should
not emit the six full-reference sections merely because an image is present.
If the graph later accepts several labelled reference assets, that is a
separate mode and a separate design decision.

### Rules the composer can enforce

MiniMax's base guide establishes these rules:

- Picture 1 is the actual first frame at 0.00 seconds.
- Shot 1 has no timestamp.
- Every later shot starts with a strictly increasing cut time within the
  video's duration, formatted as MM:SS.mmm.
- A cut should introduce new information. A camera move is preferred when
  only distance or angle changes.
- A camera move is written as natural English. Its type comes from the
  documented vocabulary. Small or large amplitude and slow or fast speed are
  added only when meaningful.
- Speakers receive stable S1, S2, and subsequent identifiers in the order of
  their first vocal event.
- Dialogue keeps the user's exact words inside a language-labelled d tag.
- Overall soundscape describes ambience, physical sounds, and non-verbal
  human sounds. N/A is valid only when the user explicitly requests complete
  silence.
- Non-diegetic music describes audience-only music. N/A is valid when there
  is no such music.

The camera control maps to the exact documented expressions:

| Control value | Prompt expression |
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

Amplitude offers Normal, Small, and Large. Normal emits no amplitude phrase.
Speed offers Normal, Slow, and Fast. Normal emits no speed phrase. This keeps
the prompt natural instead of stacking labels.

## Product principles

The composer follows five principles.

### Start from the job, not the syntax

The opening question is "What changes from this image?" The screen does not
lead with integrated_multimodal_description, speaker IDs, or tags.

### Default to one continuous shot

The most compact useful I2VA plan is one opening frame, one observable change,
one end state, one camera choice, natural scene sound, and no background
music. Cuts, dialogue, silence, and music are available but are not shown as
required reading.

### Make expensive mistakes visible

The final action sits beside a plain run summary and a deterministic prompt
check. An invalid cut, empty action, malformed dialogue tag, or missing core
field must be found before generation.

### Show the real prompt

The assembled prompt is always visible. The controls demonstrate the format
through their output without asking the user to study it.

### Never claim semantic certainty

The prompt check can prove structure. It cannot prove that the image contains
the named object, that a motion is physically plausible, or that H3 will
produce the intended result. The interface says "Prompt ready", not "Good
result guaranteed".

## Recommended interaction

### 1. Choose the image and settings

The source image is labelled "First frame". Size and duration use the existing
Generate controls. Duration continues to show seconds and frames.

Cut positions are stored as frame indexes. Their displayed timestamps are
derived from the recipe's frames-per-second value. A cut cannot be placed at
frame zero or at or beyond the final frame.

Changing duration preserves legal cuts. If a shorter duration would remove a
cut, the composer moves that cut to the latest legal frame and marks the row
for review. It never silently drops a shot.

### 2. Pick a starting structure

First use shows three compact choices:

| Starting structure | What it sets |
| --- | --- |
| Gentle motion | One shot, small slow Push In, natural scene sound, no music |
| Reveal something | Two shots with one derived cut, a changed viewpoint, natural scene sound, no music |
| One spoken moment | One shot, Static Shot, one dialogue row, natural scene sound, no music |

These are complete structural starts, not scene-content templates. A worked
example is available for each, but loading one never inserts a cyclist, a
person, a product, or any other subject claim into the user's prompt.

The user still supplies the one fact basement cannot know deterministically:
what should move or change in this specific image. The field is labelled with
that question. This is a much smaller blank than a raw prompt and it avoids
pretending that a generic worked prompt matches an arbitrary image.

### 3. Build the shot timeline

Each shot row contains:

- Starts, derived from the duration. Shot 1 is fixed at 00:00.000.
- What happens, written in ordinary language.
- What is true at the end of the shot.
- Camera move.
- Amplitude and speed when the selected move accepts them.

The first row states that appearance, composition, important objects, and
spatial relationships come from Picture 1. The user does not re-describe the
image merely to satisfy syntax.

"Add a cut" creates the next shot at the midpoint of the remaining duration.
The row asks "What new information does this shot show?" This wording carries
MiniMax's cut rule without making the user read the guide.

### 4. Add dialogue only when needed

"Add spoken words" expands a dialogue row with:

- Speaker, named in plain language.
- Spoken language.
- Exact words.
- On screen or off-screen voiceover.
- Continues across the next cut.
- Cut off by the end of the video.

Basement assigns S1, S2, and later identifiers by first vocal event. The user
never types an identifier. The composer preserves the exact dialogue text and
adds the documented d, scenetrans, and cutoff tags.

For voiceover, the serializer uses MiniMax's documented phrase and states that
the corresponding on-screen character's lips remain closed.

### 5. Make sound choices explicit

Sound in the scene defaults to "Natural scene sound". Until the user adds
specific sounds, it emits a neutral sentence saying that natural ambience and
physical sounds match the visible setting and actions. Selecting "Silent"
is the explicit request that emits N/A for overall_soundscape.

Background music defaults to "No music", which emits N/A for
non_diegetic_music. Selecting "Add music" opens Instrumentation, Tempo, and
Change over time. Those labels match the detail MiniMax requests.

This differs from defaulting both sound fields to N/A. MiniMax permits N/A for
overall_soundscape only when complete silence is explicitly requested.

### 6. Keep the assembled prompt editable

The composer has two explicit editing modes:

- Guided mode: controls own a structured prompt document and serialize it.
- Manual prompt mode: the assembled prompt is the editable source of truth.

Typing directly in the assembled prompt changes to Manual prompt mode. Guided
controls pause and the prompt check validates the edited text. "Return to
guided composer" restores the last guided version after a confirmation. No
control change silently overwrites manual edits.

Draft state, including the manual prompt, is saved locally after every change.
Starting a generation clears only the source image from browser memory. The
prompt and settings remain available until the generation request has been
accepted.

## Prompt check

The check is a local deterministic pass. It has no network or model cost.

Generation is disabled for a blocking failure:

| Check | Failure caught before generation |
| --- | --- |
| First-frame instruction | Missing or changed I2VA instruction |
| Core fields | Missing, repeated, or out-of-order field |
| Shot numbering | Missing Shot 1, skipped number, or repeated number |
| Shot times | Timestamp on Shot 1, non-increasing cut, or cut outside duration |
| Required content | Empty action or unresolved starter placeholder |
| Camera | Expression outside the documented set |
| Dialogue | Missing language, empty words, unbalanced tag, or unstable speaker ID |
| Audio | Empty field, or N/A soundscape without Silent selected in guided mode |
| Prompt size | Prompt over the graph or API limit |
| Source | Missing first-frame image |

The following are non-blocking review items because a deterministic checker
cannot prove them:

- a cut whose new information is not described;
- too many actions for the chosen duration;
- a subject or object not visible in the source image;
- conflicting motion or end states;
- pronunciation and performance quality;
- whether the result will look good.

The check summarizes the request in one line before the primary action, for
example:

    5.2 seconds, 124 frames, 1344 x 768, 2 shots, 1 spoken line, natural sound, no music

This is not an estimate. Every value comes from the current request.

## Worked I2VA output

The filled state in the mockup assembles this prompt:

    For the target video, at 0.00 seconds into the target video, <Picture 1> (from [Shot 1]) is fully referenced.

    integrated_multimodal_description: [Shot 1] Live-action, cinematic, the cyclist shown in <Picture 1> remains on the rain-darkened platform, preserving the subject's appearance, clothing, bicycle, umbrella, position, and the station layout. The camera trucks right with small amplitude at slow speed as the cyclist releases one hand from the bicycle handle, raises the closed umbrella, and presses the runner upward. The canopy opens and beads of water roll from its fabric. [Shot 2] At 00:03.000, the camera cuts to a medium close shot. The camera holds a static shot as the cyclist (S1), in a calm, clear voice, looks toward the tracks and says: <d>[English] Here comes the rain.</d> The cyclist closes their lips, settles the umbrella over one shoulder, and remains beside the bicycle through the final frame.

    overall_soundscape: Steady rain taps on the platform roof and umbrella while distant rail noise and the soft click of the umbrella runner remain audible.

    non_diegetic_music: N/A

This example uses the base guide's I2VA instruction and three fields. It does
not use the full-reference guide's six-section rewrite format.

## Options considered

### A. Deterministic composer

Strengths:

- Emits the documented syntax every time in guided mode.
- Has no model download, startup, inference, or network dependency.
- Can prove many expensive mistakes before generation.
- Shows exactly how each plain control changes the final prompt.
- Behaves identically on one machine and a fleet.

Weaknesses:

- Cannot understand the source image.
- Cannot improve a vague idea without asking for at least one image-specific
  sentence.
- A full set of controls can become a form that feels like documentation.

Decision: adopt it, but use progressive disclosure and a one-shot default so
most users see only the image, one motion question, camera, sound, settings,
and final check.

### B. Small companion model

A concrete later candidate is
Qwen/Qwen2.5-1.5B-Instruct-GGUF, Q4_K_M, from the official Qwen repository.
The evidence captured on 2026-08-05 is in
[COMPANION-MODEL-EVIDENCE.md](../reference/h3/COMPANION-MODEL-EVIDENCE.md).
At repository revision 91cad51170dc346986eccefdc2dd33a9da36ead9, the
Q4_K_M file is 1,117,320,736 bytes. The model card identifies it as a
1.54B-parameter instruction-tuned model and specifically claims improved
instruction following and structured output generation.

That is a reasonable prompt-rewrite candidate, not a qualification.

#### Cost on one machine

The H3 measurements reached 96.74 GB peak and report at least 25 GB free at
the busiest observed point. A 1.12 GB artifact is arithmetically smaller than
that observed headroom. Artifact size is not runtime peak memory, however.
Context cache, runtime buffers, container overhead, and simultaneous peaks
have not been measured.

More importantly, ADR 0003 and the media-generation decision give one runtime
the active slot on each machine. H3 is the active model while Generate is
available. A separate text runtime cannot remain active beside it today even
if the bytes appear to fit.

Switching away from H3, rewriting the prompt, and switching back would add two
model lifecycle transitions to a task that should feel immediate. It also
creates more failure and rollback paths than the prompt helper is worth.

Conclusion for a single-machine install: do not show a disabled Rewrite
control and do not switch models behind the user's back. The deterministic
composer is the complete experience.

#### Cost with a second machine

A second machine can keep a text model active while H3 runs on the first.
ADR 0016 proposes independent placements on separate fleet nodes, but its
status on 2026-08-05 is design only. ADR 0013 also states that the head does
not proxy inference to a model on its peer.

A companion service on the second machine therefore still needs:

- a pinned recipe and runtime qualification;
- an authenticated cross-manager inference path or a dedicated prompt-helper
  route;
- placement and availability behavior;
- a clear single-machine fallback;
- a versioned rewrite instruction derived from the fetched MiniMax guide;
- output validation and one repair attempt;
- a way to keep user text and source-image details local.

The second machine avoids same-node coexistence, but it is not a zero-cost UI
feature.

#### How the user knows whether a rewrite improved anything

Format validity is measurable. Semantic improvement is not.

If this option is built later, the model must return a candidate, never
replace the user's prompt automatically. Basement should show:

- the original direction;
- the rewritten prompt;
- a short list of added or changed facts;
- the deterministic prompt-check result;
- Use original and Use suggestion choices.

"Format check passed" is not presented as "better prompt". Before release,
the companion must be evaluated on a fixed image-and-intent set. Original and
rewritten prompts should run with the same image, seed, size, and frame count,
then be rated without revealing which prompt produced which result. The
twenty-minute feedback loop makes that evaluation expensive, but skipping it
would leave no evidence that the helper earns its download and complexity.

Decision: defer. If revisited, use it to suggest missing visual and sound
detail after the deterministic structure is valid. Do not use it as the
syntax authority.

### C. Complete worked prompts

Strengths:

- Remove the intimidating empty text area.
- Cost nothing at runtime.
- Give users concrete language to edit.
- Work when the optional companion is unavailable.

Weaknesses:

- A prompt written for a different image can be syntactically perfect and
  semantically wrong.
- Long examples increase reading.
- Users may preserve irrelevant subject, setting, or camera details and pay
  for a generation before seeing the mismatch.

Decision: ship worked examples behind the three starting structures, but do
not paste their scene facts into a real prompt. The selected structure
prefills only format and safe defaults. The image-specific action remains a
short required answer.

### D. Prompt insurance: motion plan, timeline, and local preflight

This is the recommended addition beyond the three named options.

The motion plan asks what changes and what the last frame should show. The
timeline turns duration into legal shot times. The preflight blocks defects
that can be proved locally. Together they act like prompt insurance: they do
not make the model infallible, but they reduce the chance of spending a long
run on a missing action, illegal cut, malformed dialogue, or incomplete
sound field.

This option has more value than making already-valid prose slightly richer
because it moves discovery of a known defect from after generation to before
generation.

Decision: adopt it as the center of the first release.

## Why this is the recommendation

The twenty-minute feedback loop changes the priority order.

1. Prevent a provable bad request.
2. Remove the structural blank page.
3. Make the intended motion and end state reviewable.
4. Preserve manual control.
5. Improve already-valid prose.

The deterministic composer, starting structures, and prompt check address the
first four without another runtime. A companion model addresses the fifth and
introduces uncertainty about memory, placement, availability, and whether its
answer is actually better.

The recommended design also keeps MiniMax's format as data and code, not as
help text. The user sees short questions. Basement takes responsibility for
the labels, tags, timestamps, and camera phrases.

## Delivery order

### Ship first

- Source image as the fixed first frame.
- Size and duration before the timeline.
- Three starting structures.
- One-shot motion plan by default.
- Optional cuts with frame-derived timestamps.
- Exact camera vocabulary with plain labels.
- Optional dialogue rows with automatic speaker IDs and tags.
- Explicit natural sound or silence.
- Explicit no music or described music.
- Always-visible assembled prompt with Manual prompt mode.
- Deterministic prompt check and run summary.
- Local draft recovery.
- The approved static
  [mockup](../../webui/mockups/h3-prompt-composer.html).

### Wait

- A companion rewrite model.
- Full-reference six-section prompts.
- Image-content understanding.
- Semantic quality scoring.
- A low-resolution H3 preview, because the measured and documented frame
  floor does not establish a cheap preview path.
- Duration estimates, until this machine has enough comparable completed
  generations to support an honest model.

## Consequences

- The UI contains more structure than a single prompt field, but first use
  exposes only the minimum path and uses the existing Generate visual
  language.
- The prompt serializer becomes a product contract. MiniMax guide changes
  require a reviewed serializer update and fixture prompts.
- Manual prompt mode remains available to expert users and for future guide
  changes the controls do not yet express.
- A prompt passing local checks may still produce a poor clip. The console
  states that boundary honestly.
- A later companion model can sit after the deterministic composer without
  changing the single-machine baseline.

## Verification required before implementation

- The owner approves the static mockup.
- The implementation team confirms the graph and API prompt-size limit.
- Serializer fixtures cover the exact I2VA instruction, field order, every
  camera expression, shot timestamps, speaker numbering, dialogue tags,
  scene transitions, cutoff, silence, and no music.
- Browser checks cover blank first use, every starting structure, duration
  changes that affect cuts, manual prompt mode, draft recovery, narrow screens,
  keyboard order, and screen-reader labels.
- No implementation claims a duration estimate from the three measurements
  in this ADR.
