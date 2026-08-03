# basement

Install and manage local AI models on your DGX Spark with one click.

basement turns a GB10 machine (NVIDIA DGX Spark, ASUS Ascent GX10, MSI
EdgeXpert) into a private AI server you can actually trust. It finds your
machine, installs a curated model, verifies that everything it downloaded
is exactly what it should be, and gives you a console and an
OpenAI-compatible endpoint. Your prompts never leave your network.

## Install

From any macOS or Linux machine on the same network as your Spark, or on
the Spark itself:

```bash
curl -fsSL https://github.com/punkjazz-labs/basement/releases/latest/download/setup.sh | sh
```

On Windows (PowerShell):

```powershell
irm https://github.com/punkjazz-labs/basement/releases/latest/download/setup.ps1 | iex
```

The installer discovers GB10 machines on your network, lets you pick one,
installs over SSH with your existing credentials (nothing is stored), and
opens the paired console in your browser. Run it on the machine itself and
it installs locally instead.

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

The current candidate pack:

- Unsloth Qwen 3.6 35B-A3B, the recommended all-rounder;
- NVIDIA Qwen 3.6 27B, flagship-level coding in a smaller footprint;
- poolside Laguna S 2.1 with its DFlash drafter, built for long agent runs.

Candidates are recipes whose complete install, inference, restart, and
removal lifecycles have not yet all passed on real hardware. The label is
honest: candidate means candidate, not verified.

The pipeline behind the pack is documented in
[`docs/MODEL-CANDIDATES-2026-08.md`](docs/MODEL-CANDIDATES-2026-08.md):
which models the community actually runs on this hardware, verified
against primary sources before anything enters the catalog.

## What we do

Five steps, always in this order.

1. **Research.** We sweep what the community actually runs on GB10
   hardware: X, NVIDIA forums, Hugging Face. Every artifact is checked
   against primary sources: repository, revision, bytes, licence. The
   current sweep is
   [`docs/MODEL-CANDIDATES-2026-08.md`](docs/MODEL-CANDIDATES-2026-08.md).
2. **Validate.** We install and run each recipe on our own Sparks. Real
   hardware, real inference, measured speeds, not a vendor claim. The
   protocol is [`docs/DGX-QUALIFICATION.md`](docs/DGX-QUALIFICATION.md).
3. **Promote.** A recipe that passes qualification moves from candidate
   to verified. The label in the console always tells the truth: right
   now every recipe in the pack is still a candidate, because none has
   completed the full lifecycle on real hardware yet.
4. **Package.** The recipe pins everything: weights by revision and byte
   count, runtime by image digest, the licence you are accepting, and
   the resources the model needs.
5. **Ship.** Recipes reach you through a signed feed and install with
   one click. That is the standard we are building toward: the feed
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

The resource evaluator already requires every node of a multi-Spark
recipe to pass independently, and recipes declare their topology
(`spark_count`). Distributed execution itself is not in the current
release: secure paired-node discovery and placement are specified in
[`docs/decisions/0005`](docs/decisions/0005-automatic-discovery-and-placement.md)
and are the gate for two-Spark flagships such as DeepSeek V4 Flash NVFP4
and Inkling-Small. The console derives hardware context from manager
inventory; the current release detects the local managed node.

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
