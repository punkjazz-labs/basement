# RunOnSpark Manager

RunOnSpark Manager is a local-first DGX Spark service that installs and manages curated vLLM recipes. The current implementation is the first Qwen 3.6 35B-A3B vertical slice: a single Go binary serves the API and embedded console, persists jobs in SQLite, validates a pinned recipe, and executes an allowlisted lifecycle through Docker's structured API.

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

Local tests use deterministic fakes and do not pull the 23.46 GB model or mutate Docker. A passing local suite is not proof of the real-DGX acceptance criteria in `PRD.md`.
