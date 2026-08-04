# AGENTS.md — read before acting in this repo

This is the canonical entry point for any contributor, human or agent, in any
harness. It is short on purpose: it states the contracts that are not visible
from the code, and links to the file that owns each one.

`basement` is one Go binary that installs and runs curated local AI models on a
GB10 machine (NVIDIA DGX Spark, ASUS Ascent GX10, MSI EdgeXpert). It embeds a
React console, serves an OpenAI-compatible endpoint, and drives Docker. The
product promise is trust: pinned versions, verified installs, honest copy,
receipts for everything. `README.md` is the user-facing description; `PRD.md` is
the approved specification baseline.

Read in this order: this file, then `docs/plans/00-conventions.md` (mandatory,
it encodes product promises), then `docs/BUILDING.md` for the build, test and
release mechanics, then the ADRs in `docs/decisions/` for whichever subsystem
you are touching.

## Session rules

1. Read `~/.config/punkjazz/project-defaults.md` before project work. It is the
   canonical local source for model routing, email identity, and machine
   addresses. If absent on a managed laptop, fetch the canonical
   project-defaults file through the `kb` helper before choosing
   infrastructure.
2. Read `docs/PROJECT-AUTONOMY.md` first. Its gates and `protected_paths` bind
   every agent, whatever harness you run in. The commit hooks enforce them.
3. Unattended sessions: an open Flight Recorder exists at `.hermes-fr.yaml`
   (opened by hermes-session). Before your first action, fill `does_not_count`
   from the task prompt with near-miss outcomes that do not qualify as
   completion — verification fails if it stays empty. Append actions with
   receipts; commit messages carry the FR id.
4. Human-present, human-typed sessions (no agent CLI): commits use the
   `manual:` prefix and judgments go to the cognition inbox via `kb-log`.
5. Never modify verification (linters, tests config, audit scripts, hooks, CI)
   in the same task whose output it checks. Park the unit and escalate instead.
6. Branch work only. Merging to the default branch needs an explicit
   in-session grant.
7. Corrections and dead ends from this session go to the cognition inbox
   (`kb-log`) so the shared brain compounds.

## The map

| Path | What lives there |
| --- | --- |
| `cmd/basement/` | the manager: runs on the Spark, serves the console and `/v1` |
| `cmd/basement-setup/` | the installer: runs on the operator's laptop, installs a Spark over SSH |
| `cmd/sign-index/` | signs a recipe index (ADR 0009); key path passed in, never key material |
| `internal/config/` | flag parsing: `--listen`, `--data-dir`, subcommands |
| `internal/httpapi/` | REST API under `/api/v1/`, SSE job events, the `/v1` inference proxy, and the embedded console handler |
| `internal/engine/` | job engine: planning, execution, rollback, per-recipe locks, one-slot runtime semaphore |
| `internal/operations/` | step executors (`verify_*`, `pull_image`, `download_artifact`, container lifecycle, fabric checks) |
| `internal/recipe/` | recipe schema (`types.go`), validation, and the embedded recipes in `recipes/*.yaml` |
| `internal/store/` | SQLite persistence (jobs, models, keys, peers) |
| `internal/auth/` | pairing token, session signing key, console session auth (API keys live in the store) |
| `internal/inventory/` | host facts (GPU, memory, disk, Docker) |
| `internal/setup/`, `internal/setupweb/` | the installer's SSH flow and its loopback wizard page |
| `internal/webui/` | `go:embed assets/*` — the committed console build |
| `webui/ui/` | React 19 + TypeScript + Vite console source |
| `packaging/` | systemd unit, `install.sh`, macOS installer and signing scripts |
| `docs/decisions/` | ADRs, the design record |
| `docs/plans/` | executor specs; `00-conventions.md` is mandatory reading |

A request flows one way. The console calls `/api/v1/...`; `internal/httpapi`
authenticates it and creates a job in `internal/store`; `internal/engine` plans
that job into an ordered list of operations from the recipe and runs them
through `internal/operations`, which is the only layer that touches Docker, the
filesystem, or the network; the console follows progress on
`GET /api/v1/jobs/{id}/events` (SSE) and step receipts land back in the store.
Inference never follows that path: `/v1/*` is a streaming authenticated proxy
straight to the active model's loopback port.

