# basement

basement is a local-first service for GB10 machines (NVIDIA DGX Spark and OEM equivalents such as the ASUS Ascent GX10 and MSI EdgeXpert) that installs and manages curated vLLM recipes. One Go binary serves the API and embedded console, persists jobs in SQLite, validates immutable recipes, and executes an allowlisted lifecycle through Docker's structured API.

## Install

From any macOS or Linux machine on the same network as your GB10 machine (or on the machine itself):

```bash
curl -fsSL https://github.com/punkjazz-labs/runonspark-manager/releases/latest/download/setup.sh | sh
```

On Windows (PowerShell):

```powershell
irm https://github.com/punkjazz-labs/runonspark-manager/releases/latest/download/setup.ps1 | iex
```

The installer discovers GB10 machines on your network, lets you pick one, installs the manager over SSH using your existing credentials (nothing is stored), and opens the paired console in your browser. Run it on the GB10 machine itself and it installs locally instead. See [`docs/decisions/0010-network-setup-and-gb10-discovery.md`](docs/decisions/0010-network-setup-and-gb10-discovery.md) for the security posture.

The embedded candidate pack contains:

- Unsloth Qwen 3.6 35B-A3B using MiaAI Lab's GB10 B12X runtime;
- NVIDIA Qwen 3.6 27B using MiaAI Lab's vLLM launch profile;
- poolside Laguna S 2.1 with its separately pinned DFlash drafter.

All three remain candidates until their complete install, inference, restart, and removal lifecycles pass on real DGX Spark hardware. A candidate label is not a claim of device verification.

Only one model is active at a time. Starting another installed model performs a transactional switch: the manager stops the previous runtime, verifies the target with health and inference checks, and attempts to restore and re-verify the previous model if the target fails. An authenticated redacted diagnostic bundle is available from the console.

Every recipe also carries enforced per-node resource guardrails. Preflight accounts for pinned artifacts, container image size, disk safety margin, vLLM's planned unified-memory allocation, KV/context settings, and reserved host memory. Memory is rechecked immediately before start, while disk headroom is rechecked throughout long transfers. The same evaluator requires every future multi-Spark node to pass independently; multi-Spark execution itself remains out of scope for the current release.

The console derives hardware context from manager inventory instead of asking for a Spark count. The current release detects the local managed node. Secure paired-node discovery, independent placement, and distributed placement are specified in [`docs/decisions/0005-automatic-discovery-and-placement.md`](docs/decisions/0005-automatic-discovery-and-placement.md) but are not yet implemented. Lifecycle jobs open a persistent deployment detail modal with named phases and typed step receipts.

## Development

The manager defaults to loopback until an operator deliberately chooses a LAN or Tailscale bind address (`install.sh` asks). Model containers always bind loopback: the manager's own authenticated `/v1` endpoint — with console-managed API keys — is the only network path to inference, so the base URL never changes when models switch (see `docs/decisions/0007`).

The console (React + TypeScript, embedded in the binary) includes a streaming playground, integration snippets, API key management, live vLLM telemetry, a storage view, and per-model speed measured on the actual device by an automatic benchmark job.

```bash
go run ./cmd/basement --data-dir ./var --listen 127.0.0.1:7070
```

On first launch, `install.sh` prints a pairing card (URL, token, QR); re-print it anytime with `basement pairing-url`. Production installation places the data directory under `/var/lib/basement`.

Run the local verification suite with:

```bash
go test ./...
go vet ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/basement
```

After changing the console, rebuild the embedded assets (CI fails if they drift from source):

```bash
cd webui/ui && npm ci && npm run build
```

Local tests use deterministic fakes and do not pull model weights, mutate Docker, or exercise GB10 kernels. A passing local suite and registry metadata checks are not proof of the real-DGX acceptance criteria in `PRD.md`.

For real-hardware acceptance, follow [`docs/DGX-QUALIFICATION.md`](docs/DGX-QUALIFICATION.md). The included `packaging/qualify-dgx.sh` helper captures preflight, installation, real inference, stop/start, smoke-test, and diagnostic receipts without printing the local pairing credential.
