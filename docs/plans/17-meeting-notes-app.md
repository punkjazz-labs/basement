# Spec 17: local meeting notes

A first-party app on top of basement, not a console tab. It ships from its own repository
(`punkjazz-labs/basement-notes`), addresses a Spark through the stable `/v1` endpoint with
an API key the owner generates on the Connect tab, and does not link against this
repository's internals. Branch names below are for that repository.

Every external fact below carries its source. Facts that could not be verified are marked
UNVERIFIED and must be measured, not assumed, by whoever builds this.

## Problem

Meeting-notes tools work by sending the meeting somewhere. Granola, the product this one
answers, runs on the user's machine and captures microphone plus system audio, and then
sends that audio off the device: its own documentation says it "passes audio directly from
your microphone and system audio to our transcription provider", with Deepgram and
AssemblyAI transcribing and OpenAI and Anthropic summarising
(https://docs.granola.ai/help-center/taking-notes/transcription,
https://www.granola.ai/security). It does not keep the audio, but four other companies
hear it. A proposed class action over recording participants and training on recordings
was filed on 30 July 2026 (Chamberlain v. Granola, Inc., No. 3:26-cv-07926-EMC, N.D. Cal.,
https://ppc.land/granola-sued-for-recording-meetings-without-consent-to-train-ai-models/).

The owner of a Spark already has the hardware to do all of it at home. What is missing is
the app.

## User-visible outcome

Press record before a call. Type sparse bullets during it, the way people already take
notes. Afterwards the bullets become real notes, expanded from the transcript, with every
line traceable to what was actually said. The audio never left the laptop. The transcript
went no further than the owner's own Spark.

## What Granola's product actually is, and what to copy

Worth stating plainly, because it is easy to build the wrong thing. Granola's value is not
transcription. It is the loop: the user types sparse bullets during the call, presses
Enhance notes, and the model merges those bullets with the transcript. **The notes are the
prompt.** Typing "pricing concerns" pulls every pricing exchange, with quotes. User text
renders in black, model additions in grey, and a control traces each line back to its
source in the transcript
(https://docs.granola.ai/help-center/taking-notes/ai-enhanced-notes).

That loop is architecture independent and it is the thing to clone. A local app that only
produces an unprompted summary is a worse product with a better privacy story, and it will
lose.

Granola's speaker attribution, by contrast, is weak: it merges microphone and system audio
into one stream and shows only "Me" and "Them", with real names arriving from side
channels like a Meet browser extension. Capturing two separate tracks, which this spec
does, is already parity and is the basis for beating it.

## Where each part runs, and why

**The app runs on the owner's Mac.** The meeting is there. A Spark has no microphone; its
rear panel exposes only HDMI 2.1a multichannel audio and whether it presents any ALSA
input device at all is UNVERIFIED (https://docs.nvidia.com/dgx/dgx-spark/hardware.html).
Capture happens where the sound is.

**Transcription runs on the Mac too, in v1.** The reasoning, not just the conclusion:

- A Spark serves one model at a time (ADR 0003). The model worth keeping resident is the
  one that writes notes. Putting speech recognition on the Spark costs a container swap
  before and after every meeting, and a swap is minutes.
- basement's proxy forwards every `/v1/*` path
  (`mux.HandleFunc("/v1/", server.proxyModel)` in `internal/httpapi/server.go`), so a
  speech recipe on the Spark would be reachable at `/v1/audio/transcriptions` with no
  manager change. But `inferenceTarget` finds its target by peeking a JSON body
  (`peekModelField` in `internal/httpapi/roles.go`), and an audio transcription request is
  `multipart/form-data`. There is no `model` field to find, so the request falls through
  to whatever model happens to be active. **Role addressing does not work for audio
  uploads today.** That is a real gap; record it in the report and route it to spec 13,
  not to this app.
- Local transcription also means the app works while the Spark is asleep or busy. It
  degrades to a transcript with no notes, which is still worth having.

**Notes run on the Spark**, through `/v1`, addressing `role/reasoning` (Roles shipped, see
`internal/httpapi/roles.go` and `docs/decisions/0015-roles-on-the-stable-endpoint.md`). The app must repeat the console's honest warning: if the
roles in use name different models, the first request after a switch is slower.

Revisit the split after spec 13 lands concurrent models. A resident speech model on the
Spark stops costing a swap at that point, and `parakeet.cpp` (below) already has GB10
images, so a third runtime kind under ADR 0011 becomes a realistic follow-on.

## The speech engine

Meeting audio is the workload, so the benchmark that matters is AMI, not LibriSpeech. On
AMI, NVIDIA Parakeet TDT 0.6b-v2 reports 11.16 percent WER against Whisper large-v3-turbo
at 16.13 percent, and Parakeet is CC-BY-4.0
(https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2,
https://www.marktechpost.com/2026/07/23/best-open-speech-recognition-asr-models-in-2026-wer-languages-latency-and-license-compared/).
Whisper's advantage is language coverage: 100-plus against Parakeet v3's 25
(https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3).

**Default choice for v1 on macOS: FluidAudio.** Swift, Apache-2.0 code with permissively
licensed models, CoreML on the Neural Engine, Parakeet TDT v3, and it needs no Hugging
Face token. Its README reports roughly 190x realtime on an M4 Pro
(https://github.com/FluidInference/FluidAudio). It also brings diarization, which matters
below.

Alternatives, with what each costs:

- **whisper.cpp**, MIT, the safe fallback and the only one with a first-class server mode
  (`whisper-server --inference-path /v1/audio/transcriptions`). Metal throughput on Apple
  Silicon is published per model size (https://github.com/ggml-org/whisper.cpp,
  https://www.promptquorum.com/local-llms/apple-silicon-whisper-metal-benchmark). Worse on
  meeting audio, better on languages.
- **Apple SpeechAnalyzer / SpeechTranscriber**, macOS 26 and later, no model to ship, but
  around 30 locales and a hard OS floor
  (https://developer.apple.com/documentation/speech/speechanalyzer).
- **MLX Parakeet** is not the fast path on a Mac; CoreML and the Neural Engine are
  (https://github.com/senstella/parakeet-mlx).
- **faster-whisper / CTranslate2 is out on macOS** (no Metal backend, CPU only) and is
  currently broken on ARM64 with CUDA: no `ctranslate2[cuda]` wheel exists for linux/arm64
  and pip silently installs the CPU build
  (https://github.com/speaches-ai/speaches/issues/620). Do not build on it.

For the later Spark-side option, the relevant find is `mudler/parakeet.cpp`: MIT, ggml
C++, with prebuilt multi-arch CUDA 13 images that explicitly cover GB10 and Grace
Blackwell (https://github.com/mudler/parakeet.cpp). NeMo Parakeet runs on GB10 directly
with published RTFx figures (0.6b-v3 at 282.9x, tdt-ctc-110m at 651.9x) but pins an exact
NVIDIA PyTorch container
(https://forums.developer.nvidia.com/t/running-parakeet-speech-to-text-on-spark/356353).
There is **no measured whisper.cpp throughput on GB10** anywhere; forum reports are
qualitative only. UNVERIFIED.

**What the executor must still measure**, on the owner's actual Mac, with an actual hour of
real meeting audio: realtime factor, peak memory, and word error rate against a
hand-corrected transcript, for the chosen engine and one fallback. Put the numbers in the
report. No number reaches the product that was not measured here.

## Speakers

Two separate capture tracks give perfect "you" versus "everyone else" with no model and no
licence question, which is already parity with Granola. Ship that in v1 and label it
honestly: the labels come from which device the audio arrived on, not from recognition.

Real diarization, telling three remote participants apart, is a later spec. When it comes:

- **FluidAudio** on macOS reports 17.7 percent DER on AMI, Apache-2.0, no token
  (https://github.com/FluidInference/FluidAudio).
- **sherpa-onnx** is the portable option: Apache-2.0, pyannote segmentation plus speaker
  embeddings in ONNX, prebuilt arm64, and a reported real-world footprint around 45 MB
  running CPU-only (https://k2-fsa.github.io/sherpa/onnx/speaker-diarization/index.html).
- **pyannote.audio** is MIT in code but its weights are Hugging Face gated, and the better
  models are a paid tier (https://huggingface.co/pyannote/speaker-diarization-community-1).
- **NeMo `diar_sortformer_4spk-v1` is CC-BY-NC-4.0 and therefore unusable** in a product.
  The streaming v2 is CC-BY-4.0 but its GB10 viability is UNVERIFIED
  (https://huggingface.co/nvidia/diar_streaming_sortformer_4spk-v2).
- **whisper.cpp `-tdrz` is not diarization.** It emits speaker-change markers with no
  identity, has exactly one English-only checkpoint, and its author describes it as a
  paused proof of concept (https://github.com/akashmjn/tinydiarize). Do not build on it.
  `--diarize` in the same project is a stereo left/right heuristic.

## Capturing what the other side says

**Use Core Audio process taps, not ScreenCaptureKit.** ScreenCaptureKit is a video API
with audio attached: an audio-only capture drops frames unless a dummy video output is
attached, it requires Screen Recording permission with the recording indicator and
Sequoia's periodic re-authorisation, and microphone capture through it needs macOS 15.
Taps are audio-only, work per process, can exclude the app's own audio, and use a separate
TCC category (https://developer.apple.com/documentation/coreaudio/catapdescription,
https://developer.apple.com/documentation/coreaudio/capturing-system-audio-with-core-audio-taps,
https://developer.apple.com/forums/thread/718279).

**BlackHole is no longer needed.** A tap observes output without rerouting it, so the
owner keeps hearing the meeting. BlackHole is also GPL-3.0
(https://github.com/ExistentialAudio/BlackHole).

Constraints that shape the build, all of them non-negotiable
(https://dgrlabs.co/blog/2026-04-25-capturing-system-audio-on-macos-in-2026.html):

- The aggregate device needs a real output device as its main sub-device with the tap as a
  sub-tap and `kAudioAggregateDeviceTapAutoStartKey` true. Tap-as-main with no sub-devices
  produces silence with no error.
- `AVAudioEngine` cannot retarget arbitrary HAL devices; use
  `AudioDeviceCreateIOProcIDWithBlock`.
- **The app must be a signed `.app` bundle.** TCC keys off the signing identity, an
  unsigned build never even raises the prompt, and bare executables stopped appearing in
  the Privacy pane on recent releases (https://developer.apple.com/forums/thread/807898).
  Notarised non-App-Store distribution is fine. App Sandbox plus taps is fragile.
- `NSAudioCaptureUsageDescription` has to be typed into the plist by hand and there is no
  public API to query or request the permission, so the app cannot show its own state
  reliably and must handle "granted nothing, recorded silence".
- Taps show **no menu bar indicator**. The app therefore owns the honesty here and must
  show an unmistakable recording state of its own. Do not treat the missing indicator as a
  feature.

Known live problems to design around: taps have been reported delivering all-zero PCM
after several minutes on a macOS 26.5 beta, recoverable only by tearing the tap down and
rebuilding it, and zeros are indistinguishable from real silence
(https://developer.apple.com/forums/thread/825780). macOS 26.1 fixed several audio-capture
bugs including a system-wide low-pass filter applied to captures
(https://weblog.rogueamoeba.com/2025/11/04/macos-26-tahoe-includes-important-audio-related-bug-fixes/).
The floor and the periodic teardown are the executor's call, with evidence, in the report.

Reference implementations: `insidegui/AudioCap` is the canonical one and is BSD-2
(https://github.com/insidegui/AudioCap). `makeusabrew/audiotee` has the right shape but
**no LICENSE file at all**, so it is legally unusable; do not copy from it.

For the eventual Linux client, capture is easy and better: every PipeWire or PulseAudio
sink exposes a monitor source, `@DEFAULT_MONITOR@` is a valid target, and per-application
capture works by node name (https://www.mankier.com/1/pactl,
https://docs.pipewire.org/page_man_pw-cat_1.html). Downsample to 16 kHz mono; whisper.cpp
and Silero VAD accept 16 kHz only.

## Shape of the program

Capture must be native Swift on macOS. Everything above it does not have to be. Two
viable shapes, and the executor picks one in the report with reasons:

1. **Go core plus a Swift capture sidecar, inside a signed `.app`.** The core serves a
   loopback page in the owner's browser, exactly as `internal/setupweb` does in this
   repository, and the `.app` bundle plus signing plus notarisation reuses
   `packaging/macos/Info.plist`, `packaging/build-macos-installer.sh` and
   `packaging/sign-macos-release.sh`, which already do all of this for the setup app. The
   console design system transfers directly.
2. **A native Swift app.** Better system integration, a second design language to
   maintain, and nothing this project has built before.

Prior art worth reading before choosing, both MIT: `michaelwilhelmsen/humla` (Tauri and
Rust with Swift sidecars, two separate capture streams, FluidAudio diarization, and it
sends notes and transcript to the model as two labelled inputs, which is exactly the
Granola loop) and `pasrom/meeting-transcriber` (Swift, Core Audio taps, WhisperKit or
Parakeet, dual-track diarization). humla is the closest architectural match to shape 1.

## Storage

A folder the owner picks, one directory per meeting:

```
2026-08-04-1400-pricing-call/
  notes.md        front matter: title, date, duration, engine and model used
  transcript.md   timestamped, two-track speaker labels
  audio/          mic.wav, system.wav
  meta.json       durations, engine and model versions, what ran where
```

Markdown, so the owner can leave. No database, no proprietary container, no sync.

If the folder sits inside iCloud Drive or Dropbox, the app says so plainly: `This folder
syncs to <service>. Your notes will be copied there.` Detecting the common sync paths is a
few lines and is the difference between honesty and a technicality.

**Audio is deleted after a successful transcription by default**, with an opt-in to keep
it. The recording is the most sensitive thing in the directory and the one the owner will
never think about again. Granola discards audio too, and that is the one part of its
design worth copying without changes.

## Notes generation

1. Send the transcript and the owner's bullets as two labelled inputs. Never send audio.
2. Enhance is explicit and repeatable. It runs when the owner asks, it can run again, and
   re-running never destroys what the owner typed.
3. Owner text and model text are visually distinct, and every model line carries a link
   back to the transcript lines it came from. A line the model could not anchor is either
   dropped or shown in a separate unanchored section; the mockup decides which. A model
   inventing a follow-up nobody agreed to is the failure that kills trust in this
   category, and the prompt says so in as many words.
4. Chunk with overlap for long meetings, summarise chunks, then summarise summaries.
5. Stream the output as it generates, with the console's existing rendering pattern:
   `marked` plus `DOMPurify.sanitize`. No new dependency.
6. When the Spark does not answer, the transcript is still written and the notes stay
   pending, retried on demand: `Your Spark did not answer, so the transcript is saved and
   the notes are waiting.` Never lose a meeting because a machine was off.

## The privacy claim, exactly

What the app may say:

> The recording and the transcript are made on this Mac. The transcript is sent to your
> Spark at `<base URL>` to write the notes. Nothing is sent to any other service.

That claim is stronger than Granola's on the axis that matters, and it is checkable: the
only outbound connection the app opens is to the configured Spark.

What it must not say: that nothing leaves the machine (the transcript leaves this Mac for
the Spark, over the owner's own network), that the recording is lawful, or that consent
was obtained. One first-run note, once, plainly: `basement does not tell the other people
in the room that you are recording.` Given the litigation cited above, that line is
product design, not legal decoration.

If the notes folder syncs, the claim on screen names the service too. The claim is always
about the configuration actually in force.

## Build plan

1. **Skeleton.** The chosen shape, loopback UI, configuration screen (Spark base URL, API
   key, notes folder), and a `/v1/models` probe that proves the key works before anything
   else is attempted.
2. **Capture.** Two-track recording to disk, level meters that prove signal is arriving
   (the all-zero-PCM failure above makes this a feature, not a decoration), start and
   stop, the permission copy, an unmissable recording indicator, and the signed `.app`
   with `NSAudioCaptureUsageDescription`. Ship this and record a real meeting before
   writing more.
3. **Transcription.** The chosen engine behind an interface, progress, `transcript.md`
   with timestamps and two-track labels. Measurements in the report.
4. **The enhance loop.** Bullet editor, two-input prompt, streaming render, traceability
   mapping, `notes.md` with front matter.
5. **Library.** Meeting list, full-text search over the folder, rename, delete including
   audio, open in the owner's editor.
6. **Packaging.** Signing and notarisation through the same identity and notary profile
   `packaging/sign-macos-release.sh` uses. An app that records audio and is not notarised
   should not get past a cautious user.

## Test strategy

Three hardware seams, all stubbed, the way this project stubs Docker and the GPU:

- **Capture** behind an interface with a fake emitting a fixture wav. No test needs a
  microphone or a permission grant. One test asserts the all-zero-PCM detector fires on a
  silent fixture and does not fire on a quiet one.
- **The speech engine** behind an interface returning a golden transcript. Chunking,
  timestamp arithmetic and two-track labelling are deterministic against it.
- **The Spark** behind `httptest`. Cases: a clean streamed enhance; a response asserting a
  follow-up with no transcript anchor (dropped or marked, asserted); a connection refused
  mid-generation leaving the transcript intact and notes pending; a 401 whose message
  names the key and the Connect tab; a re-run of enhance that leaves owner-typed text
  byte-identical.

Plus: audio deleted when the setting says so and kept when it does not, asserted on disk;
a sync-folder path detected and named; `meta.json` recording the engine and model actually
used.

One optional integration test behind a build tag may hit a real Spark when an environment
variable names one. Nothing in the default suite touches hardware or the network.

## Open questions (owner)

- **Which shape**, Go core plus Swift sidecar in an `.app`, or a native Swift app. This is
  the decision with the longest tail.
- **Mac only?** Sparks are ARM Linux and this app is macOS. An owner with a Spark and a
  Windows laptop gets nothing from v1. Acceptable for a first release?
- **macOS floor.** Taps need 14.2 and are documented as best from 14.4. The 26.1 audio
  fixes argue for a higher floor. Where does it land, and does the demo machine meet it?
- **Keeping audio.** The default here is delete. For a firm that needs the recording as a
  record, the default is wrong and belongs in the first-run choice instead.
- **Calendar.** Titling from the calendar entry, and Granola's trigger for meetings with
  two or more attendees, are the biggest quality-of-life features and the first place a
  local app can start reaching outside itself. Not in this spec. Wanted?
- **Which role.** If the owner has assigned no `role/reasoning`, should the app fall back
  to whatever is active, or refuse and send them to the Roles page?