Two decisions constrain almost every change to that flow:

- **ADR 0003** (`docs/decisions/0003-transactional-single-active-model.md`) —
  one model is active at a time, and starting another is a transactional switch
  with verification and rollback, not an independent start.
- **ADR 0007** (`docs/decisions/0007-stable-endpoint-api-keys.md`) — the
  manager owns `/v1`, model containers publish on `127.0.0.1` only, and the
  base URL never changes when models switch.

Read the ADR before changing behavior it describes. If you change that
behavior, the ADR changes with it in the same commit.

## Contracts that are not visible from the code

**The console is committed build output.** `webui/ui` is built by Vite *into*
`internal/webui/assets`, which is committed and embedded with `go:embed`. CI
rebuilds from committed source and runs `git diff --exit-code
internal/webui/assets` in a step named "Committed console assets must match
source". So the assets must be built from committed source only, never from a
working tree that carries anything else. `docs/BUILDING.md` has the clean-worktree
procedure; use it every time.

**Tests must not read the machine they run on.** CI runners can hold real RDMA
hardware, so anything that probes fabric links, addresses, Docker, or host
inventory must be stubbed. See `docs/BUILDING.md` for the seam idiom this repo
uses and where the seams are.

**Never hide a test exit code.** `go build ./... && go vet ./... && go test
./...` must pass. Do not pipe `go test` into `grep`, `head`, `tail`, or anything
else that swallows its status; read the whole output.

**Everything is pinned.** Container images by `sha256` digest, artifacts by
revision and exact byte count, source repos by commit. A floating tag or
`latest` in a recipe is a defect, not a shortcut.

**No invented facts, anywhere users can see them.** Attribution, licences,
dates and speeds come from recipe data or from a source recorded in
`docs/MODEL-CANDIDATES-2026-08.md`; absent values are `n/a`. Quantization lines
state the format only (NVFP4, FP8, BF16), never who built the quantized
weights. If a change needs a fact you do not have, leave `n/a` and say so in
your report.

**Product copy: no emoji, no em dashes.** Plain language, benefit first,
sentence case. Controls say exactly what happens. Errors say what went wrong and
what to do next, without apologies. Numbers are honest, and an estimate says it
is one.

**UI work is mockup-first.** Every new visual concept (not incremental edits)
needs a static mockup approved by the owner before implementation. The design
system is `webui/ui/src/styles.css`; its comment header defines the colour roles
and is the authority when any doc disagrees with it.

**This repository may become public.** Never write a machine address, hostname,
token, API key, personal network detail, or credential into any file here,
including docs and commit messages. Secrets are passed by path or by stdin, and
diagnostics go through `internal/redact`.

**Releases are not gated by tests.** The release workflow builds and publishes
on a `v*` tag and runs no tests and no drift check, so `main` must already be
green before anyone tags. The macOS disk image is signed and notarized
afterwards, by hand, on the one Mac that holds the identity.

**Commits.** Subject line is `manual: ` plus a plain-language sentence about
what a user gets, lowercase, no trailing period: `manual: an unreachable Spark
says so, instead of Failed to fetch`. Body explains why, wrapped at about 72
columns. Agents add a `Co-Authored-By:` trailer identifying themselves. The
`manual:` prefix is enforced by the commit-msg hook, which also blocks commits
touching `protected_paths`.

## Before you hand work back

```bash
cd webui/ui && npm ci && npm run build && npm test   # console work
cd ../.. && go build ./... && go vet ./... && go test ./...
```

`npm run build` runs `tsc --noEmit` first, so type errors fail the build. CI does
not run `npm test`, so run it yourself. If the console change is going into a
commit, rebuild the committed assets with the procedure in `docs/BUILDING.md`
rather than shipping whatever an in-place build produced. Then follow the
reporting format at the end of
`docs/plans/00-conventions.md`: what you ran, what it said, screenshots for UI
work, and every deviation with its reason. Unresolved questions go in the
report, not into guessed code.

Deeper mechanics — the asset rebuild procedure, the test seams, the recipe
schema, the release chain, and what a release changes on a real Spark — are in
[`docs/BUILDING.md`](docs/BUILDING.md).
