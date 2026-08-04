# Building basement

The mechanics behind [`AGENTS.md`](../AGENTS.md): how the tree fits together,
how to build and test it without lying to yourself, and what a release does to a
real machine. Product rules live in
[`docs/plans/00-conventions.md`](plans/00-conventions.md) and are not repeated
here.

## What you need

Go 1.25 and Node 22, the versions CI pins. Nothing else: `CGO_ENABLED=0`
everywhere, so cross-compiling to `linux/arm64` needs no target toolchain, and
Docker is never required to run the tests.

```bash
go build ./... && go vet ./... && go test ./...
cd webui/ui && npm ci && npm run build && npm test
```

`npm run build` is `tsc --noEmit && vite build`, so type errors fail it. CI runs
`go test -race ./...`; run the race detector locally before handing back
anything touching the engine, the store, or a poller. CI does **not** run
`npm test`, so the console unit tests are yours to run. It does run
`shellcheck packaging/*.sh`, so packaging changes have to be clean under it.

Run the manager against a scratch data directory:

```bash
go run ./cmd/basement --data-dir ./var --listen 127.0.0.1:7070
```

`./var` is gitignored. For console work, `cd webui/ui && npm run dev` proxies
`/api` and `/v1` to `127.0.0.1:7070`, so the Vite dev server talks to a real
manager.

## How a change moves through the layers

The console calls `/api/v1/...`. `internal/httpapi` authenticates (console
session, API key, or fleet node key, per route) and writes a job row through
`internal/store`. `internal/engine` reads the recipe, plans the job into an
ordered list of operations, and runs them through `internal/operations`, which
is the only package allowed to touch Docker, the filesystem, or the network.
Operations run one at a time except for the matching head and worker artifact
downloads of a two-Spark install; those two transfers overlap while their step
rows and receipts remain separate. Receipts go back into the store; the console
follows along on `GET /api/v1/jobs/{id}/events` (SSE).

Two locks shape the engine: a mutex per recipe id, and a one-slot semaphore
around every operation in `runtimeOperations` (`internal/engine/engine.go`) —
the ones that touch containers, live memory, or the single active slot. That
semaphore is how ADR 0003's "one model active at a time" survives concurrent
jobs. Downloads and image pulls deliberately sit outside it so an install can
proceed while another model is serving.

Inference does not go through any of that. `/v1/*` is a streaming authenticated
proxy from the manager to the active model's loopback port (ADR 0007). Model
containers never publish on a routable address.

Adding a feature usually means touching, in this order: the recipe schema (only
if the feature is declared per model), the operation that does the work, the
engine plan that places it, the API handler, the console. Working the other way
round produces UI that promises something the engine cannot deliver.

## The console asset contract

`webui/ui` is the source. Vite is configured (`webui/ui/vite.config.ts`) to
build *into* `internal/webui/assets`, with `emptyOutDir: true`. That directory
is committed and embedded:

```go
//go:embed assets/*
var Assets embed.FS
```

CI checks out the commit, rebuilds the console from committed source, and runs

```
git diff --exit-code internal/webui/assets
```

in a step named **"Committed console assets must match source"**. So the rule is
not "rebuild the assets", it is **the committed assets must equal a build of the
committed source, and nothing else**. A build run in a working tree that carries
any other UI change bakes that change in and fails the check on the next commit.

Bundle filenames are content-hashed (`static/index-<hash>.js`), so a rebuild
renames files. That is expected; `emptyOutDir` removes the old ones. `git diff`
only sees tracked paths, so `git add` the whole directory: the new hashed files
are untracked until you do, and only the deletions would show.

The release workflow never rebuilds the console. A tagged release ships exactly
the bytes committed in `internal/webui/assets`, which is the other reason drift
is treated as a failure rather than a nuisance.

### Rebuilding safely when the tree is dirty

Commit the source first, build from a clean checkout of that commit, then commit
the result. This is the sequence the repository's own asset commits used.

```bash
# 1. commit the console source change alone
git add webui/ui/src && git commit

# 2. build it in a throwaway checkout of exactly that commit
git worktree add /tmp/basement-assets HEAD
(cd /tmp/basement-assets/webui/ui && npm ci && npm run build)

# 3. take the result, drop the checkout
rm -rf internal/webui/assets
cp -R /tmp/basement-assets/internal/webui/assets internal/webui/assets
git worktree remove --force /tmp/basement-assets

# 4. commit the assets
git add internal/webui/assets && git commit
```

Two commits, source then assets, is normal here and keeps unrelated in-flight
work out of the bundle. If your working tree is clean apart from the console
change you just committed, `cd webui/ui && npm run build` in place is
equivalent; the worktree dance only buys anything when something else is
uncommitted. `npm ci` needs network access.

