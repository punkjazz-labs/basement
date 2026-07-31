# RunOnSpark Manager

RunOnSpark Manager is a local-first DGX Spark service that installs and manages curated vLLM recipes. One Go binary serves the API and embedded console, persists jobs in SQLite, validates immutable recipes, and executes an allowlisted lifecycle through Docker's structured API.

The embedded candidate pack contains:

- Unsloth Qwen 3.6 35B-A3B using MiaAI Lab's GB10 B12X runtime;
- NVIDIA Qwen 3.6 27B using MiaAI Lab's vLLM launch profile;
- poolside Laguna S 2.1 with its separately pinned DFlash drafter.

All three remain candidates until their complete install, inference, restart, and removal lifecycles pass on real DGX Spark hardware. A candidate label is not a claim of device verification.

Only one model is active at a time. Starting another installed model performs a transactional switch: the manager stops the previous runtime, verifies the target with health and inference checks, and attempts to restore and re-verify the previous model if the target fails. An authenticated redacted diagnostic bundle is available from the console.

Every recipe also carries enforced per-node resource guardrails. Preflight accounts for pinned artifacts, container image size, disk safety margin, vLLM's planned unified-memory allocation, KV/context settings, and reserved host memory. Memory is rechecked immediately before start, while disk headroom is rechecked throughout long transfers. The same evaluator requires every future multi-Spark node to pass independently; multi-Spark execution itself remains out of scope for the current release.

The console derives hardware context from manager inventory instead of asking for a Spark count. The current release detects the local managed node. Secure paired-node discovery, independent placement, and distributed placement are specified in [`docs/decisions/0005-automatic-discovery-and-placement.md`](docs/decisions/0005-automatic-discovery-and-placement.md) but are not yet implemented. Lifecycle jobs open a persistent deployment detail modal with named phases and typed step receipts.

## Development

The manager defaults to loopback until an operator deliberately chooses a LAN bind address.

```bash
go run ./cmd/runonspark-manager --data-dir ./var --listen 127.0.0.1:7070
```

On first launch, read `./var/pairing-token` and enter it in the browser. Production installation should place the data directory under `/var/lib/runonspark-manager` and bind only to an explicitly selected LAN or Tailscale interface.

Run the local verification suite with:

```bash
go test ./...
go vet ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/runonspark-manager
```

Local tests use deterministic fakes and do not pull model weights, mutate Docker, or exercise GB10 kernels. A passing local suite and registry metadata checks are not proof of the real-DGX acceptance criteria in `PRD.md`.

For real-hardware acceptance, follow [`docs/DGX-QUALIFICATION.md`](docs/DGX-QUALIFICATION.md). The included `packaging/qualify-dgx.sh` helper captures preflight, installation, real inference, stop/start, smoke-test, and diagnostic receipts without printing the local pairing credential.
