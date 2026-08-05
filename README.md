# basement

Your own AI, running on your own machine, in about ten minutes.

basement turns a GB10 desktop (NVIDIA DGX Spark, ASUS Ascent GX10, MSI
EdgeXpert) into a private AI server. You pick a model from a short curated
list, click Install, and a few minutes later you are talking to it. Nothing
you type leaves your network. There is no account to make and nothing phones
home.

## What you can do with it

- **Chat with a large model.** Pick one, install it, talk to it in the
  console. The playground is right there.
- **Make video with sound.** Describe a scene in words and get back a real
  video with an audio track, generated on your own hardware.
- **Point your apps at it.** One OpenAI-compatible URL with API keys you
  manage yourself. Existing clients work unchanged, and the URL stays the
  same when you switch models.
- **See what your machine is doing.** Live GPU memory, power, temperature
  and serving speed, measured on your hardware rather than quoted from
  somewhere else.
- **Run more than one Spark.** Add a second machine and see both on one
  screen, each serving its own model.

## Get it

Two computers are involved, and it is worth being clear about which is
which. The **Spark** is the GB10 machine that will run the models. The
**laptop** is whatever computer you are sitting at right now, on the same
network as the Spark.

You run the installer on the laptop. It looks for GB10 machines on your
network, lets you pick one, connects to it over SSH with the credentials you
already use (nothing is stored), installs basement on the Spark, and opens
the paired console in your browser. Nothing stays behind on the laptop: the
installer is a one-shot tool, not something you keep.

There are three ways to run it. They all do the same install.

### macOS, no terminal