## Tests

`go build ./... && go vet ./... && go test ./...` must pass, on a laptop and on
a Spark, with identical results. **Never pipe `go test` into `grep`, `head`,
`tail`, or a formatter that drops its exit status.** A filtered run that prints
nothing looks exactly like a passing run.

There is no test-helper package, no build tags for hardware, and no short-mode
gating. One `t.Skip` exists in the whole tree. Everything is bought instead with
**seams**: package-level function variables that hold the real implementation,
which tests reassign and restore. The invariant is stated in the code —
*"Production never reassigns these"* (`internal/httpapi/fleet.go`).

```go
// internal/operations/fabric.go
var sysfsRoot = "/sys"                      // tests point it at a fixture
var fabricLink = detectFabricLink           // the detector, injectable for tests
var fabricAddress = FabricAddress           // tests fake sysfs but cannot fake netlink
var ServeFabricProbe = serveFabricProbe     // exported only so httpapi can test the node endpoint
```

The override idiom is always the same: save, `t.Cleanup` to restore, then
assign. `withFabric` in `internal/operations/fabric_test.go` and `holdSeams` in
`internal/httpapi/fleet_test.go` are the two to copy.

```go
func withFabric(t *testing.T, link FabricLink, detected error, address string, addressErr error) {
	t.Helper()
	previousLink, previousAddress := fabricLink, fabricAddress
	t.Cleanup(func() { fabricLink, fabricAddress = previousLink, previousAddress })
	fabricLink = func() (FabricLink, error) { return link, detected }
	fabricAddress = func(string) (string, error) { return address, addressErr }
}
```

**Stub anything that reads the machine.** This is not hypothetical: CI runners
can hold a real RDMA device (an accelerated-networking Mellanox VF has a link
and an address), so unstubbed fabric detection succeeds on the runner and
overrides whatever the test meant to assert. The real-system readers are:

| What it reads | Where | Seam |
| --- | --- | --- |
| `/sys/class/infiniband`, `.../net/<dev>/carrier` | `internal/operations/fabric.go` | `sysfsRoot`, `fabricLink` |
| `net.InterfaceByName().Addrs()` (netlink) | `internal/operations/fabric.go`, `fleet.go` | `interfaceHasIPv4`, `fabricAddress` |
| binds a listener on the fabric address | `internal/operations/fabric.go` | `ServeFabricProbe` |
| `net.Interfaces()` / `net.InterfaceAddrs()` | `internal/discovery/`, `internal/httpapi/fleet.go` | `localIPs`, `discoverCandidates`, `selfAddresses` |
| SSH dial and remote probe | `internal/setup/` | `connectTarget`, `adoptDial`, `adoptProbe`, `adoptInstall` |
| `nvidia-smi`, `/proc/meminfo`, `/proc/device-tree/model` | `internal/inventory/inventory.go` | the `inventory.Provider` interface |

Two seam shapes exist beside the function variable, and both are already in use:
an **interface plus a hand-written fake** where the thing has a shape
(`inventory.Provider`, `operations.Executor`, `setup.Runner` — see
`readyInventory` in `internal/httpapi/server_test.go`, `resourceInventory` in
`internal/operations/host_test.go`), and an **injected `http.RoundTripper` or
`httptest.Server`** for anything spoken over HTTP. Docker is never shelled out
to; it is reached over its socket API, so `DockerClient` takes a fake transport
(`withoutNegotiation` in `internal/operations/docker_test.go`).

If you add code that reads the host, add the seam in the same change. A test
that passes because of the machine it ran on is not a test.

## Recipes

A recipe is one YAML file in `internal/recipe/recipes/`, embedded with
`//go:embed recipes/*.yaml` and loaded by `recipe.Builtin()` at startup. Loading
is strict: unknown YAML keys are a hard error (`KnownFields(true)`), every
recipe is validated, and a single invalid file is a fatal boot failure. The
embedded set is the permanent offline floor; the signed remote feed (ADR 0009)
layers on top of it and drops bad entries instead of failing.

The schema is `internal/recipe/types.go`; the rules are
`internal/recipe/validator.go`. A recipe declares:

- **identity and trust** — `schema_version` (must be 1), `id`, `version`,
  `display_name`, `publisher`, `trust`, `verification`. `basement-verified`
  requires `dgx-spark-verified`; the validator rejects the mismatch with
  `verified trust requires real DGX verification`.
- **attribution** — `model_by`, `recipe_by`, `model_released`. Optional, not
  validated, and deliberately so: the console shows `n/a` rather than guess.
