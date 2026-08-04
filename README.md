# basement

Install and manage local AI models on your DGX Spark with one click.

basement turns a GB10 machine (NVIDIA DGX Spark, ASUS Ascent GX10, MSI
EdgeXpert) into a private AI server you can actually trust. It finds your
machine, installs a curated model, verifies that everything it downloaded
is exactly what it should be, and gives you a console and an
OpenAI-compatible endpoint. Your prompts never leave your network.

## Install

Two computers are involved, and it is worth being clear about which is
which. The **Spark** is the GB10 machine that will run the models. The
**laptop** is whatever computer you are sitting at right now, on the same
network as the Spark.

You run the installer on the laptop. It looks for GB10 machines on your
network, lets you pick one, connects to it over SSH with the credentials
you already use (nothing is stored), installs basement on the Spark, and
opens the paired console in your browser. Nothing stays behind on the
laptop: the installer is a one-shot tool, not something you keep.

There are three ways to run it. They all do the same install.

### macOS, no terminal

Download [**basement-setup-macos.dmg**](https://github.com/punkjazz-labs/basement/releases/latest/download/basement-setup-macos.dmg),
open it, and double-click **Basement Setup**. The wizard opens in your
browser and takes it from there. When it is done, eject the disk image.

One download covers both Apple Silicon and Intel Macs. It is signed with
our Apple Developer ID and notarized by Apple, and the notarization ticket
is stapled to the download itself, so it opens without a warning even on a
Mac that is offline.

### Windows, no terminal

Download
[**basement-setup-windows-amd64.exe**](https://github.com/punkjazz-labs/basement/releases/latest/download/basement-setup-windows-amd64.exe)
and double-click it. The wizard opens in your browser. On an Arm PC take
`basement-setup-windows-arm64.exe` instead.

We do not have a Windows code signing certificate, so the first time you
run it Windows shows a blue box: *Windows protected your PC*. Click **More
info**, then **Run anyway**. SmartScreen is telling you something true,
which is that this file is not signed by a certificate it recognises, and
we would rather say so than work around it. If you want to check the file
yourself first, every release publishes
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
page. This is also the path to use when you are sitting at the Spark
itself: run it there and it installs locally, no SSH involved.

### What a run does

Two Sparks are one run, not two. If the sweep found another GB10-class
machine, the installer offers to set that one up as well, asking once per
machine and connecting to nothing you did not say yes to. When it has set
up two machines it prints both console addresses and the three steps that
pair them, which you follow in the console (see Multi-Spark below). If a
second machine cannot be installed, the first one keeps its result: it is
already running.

Running the installer again on a machine that already has basement is also
how you update it. It replaces the binary and the service file and restarts
the service; your models, API keys, jobs and settings stay where they are.

## What you get

- A console. Pick a model, click Install, watch every step complete with
  its own receipt. Talk to the model in the playground when it is up.
- An endpoint. One stable OpenAI-compatible URL with API keys you manage
  in the console. Point any client at it; the URL never changes when you
  switch models.
- Honest numbers. Speeds marked typical are community reported. After
  install, basement measures the real tokens per second on your machine
  and shows that instead.
- Live monitoring. GPU memory, power, temperature, and serving metrics,
  sampled from the running model.

## The models

The console currently highlights three candidates:

- Unsloth Qwen 3.6 35B-A3B, the recommended all-rounder;
- NVIDIA Qwen 3.6 27B, flagship-level coding in a smaller footprint;
- poolside Laguna S 2.1 with its DFlash drafter, built for long agent runs.

All eight embedded recipes declare themselves candidates. A candidate has not
completed the evidence and curation decision required for a verified label.
Passing part of qualification does not promote it automatically.

The pipeline behind the pack is documented in
[`docs/MODEL-CANDIDATES-2026-08.md`](docs/MODEL-CANDIDATES-2026-08.md):
which models the community actually runs on this hardware, checked
against primary sources before anything enters the catalog.

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
   from candidate to verified. Right now all eight embedded recipes still
   declare candidate. The recipe data carries that state, but the Models table
   does not display it yet.
4. **Package.** The recipe pins everything: weights by revision and byte
   count, runtime by image digest, the licence you are accepting, and
   the resources the model needs.
5. **Ship.** The target is for recipes to reach you through a signed feed and
   install with one click. That is the standard we are building toward: the feed
   design is set but not live yet
   ([`docs/decisions/0009`](docs/decisions/0009-signed-recipe-feed-design.md)).
   Until it ships, recipes travel inside the binary itself.

## Why you can trust an install

Every model comes from a recipe: an immutable file that pins the exact
weights (repository, revision, byte count), the exact runtime container
(image digest, never a tag), the licence you are accepting, and the
resources the model needs. Before installing, basement checks your
architecture, memory, disk, Docker, and the NVIDIA runtime, and refuses
politely if something will not fit. During install it verifies what it
downloads against the pinned bytes. Every step leaves a receipt you can
open in the console.

No accounts. No telemetry. No phone-home. The binary serves your network
only if you ask it to.

---

The rest of this document is for people who want the details.

## How it works

basement is one Go binary. It embeds the console (React + TypeScript),
serves the local API, persists jobs and state in SQLite, and drives
Docker through its structured API with an allowlisted lifecycle. There is
no daemon zoo and no package manager: one process, one database file, one
container per active model.

Only one model is active at a time. Starting another installed model is a
transactional switch: stop the previous runtime, start and verify the
target with health and inference checks, and roll back to the previous
model if the target fails. A benchmark job then measures real throughput
on the device and records it. Lifecycle jobs open a persistent deployment
detail modal with named phases and typed step receipts.

The manager defaults to loopback until an operator deliberately chooses a
LAN or Tailscale bind address (the installer asks). Model containers
always bind loopback: the manager's authenticated `/v1` endpoint, with
console-managed API keys, is the only network path to inference, so the
base URL never changes when models switch (see
[`docs/decisions/0007`](docs/decisions/0007-stable-endpoint-api-keys.md)).
An authenticated, redacted diagnostic bundle is available from the
console. The security posture of setup and discovery is documented in
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
  decoding, parsers).

Preflight evaluates pinned artifact bytes, image size, disk safety
margin, the runtime's planned unified-memory allocation, KV and context
settings, and reserved host memory. Memory is rechecked immediately
before start; disk headroom is rechecked throughout long transfers
([`docs/decisions/0004`](docs/decisions/0004-per-node-resource-guardrails.md)).
The signed recipe feed design is in
[`docs/decisions/0009`](docs/decisions/0009-signed-recipe-feed-design.md).

## Runtimes

Today every recipe runs on vLLM in a digest-pinned container. The
accepted direction is runtime independence: recipes will declare a
runtime kind (vLLM, SGLang, llama.cpp) and the manager adapts command,
memory model, health, and metrics per kind, while every trust guarantee
stays identical. Rationale, evidence, and phasing live in
[`docs/decisions/0011`](docs/decisions/0011-multi-runtime-support.md).
The short version: on this hardware, Inkling-Small is served by SGLang,
single-Spark DeepSeek V4 Flash lives in the GGUF world, and the Qwen
family is vLLM. No single runtime covers the frontier anymore.

## Multi-Spark

Two Sparks are paired by hand, in the console. The installer can set both
machines up in one run, but it never exchanges credentials for you: you
create an API key on the second Spark's Connect tab, then add that Spark on
the first one under Fleet, with its console URL and that key. Pairing codes
and mutual authentication are specified in
[`docs/decisions/0005`](docs/decisions/0005-automatic-discovery-and-placement.md)
and are not built, so the manual step is the whole story today.

Once paired, the Fleet tab shows every Spark you have added and what each
one is serving. A recipe that declares two Sparks (`spark_count`) plans
across exactly two nodes: this manager as the head and the single added
peer as the worker. Each node runs its own preflight and has to pass on its
own; the head refuses to start a distributed model when no peer is
configured, when more than one is, or when the recipe does not describe the
interconnect. That is the whole supported topology: a head and one worker.
No two-Spark recipe has completed the qualification protocol on real
hardware yet, so the path is built but unproven, like every other candidate
in the pack.

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
Docker, or exercise GB10 kernels. A passing local suite and registry
metadata checks are not proof of the real-DGX acceptance criteria in
`PRD.md`. For real-hardware acceptance, follow
[`docs/DGX-QUALIFICATION.md`](docs/DGX-QUALIFICATION.md); the included
`packaging/qualify-dgx.sh` helper captures preflight, installation, real
inference, stop/start, smoke-test, and diagnostic receipts without
printing the local pairing credential.

Layout: `cmd/` entrypoints; `internal/` manager packages (recipe,
operations, httpapi, engine, store, discovery, setup); `webui/ui` console
source; `internal/webui/assets` the embedded production build;
`internal/recipe/recipes/` the shipped recipe pack; `docs/decisions/`
numbered ADRs; `docs/runbooks/` operational runbooks. Start with ADRs
0003 (transactional single active model), 0004 (guardrails), 0007
(stable endpoint), 0009 (signed feed), 0010 (setup security), 0011
(runtimes), 0012 (curated model trust, proposed).

## Releases

There are two programs. `cmd/basement` is the manager: it runs on the
Spark, and it is what the `curl | sh` bootstrap and `basement setup
--binary` download. `cmd/basement-setup` is the installer: it runs on the
operator's laptop, opens the wizard as a loopback-only browser page, and
only ever installs a Spark over SSH. It is built for macOS and Windows and
never for Linux, because a Spark installs itself through the manager.

Pushing a `v*` tag runs `.github/workflows/release.yml` on Linux, which
publishes, each with a `.sha256` beside it:

- `basement-{linux,darwin,windows}-{amd64,arm64}`, the manager;
- `basement-setup-darwin-{amd64,arm64}`, the two macOS installer slices;
- `basement-setup-windows-{amd64,arm64}.exe`, the Windows installer, linked
  with `-H=windowsgui` so double-clicking it opens no console window;
- `setup.sh`, `setup.ps1`, `packaging.tar.gz`.

Then, on the Mac that holds the Developer ID identity and the notarytool
keychain profile:

```sh
packaging/sign-macos-release.sh v0.9.3
```

That signs and notarizes the two darwin manager binaries in place, then
calls `packaging/build-macos-installer.sh`, which lipos the two installer
slices into one universal binary, wraps it in `Basement Setup.app`, and
signs, notarizes, staples and verifies `basement-setup-macos.dmg` before
uploading it. Verification is fatal: a disk image that cannot prove it
carries a stapled ticket is never uploaded.

The lipo happens there rather than in CI because the Mac step is required
anyway. The signing identity and the notarization credentials exist on one
laptop and nowhere else, so adding a macOS CI runner purely to run `lipo`
would remove nothing from the local script. The two darwin slices stay
published as honest build inputs; the single user-facing macOS download is
the disk image.

The Windows executables are unsigned. There is no Windows code signing
certificate, so SmartScreen warns on first run, and the install section
above documents the click-through instead of hiding it.

To rehearse the macOS packaging without cutting a release:

```sh
mkdir -p /tmp/slices
for arch in arm64 amd64; do
  GOOS=darwin GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -o /tmp/slices/basement-setup-darwin-$arch ./cmd/basement-setup
done
REHEARSE=1 SETUP_SLICE_DIR=/tmp/slices packaging/build-macos-installer.sh v0.0.0
```

That assembles and signs the bundle and stops: nothing is notarized,
stapled or uploaded, and the output is named `-REHEARSAL` so it cannot be
mistaken for a release artifact.

The app icon is `packaging/macos/basement.icns`, built from
`packaging/macos/icon.svg`, which is the product favicon copied verbatim
from the website. `packaging/macos/make-icon.sh` regenerates it; the
release scripts only copy the committed `.icns`, so cutting a release needs
no SVG renderer.