Download [**basement-setup-macos.dmg**](https://github.com/punkjazz-labs/basement/releases/latest/download/basement-setup-macos.dmg),
open it, and double-click **Basement Setup**. The wizard opens in your
browser and takes it from there. When it is done, eject the disk image.

One download covers both Apple Silicon and Intel Macs. It is signed with our
Apple Developer ID and notarized by Apple, and the notarization ticket is
stapled to the download itself, so it opens without a warning even on a Mac
that is offline.

### Windows, no terminal

Download
[**basement-setup-windows-amd64.exe**](https://github.com/punkjazz-labs/basement/releases/latest/download/basement-setup-windows-amd64.exe)
and double-click it. The wizard opens in your browser. On an Arm PC take
`basement-setup-windows-arm64.exe` instead.

We do not have a Windows code signing certificate, so the first time you run
it Windows shows a blue box: *Windows protected your PC*. Click **More
info**, then **Run anyway**. SmartScreen is telling you something true, which
is that this file is not signed by a certificate it recognises, and we would
rather say so than work around it. If you want to check the file yourself
first, every release publishes
`basement-setup-windows-amd64.exe.sha256` next to the binary.

### Either, with a terminal

On macOS or Linux:

```bash
curl -fsSL https://github.com/punkjazz-labs/basement/releases/latest/download/setup.sh | sh
```

On Windows (PowerShell):

```powershell
irm https://github.com/punkjazz-labs/basement/releases/latest/download/setup.ps1 | iex
```

This downloads the basement binary for the machine you are on, checks it
against its published SHA-256, and runs `basement setup`, which is the same
flow as the two installers above with terminal prompts instead of a browser
page. This is also the path to use when you are sitting at the Spark itself:
run it there and it installs locally, no SSH involved.

### What a run does

Two Sparks are one run, not two. If the sweep found another GB10-class
machine, the installer offers to set that one up as well, asking once per
machine and connecting to nothing you did not say yes to. If a second machine
cannot be installed, the first one keeps its result: it is already running.

## What a recipe is

Everything basement installs comes from a recipe, and it is worth knowing
what one is, because it is where the trust comes from.

A recipe is a small immutable file that pins exactly what you are about to
run: the weights by repository, revision and byte count; the runtime
container by image digest rather than a moving tag; the licence you are
accepting; and how much memory and disk the model actually needs.

That is what lets basement check, before it downloads anything, whether a
model will fit on your machine, and refuse politely rather than half-install
it. During the install it verifies what it downloaded against the pinned
bytes. Every step leaves a receipt you can open in the console and read.

## The models

Nine recipes ship inside the binary. A few worth starting with:

- **Qwen 3.6 35B-A3B (Unsloth)**, the recommended all-rounder;
- **Qwen 3.6 27B** from NVIDIA, flagship-level coding in a smaller footprint;
- **Laguna S 2.1 + DFlash** from poolside, built for long agent runs;
- **MiniMax H3**, which generates video and audio together from a written
  prompt on a single Spark.

MiniMax H3 is the first media model in the catalogue. It is worth knowing two
things before you install it. A generation is minutes to hours rather than
seconds: measured on one Spark, the default size takes about eighteen
minutes and the largest tested canvas takes over two. And its licence
excludes the European Union, the United Kingdom, the Republic of Korea and
the United States of America, from use as well as distribution, so the
install asks you to confirm your territory before it starts.

All nine recipes declare themselves **candidates**. A candidate has not
completed the evidence and curation decision required for a verified label.
Passing part of qualification does not promote it automatically.

The pipeline behind the pack is documented in
[`docs/MODEL-CANDIDATES-2026-08.md`](docs/MODEL-CANDIDATES-2026-08.md):
which models the community actually runs on this hardware, checked against
primary sources before anything enters the catalog.

## Keeping it up to date

The console tells you when a new version exists, at the bottom of the
sidebar. Click it and basement updates itself: it downloads the new release,
checks it against a signature only we can produce, installs it beside the
running version rather than over it, restarts, and confirms the new version
answers. If it does not, the previous version is put back automatically.

Your models keep serving through the restart. The manager updating itself
does not stop a model container.

One exception, once. A Spark installed before this mechanism existed has no
updater to trigger, so it needs the installer run against it one final time.
After that the button works.

## What we do

Five steps, always in this order.

1. **Research.** We sweep what the community actually runs on GB10
   hardware: X, NVIDIA forums, Hugging Face. Each recipe records its source,
   immutable revision, expected bytes, and licence. The current sweep is
   [`docs/MODEL-CANDIDATES-2026-08.md`](docs/MODEL-CANDIDATES-2026-08.md).
2. **Validate.** Qualification installs and runs an exact recipe version on
   owned hardware, exercises its lifecycle, and records real inference and
   measured results. This has not been completed for every embedded recipe.
   The protocol and recorded outcomes are
   [`docs/DGX-QUALIFICATION.md`](docs/DGX-QUALIFICATION.md).
3. **Promote.** Qualification evidence is reviewed before a recipe can move
   from candidate to verified. Right now all nine embedded recipes still
   declare candidate.
4. **Package.** The recipe pins everything: weights by revision and byte
   count, runtime by image digest, the licence you are accepting, and the
   resources the model needs.
5. **Ship.** The target is for recipes to reach you through a signed feed and
   install with one click. That is the standard we are building toward: the
   feed design is set but not live yet
   ([`docs/decisions/0009`](docs/decisions/0009-signed-recipe-feed-design.md)).
   Until it ships, recipes travel inside the binary itself.

## Why you can trust an install

Before installing, basement checks your architecture, memory, disk, Docker
and the NVIDIA runtime, and refuses politely if something will not fit.
During install it verifies what it downloads against the pinned bytes. Every
step leaves a receipt you can open in the console.

Manager updates are signed with a key that never leaves one machine, and the
public half is compiled into every release build, so a manager will only
install an update it can prove we produced.

No accounts. No telemetry. No phone-home. The binary serves your network only
if you ask it to.

---

The rest of this document is for people who want the details.

## How it works

basement is one Go binary. It embeds the console (React + TypeScript), serves
the local API, persists jobs and state in SQLite, and drives Docker through
its structured API with an allowlisted lifecycle. There is no daemon zoo and
no package manager: one process, one database file, one container per active
model.

Only one model is active at a time. Starting another installed model is a
transactional switch: stop the previous runtime, start and verify the target
with health and inference checks, and roll back to the previous model if the
target fails. A benchmark job then measures real throughput on the device and
records it. Lifecycle jobs open a persistent deployment detail modal with
named phases and typed step receipts.

The console has nine tabs: Models, Roles (persistent endpoints that stay
stable while the model behind them changes), Playground, Generate, Connect,
Monitor, Fleet, Storage and Activity.

The manager defaults to loopback until an operator deliberately chooses a LAN
or Tailscale bind address (the installer asks). Model containers always bind
loopback: the manager's authenticated `/v1` endpoint, with console-managed
API keys, is the only network path to inference, so the base URL never
changes when models switch (see
[`docs/decisions/0007`](docs/decisions/0007-stable-endpoint-api-keys.md)).
An authenticated, redacted diagnostic bundle is available from the console.
The security posture of setup and discovery is documented in
[`docs/decisions/0010`](docs/decisions/0010-network-setup-and-gb10-discovery.md).

## Recipes

A recipe is schema-validated and immutable per version. It declares:

- artifacts: Hugging Face repository, pinned revision, expected bytes,
  licence and licence URL, per role (primary weights, drafter, and so on);
- runtime: kind, container image pinned by digest, environment, shared
  memory and locking requirements;
- requirements: architecture, minimum memory, reserve and safety margins,
  secrets, whether licence acceptance is required;
- service: port, served model id, and per-runtime launch configuration
  (tensor parallel size, KV cache dtype, context length, speculative
  decoding, parsers, or for a media runtime the pinned workflow graphs and
  the canvas and duration limits it allows).

Preflight evaluates pinned artifact bytes, image size, disk safety margin,
the runtime's planned unified-memory allocation, KV and context settings, and
reserved host memory. Memory is rechecked immediately before start; disk
headroom is rechecked throughout long transfers
([`docs/decisions/0004`](docs/decisions/0004-per-node-resource-guardrails.md)).
The signed recipe feed design is in
[`docs/decisions/0009`](docs/decisions/0009-signed-recipe-feed-design.md).

## Runtimes

A recipe declares a runtime kind and the manager adapts command, memory
model, health and metrics per kind, while every trust guarantee stays
identical. Four kinds are supported today, and the shipped pack uses all of
them: **vLLM** for most of the text models, **SGLang** for Inkling Small,
**llama.cpp** for single-Spark DeepSeek V4 Flash in the GGUF world, and
**ComfyUI** for MiniMax H3. No single runtime covers the frontier any more,
which is why runtime independence exists at all. Rationale, evidence and
phasing live in
[`docs/decisions/0011`](docs/decisions/0011-multi-runtime-support.md).

Media runtimes differ in one visible way: there is no chat endpoint. A
generation is submitted, queued, and reported step by step, and the finished
file lands in a gallery in the console.

## Multi-Spark

The Fleet tab finds GB10 machines on your network and adopts one for you:
it signs in over SSH, confirms it is a GB10, installs basement on it, waits
for its console, pairs the two managers, and adds it to the fleet. If you
would rather do it by hand, the same tab takes a console URL and an API key
you generate on the other Spark's Connect tab.

Once added, the Fleet tab shows every Spark and what each one is serving. A
recipe that declares two Sparks (`spark_count`) plans across exactly two
nodes: this manager as the head and the single added peer as the worker.
Each node runs its own preflight and has to pass on its own; the head refuses
to start a distributed model when no peer is configured, when more than one
is, or when the recipe does not describe the interconnect. That is the whole
supported topology: a head and one worker. No two-Spark recipe has completed
the qualification protocol on real hardware yet, so the path is built but
unproven, like every other candidate in the pack.

Updating every Spark in a fleet from one console is built but not shipped: a
failed rollout has no way out yet, so it stays on a branch rather than in a
release.

## Development

Run the manager locally:

```bash
go run ./cmd/basement --data-dir ./var --listen 127.0.0.1:7070
```

On first launch, `install.sh` prints a pairing card (URL, token, QR);
re-print it anytime with `basement pairing-url`. Production installation
places the data directory under `/var/lib/basement`.

Local verification suite:

```bash
go test ./...
go vet ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/basement
```

After changing the console, rebuild the embedded assets (CI fails if they
drift from source):

```bash
cd webui/ui && npm ci && npm run build
```

Local tests use deterministic fakes and do not pull model weights, mutate
Docker, or exercise GB10 kernels. A passing local suite and registry metadata
checks are not proof of the real-DGX acceptance criteria in `PRD.md`. For
real-hardware acceptance, follow
[`docs/DGX-QUALIFICATION.md`](docs/DGX-QUALIFICATION.md); the included
`packaging/qualify-dgx.sh` helper captures preflight, installation, real
inference, stop/start, smoke-test, and diagnostic receipts without printing
the local pairing credential.

Layout: `cmd/` entrypoints; `internal/` manager packages (recipe, operations,
httpapi, engine, store, discovery, fleet, update, setup); `webui/ui` console
source; `internal/webui/assets` the embedded production build;
`internal/recipe/recipes/` the shipped recipe pack; `internal/recipe/graphs/`
the pinned media workflow graphs; `docs/decisions/` numbered ADRs;
`docs/runbooks/` operational runbooks. Start with ADRs 0003 (transactional
single active model), 0004 (guardrails), 0007 (stable endpoint), 0008
(manager self-update), 0009 (signed feed), 0010 (setup security), 0011
(runtimes), 0016 (multi-node fleet), 0017 (H3 prompt composer).

## Releases

There are two programs. `cmd/basement` is the manager: it runs on the Spark,
and it is what the `curl | sh` bootstrap and `basement setup --binary`
download. `cmd/basement-setup` is the installer: it runs on the operator's
laptop, opens the wizard as a loopback-only browser page, and only ever
installs a Spark over SSH. It is built for macOS and Windows and never for
Linux, because a Spark installs itself through the manager. A third program,
`cmd/basement-updater`, is the small root helper that applies a verified
manager update; it is Linux and arm64 only.

Pushing a `v*` tag runs `.github/workflows/release.yml` on Linux, which
publishes as a **draft**, each asset with a `.sha256` beside it:

- `basement-{linux,darwin,windows}-{amd64,arm64}`, the manager;
- `basement-updater-linux-arm64`, the root update helper;
- `basement-setup-darwin-{amd64,arm64}`, the two macOS installer slices;
- `basement-setup-windows-{amd64,arm64}.exe`, the Windows installer, linked
  with `-H=windowsgui` so double-clicking it opens no console window;
- `setup.sh`, `setup.ps1`, `packaging.tar.gz`.

The manager builds carry the update signing key's public half, from the
`BASEMENT_UPDATE_PUBLIC_KEYS` repository variable, so a release cannot be
built without it.

The release stays a draft because everything it uploads for macOS is unsigned
until the next step replaces it. Then, on the Mac that holds the Developer ID
identity and the notarytool keychain profile:

```sh
packaging/sign-macos-release.sh v0.5.0 v0.4.11
```

The first argument is the tag; the rest are the versions this release accepts
an update from. That signs and notarizes the two darwin manager binaries in
place, calls `packaging/build-macos-installer.sh` to lipo the two installer
slices into one universal binary, wrap it in `Basement Setup.app`, and sign,
notarize, staple and verify `basement-setup-macos.dmg`, then calls
`packaging/sign-linux-update.sh` to sign the Linux manager manifest with the
Ed25519 key held in the macOS Keychain. Publishing is the last line, so a
release nobody signs stays a draft, and nobody can download an unsigned
binary out of a window between the two.

Verification is fatal at every stage: a disk image that cannot prove it
carries a stapled ticket is never uploaded, and an update manifest whose
signature does not match a key compiled into the release is never published.

The lipo happens there rather than in CI because the Mac step is required
anyway. The signing identity and the notarization credentials exist on one
laptop and nowhere else, so adding a macOS CI runner purely to run `lipo`
would remove nothing from the local script. The two darwin slices stay
published as honest build inputs; the single user-facing macOS download is
the disk image.

The Windows executables are unsigned. There is no Windows code signing
certificate, so SmartScreen warns on first run, and the install section above
documents the click-through instead of hiding it.

To rehearse the macOS packaging without cutting a release:

```sh
mkdir -p /tmp/slices
for arch in arm64 amd64; do
  GOOS=darwin GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -o /tmp/slices/basement-setup-darwin-$arch ./cmd/basement-setup
done
REHEARSE=1 SETUP_SLICE_DIR=/tmp/slices packaging/build-macos-installer.sh v0.0.0
```

That assembles and signs the bundle and stops: nothing is notarized, stapled
or uploaded, and the output is named `-REHEARSAL` so it cannot be mistaken
for a release artifact.

The app icon is `packaging/macos/basement.icns`, built from
`packaging/macos/icon.svg`, which is the product favicon copied verbatim from
the website. `packaging/macos/make-icon.sh` regenerates it; the release
scripts only copy the committed `.icns`, so cutting a release needs no SVG
renderer.