- **source** — an HTTPS `url` on `github.com` or `huggingface.co` plus a
  40-character `revision`.
- **topology** — `spark_count` 1 or 2. Two Sparks must declare an
  `interconnect` (kind `connectx7`, a master port, and NCCL/Gloo/TP environment
  from a fixed allowlist); one Spark must not.
- **runtime** — `kind` (`vllm`, `sglang`, `llamacpp` or `comfyui`), `image`
  without a tag, `digest`
  pinned to `sha256:<64 hex>`, `image_bytes`, `image_disk_bytes`, `shm_bytes`,
  and an environment restricted to an allowlist of name/value pairs. Optional
  `writable_paths` names absolute container paths a runtime must be able to
  write before it can serve (a JIT kernel cache, typically); each becomes its
  own bounded tmpfs, writable and nosuid but not noexec, because such a cache
  holds shared objects the runtime loads. The manager owns the size, and the
  validator refuses a path that is the container root, the scratch `/tmp`, or
  at or inside any mount the container already has.
- **artifacts** — one or more, exactly one `role: primary`. Each carries
  `repository`, a 40-hex `revision`, `expected_bytes`, `licence` and a
  `licence_url` that must point at that artifact's own Hugging Face repository.
  A speculative drafter is a second artifact with its own role.
- **requirements** — `architecture: aarch64`, the Docker and NVIDIA runtime
  flags, `per_node_minimum_memory_bytes`, `per_node_memory_reserve_bytes`,
  `safety_margin_bytes`, `secrets` (only `HF_TOKEN` is allowlisted),
  `required_licence_acceptance`.
- **service** — `internal_port` (8000 for the text kinds, 8188 for `comfyui`,
  which is ComfyUI's own documented port), `served_model_id` (must identify the
  pinned primary artifact), and exactly one of a `vllm`, `sglang`, `llamacpp` or
  `comfyui` block matching `runtime.kind`. A `comfyui` block pins the model's
  canvas, its frame grid and the workflow graphs it generates through; those
  graphs are files under `internal/recipe/graphs/`, embedded with the recipes,
  named by a recipe and never supplied by one. The validator checks at load
  time that every named graph exists, parses, and carries exactly the
  substitution tokens its mode needs, so a graph can never silently ignore the
  user's prompt.
- **operations** and **uninstall** — the step lists, from a closed set of 20
  names. There is no per-step payload: an operation is just a `type`. The order
  is pinned; the validator requires the complete install lifecycle and the
  complete uninstall lifecycle, in sequence, ending in the verification that
  fits the kind: `verify_openai_inference` for a text model,
  `verify_media_generation` for `comfyui`, which generates the smallest clip
  the recipe allows and requires a non-empty file on disk. `run_shell` is rejected by name in
  addition to being absent from the allowlist.
- **memory_model** — optional for the kinds that claim a share of the device,
  required for `llamacpp` and `comfyui`, which claim an absolute number of
  bytes instead and have nothing else to bound them with. Absent means "no
  estimate yet", not "no footprint", and the console reports unknown. Two
  shipped recipes explain in a comment why theirs is absent rather than
  guessing a number.

Executors for those step names live in `internal/operations/host.go`
(`HostExecutor.Execute`), with a mirror `Completed` switch that makes each step
idempotent so an interrupted job can resume. A few step names are synthesized by
the engine and cannot appear in a recipe: `measure_throughput`, `verify_fabric`,
`verify_peer_node`, `teardown_stop_container`.

**Honesty rules for recipe copy.** Every user-visible claim traces to data:

- Attribution comes from `model_by` / `recipe_by` / `model_released`, falling
  back to `publisher` for the byline and to `n/a` for everything else. Never
  fill one in from knowledge, a model card summary, or inference.
- Quantization lines state the format only. For vLLM recipes the label is
  *derived from the artifact repository name* (`readableWeights` in
  `webui/ui/src/catalog.ts`, against a fixed set: NVFP4, FP8, INT8, AWQ, GGUF
  and friends), never authored. Who built the quantized weights is deliberately
  not shown.
- Speeds shown before a machine measures its own are community-reported and
  each figure carries a source comment in `webui/ui/src/views/Models.tsx`
  pointing at `docs/MODEL-CANDIDATES-2026-08.md`. A number without a traceable
  source does not ship.
- Every shipped recipe is `trust: basement-candidate` /
  `verification: candidate` and a test asserts it. Promotion to verified
  requires the real-hardware protocol in
  [`docs/DGX-QUALIFICATION.md`](DGX-QUALIFICATION.md), not a passing local
  suite.
- Where a recipe made a judgement call, the file says so in a comment: a
  rejected provenance, a licence file that does not exist at that revision, a
  parser that is an unverified approximation. Keep that habit; a comment
  admitting a gap is worth more than a clean-looking field.

## Releases

Two programs ship. `cmd/basement` is the manager that runs on the Spark;
`cmd/basement-setup` is the installer that runs on an operator's laptop and is
built for macOS and Windows only.

1. **Push to `main` and wait for `ci` to be green.**
   `.github/workflows/release.yml` builds and publishes; it does **not** run
   `go test`, `go vet`, or the asset drift check. `ci.yml` also triggers on
   `v*` tags, but it runs alongside the release job, not before it, and does not
   gate it. A tag on a red main publishes a broken release.
2. **Tag `vX.Y.Z` and push it.** The release job builds on Linux with
   `CGO_ENABLED=0 -trimpath` and `-X main.version=${GITHUB_REF_NAME}`, then
   `gh release create` publishes, each with a `.sha256` beside it:
   `basement-{linux,darwin,windows}-{amd64,arm64}`,
   `basement-setup-darwin-{amd64,arm64}`,
   `basement-setup-windows-{amd64,arm64}.exe` (linked `-H=windowsgui`),
   `setup.sh`, `setup.ps1`, `packaging.tar.gz`. These asset names are a
   contract: `packaging/setup.sh`, `setup.ps1` and `basement setup --binary`
   download them by name.
3. **Mark the release prerelease until the Mac step finishes.** Nothing in the
   repository does this: `gh release create` is called with no `--prerelease`,
   `--draft`, or `--latest`, and there is no `gh release edit` anywhere. So the
   moment CI publishes, GitHub makes that tag `latest` by its own rule (newest
   non-prerelease), while `basement-setup-macos.dmg` does not exist yet and the
   darwin manager binaries are still unsigned — the README's
   `/releases/latest/download/basement-setup-macos.dmg` link 404s until step 4
   runs. Marking it prerelease by hand and promoting it in step 5 is the
   operator's job, not the workflow's.
4. **Sign and notarize on the Mac that holds the Developer ID identity and the
   `notarytool` keychain profile.** They exist on one laptop and nowhere else,
   so this step cannot move into CI.

   The update release key is a dedicated Ed25519 private key stored as one
   base64 value in a macOS Keychain generic-password item. Configure the
   public half in the repository variable `BASEMENT_UPDATE_PUBLIC_KEYS` as
   `key-id=base64-public-key`. Release builds fail when that variable is
   absent, so both the manager and fixed root helper always embed the same
   key ring. Never put the private value in a repository variable, command
   argument, environment value, log, or release asset.

   Choose `UPDATE_KEY_ID` and `UPDATE_KEYCHAIN_SERVICE`, then generate the
   pair and send the private half straight to Keychain. The signer writes the
   matching public ring entry to `/tmp/basement-update-public-key.txt`; it has
   no option that writes the private half to a file. Keep `-w` last so
   `security` reads the generated value from standard input:

   ```sh
   go run ./cmd/sign-update-manifest -mode keygen \
     -key-id "$UPDATE_KEY_ID" \
     -public-key-out /tmp/basement-update-public-key.txt |
     security add-generic-password -U -a "$(id -un)" \
       -s "$UPDATE_KEYCHAIN_SERVICE" -w
   gh variable set BASEMENT_UPDATE_PUBLIC_KEYS < /tmp/basement-update-public-key.txt
   ```

   Set `UPDATE_KEY_ID` to the matching key id and
   `UPDATE_KEYCHAIN_SERVICE` to the chosen Keychain service name. Set
   `UPDATE_KEYCHAIN_ACCOUNT` only when the Keychain item uses a different
   account from the current macOS user. Then name every release that the new
   manager can safely roll back to:

   ```sh
   packaging/sign-macos-release.sh vX.Y.Z vW.Y.Z
   ```

   The Mac step signs the exact Linux update manifest before publication,
   verifies it against the public key embedded by the release build, uploads
   it, downloads it again, and verifies the published bytes. A release is
   left as a draft if any check fails. Rotating this key or the root updater
   requires a manual installer upgrade in updater protocol 1.

   It reads the identity from `SIGN_IDENTITY` (required, no default — the
   script fails with a clear message if it is unset) and the keychain profile
   from `NOTARY_PROFILE` (defaults to `basement`), and needs an
   authenticated `gh`. It downloads the two darwin manager binaries from the
   release, `codesign`s them with a hardened runtime and a timestamp, notarizes
   each as a zip (a bare executable cannot be stapled), and re-uploads the
   signed binaries with fresh checksums. Then it calls
   `packaging/build-macos-installer.sh`,
   which `lipo`s the two installer slices into one universal binary, wraps it in
   `Basement Setup.app` with the committed
   `packaging/macos/basement.icns`, and signs, notarizes, staples and verifies
   `basement-setup-macos.dmg` before uploading it. Verification is fatal: a disk
   image that cannot prove it carries a stapled ticket is never uploaded.

   To rehearse without a release, build the two slices yourself and run
   `REHEARSE=1 SETUP_SLICE_DIR=... packaging/build-macos-installer.sh v0.0.0`;
   it stops after signing the bundle and names the output `-REHEARSAL`.
5. **Promote to latest** (`gh release edit vX.Y.Z --prerelease=false --latest`)
   and then actually fetch the URLs the README hands people:
   `/releases/latest/download/basement-setup-macos.dmg`,
   `/releases/latest/download/basement-setup-windows-amd64.exe`,
   `/releases/latest/download/setup.sh`,
   `/releases/latest/download/setup.ps1`. Nothing in the repository verifies
   these; a release is not done until someone has.

The Windows executables are unsigned — there is no certificate — and SmartScreen
warns on first run. The README documents the click-through rather than hiding
it. Do not "fix" this by removing the warning from the docs.

`scripts/release.sh` is an older local build-everything script that produces
zips rather than the DMG. The tag-driven workflow above plus the two macOS
scripts is the live path.

## What a release changes on a real Spark

Described generically; never put a hostname or address in this repository.

The manager runs as a systemd service defined by
`packaging/systemd/basement.service`: a dedicated `basement` system user in the
`docker` group, `ProtectSystem=strict`, an empty capability set, and one
writable path. Its `ExecStart` is

```
/usr/lib/basement/current/basement --data-dir /var/lib/basement --listen 127.0.0.1:7070
```

`packaging/install.sh` installs the binary in a version slot below
`/usr/lib/basement/versions`, selects it through `/usr/lib/basement/current`,
and keeps `/usr/lib/basement/basement` as a compatibility symlink. It creates
`/var/lib/basement` (mode 0750, owned by the service user), installs the manager
and updater units, and only if the operator chose a network interface, or
`BASEMENT_LISTEN` was pre-seeded, writes a **drop-in** at
`/etc/systemd/system/basement.service.d/listen.conf` that clears `ExecStart` and
sets it again with the chosen address. Loopback is the default; exposure is
always a deliberate choice. That drop-in is the reason you must never assume the
committed unit describes a running machine.

`/var/lib/basement` holds `manager.db` (SQLite: jobs, models, API keys, peers,
token counters), `pairing-token` (the console pairing credential, read by
`basement pairing-url`), `auth-signing-key`, downloaded model artifacts, and
generated container configuration. A console update adds one signed manager
slot, changes the `current` symlink, and restarts only the manager service. The
fixed root updater, its units, and its embedded public key change only when the
installer is run manually. Everything else in the data directory stays.

**Model containers survive a manager restart.** They are Docker containers
owned by the daemon, not children of the manager process, so a restarted manager
finds them still serving. On startup the manager resumes jobs that were
interrupted and reconciles any model recorded as active but `recovering` by
issuing a fresh start job for it. A start job also rebuilds a container whose
baked-in configuration no longer matches the current one, such as a moved data
directory or a fabric master address that changed across a reboot, but only
when a value positively disagrees.

The manager update payload changes one manager slot and the selected symlink.
The manual release package may also change the fixed updater helper and systemd
units. A data layout or container contract change still needs a migration path
and an ADR before it belongs in code.

## Where the rest lives

- [`docs/plans/00-conventions.md`](plans/00-conventions.md) — product rules,
  code style, the report format. Mandatory.
- [`docs/decisions/`](decisions/) — ADRs. 0003 single active model, 0004
  guardrails, 0007 stable endpoint, 0009 signed feed, 0010 setup security, 0011
  runtimes, 0012 curated trust, 0013 delegated placement, 0014 console adoption.
- [`docs/DGX-QUALIFICATION.md`](DGX-QUALIFICATION.md) — how a candidate recipe
  becomes verified, on real hardware.
- [`docs/MODEL-CANDIDATES-2026-08.md`](MODEL-CANDIDATES-2026-08.md) — the model
  sweep, and the source of every community-reported number in the console.
- `webui/ui/src/styles.css` — the console design system. Its header comment
  defines the colour roles and is the authority when a document disagrees with
  it.
- `PRD.md` — the approved specification baseline.
