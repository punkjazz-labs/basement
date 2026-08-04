# Spec 13: One-click manager update

Status: proposed implementation plan. Architecture is ADR 0008. Design only.

This plan supersedes `docs/plans/16-manager-self-update.md` where they differ. In
particular, it uses a separate root updater, signs a manifest rather than only a binary,
requires a manual first upgrade, treats every update surface as mockup-gated, and follows
ADR 0016's decided fleet maintenance protocol.

Every implementation phase must remain shippable on its own. A dormant verifier, schema
contract or installed unit is acceptable. An enabled console action without release
authenticity, root recovery, database compatibility and honest persisted status is not.

## 0. Outcome and limits

The owner gets these outcomes:

1. A standalone update-capable basement installation reports a newer signed stable
   release and can install it from the console.
2. The manager downloads and verifies as user `basement`; a narrow root service installs
   only the verified fixed asset.
3. The selected manager restarts from a version slot. If it does not become healthy, the
   previous slot is restored automatically.
4. Reloading the console during any interruption shows the persisted attempt rather than
   inventing success or failure.
5. A running model container is left alone. The console and authenticated `/v1` proxy
   reconnect around the manager restart.
6. A future ADR 0016 fleet updates one local node at a time in a controller-owned
   maintenance run while placement remains blocked by exact-version agreement.

This spec does not deliver:

- background or scheduled automatic installation;
- a writable `/usr/lib/basement` in the manager service;
- a general root command, sudoers rule, polkit rule or caller-selected unit;
- recipe, model, image, artifact or container update;
- database snapshot rollback;
- uninterrupted in-flight `/v1` requests;
- an updater-helper, updater-unit or signing-key rotation through the normal manager
  update payload;
- one-click enablement on a machine that has only today's flat install;
- fleet update before ADR 0016's identity, maintenance and previous-version status
  protocol exists.

The first update-capable installation is a manual root bootstrap. Subsequent eligible
manager releases are the one-click path.

## 1. Verified current baseline

These are current facts the implementation must preserve or deliberately migrate.

| Concern | Current evidence | Consequence for this plan |
| --- | --- | --- |
| Availability check | `internal/httpapi/server.go:1886-1927` calls the latest-release API, returns version and release metadata, uses string inequality and caches for one hour. | Replace string inequality with strict version ordering and add a separate signed-installable state. Keep a bounded availability cache. |
| Console action | `webui/ui/src/App.tsx:125-135,291-295` polls hourly and renders an external release link. | Replace the anchor with a button and persisted update surface after mockup approval. |
| Manager privilege | `packaging/systemd/basement.service:8-41` runs as `basement`, with no capabilities, `NoNewPrivileges=yes`, `ProtectSystem=strict` and only `/var/lib/basement` writable. Lines 10-21 also put it in the privileged Docker group and state that trust boundary. | Preserve the direct filesystem confinement and do not add service-management power. Do not claim containment from the already trusted Docker daemon. |
| Flat install | `packaging/install.sh:34-37` installs one executable at `/usr/lib/basement/basement`. | The bootstrap installer must adopt that executable into a rollback slot. |
| Listen override | `packaging/install.sh:63-66` can replace `ExecStart` in a drop-in and still names the flat path. `internal/setup/install.go:212-230` does the same in the setup path. | Keep `/usr/lib/basement/basement` as a compatibility symlink and test both installer paths. |
| Linux release trust | `.github/workflows/release.yml:23-35` publishes the Linux binary and SHA-256 file. | Add a signed manifest. The checksum remains informational and backward compatible. |
| Publication order | `.github/workflows/release.yml:80-99` creates a draft; `packaging/sign-macos-release.sh:55-62` publishes it after the Mac pass. | Sign and verify the Linux update manifest before the existing final publish. |
| macOS signing only | `packaging/sign-macos-release.sh:29-47` signs and notarizes Darwin binaries. | Add a separate update-manifest signing step. Do not describe Apple signing as Linux authenticity. |
| Health endpoint | `internal/httpapi/server.go:268-274` returns `status` and manager version at `/healthz`. | The root updater requires this exact route and expected version after restart. |
| Shutdown | `cmd/basement/main.go:152-171` drains HTTP, stops counters and closes the database without stopping containers. | The updater may stop only the manager. |
| Restart recovery | `internal/store/store.go:123-139` marks non-terminal jobs interrupted, active models recovering and active generations interrupted. `internal/engine/engine.go:284-312` resumes jobs and reconciles active models. | Refuse planned updates during jobs and generations, but make unexpected restart status honest. |
| Docker ownership | `internal/operations/docker.go:85-90,348-380` talks to the Docker daemon and creates containers through its API. Model ports are loopback-only at `internal/operations/docker.go:310-318`. | Inference: a running container survives the manager process, but public `/v1` is unavailable while the manager is down. Hardware must verify the claim before user copy ships. |
| Store migration | `internal/store/store.go:145-149` initializes `schema_meta` to `1`; additive migration examples are at `internal/store/store.go:261-345`. | Add real migration and reader compatibility metadata before rollback is enabled. |
| Fleet version rule | `docs/decisions/0016-multi-node-fleet.md:490-524` requires exact release identity for placement and defines controller-last rolling order. | Local update is a fleet maintenance step, not an independent member action. |

No external URL is cited in this plan. External release locations used by production code
must be verified during implementation, but this document makes no reachability claim
about them.

## 2. Architecture contracts

### 2.1 Signed release manifest

The exact bytes of `basement-linux-arm64.update.json` are signed with Ed25519. The
detached `basement-linux-arm64.update.sig` is base64 of the raw signature plus one
newline. The verifier uses only Go's `crypto/ed25519`, `crypto/sha256`, `encoding/base64`
and strict `encoding/json` decoding.

Schema 1 binds:

- manifest schema and updater protocol;
- signing key id;
- exact stable release version;
- `linux` and `arm64`;
- fixed asset name;
- exact byte size and SHA-256 digest;
- exact manager versions allowed as rollback readers.

The manifest is verified before its fields are trusted. A known key id selects one key
from the embedded release-key ring. The release key is not the recipe key. The manager
and root updater embed the same public key ring; tests inject their own.

The `.sha256` file remains published because existing installers consume it. It is not
the root updater's trust anchor. Payload plus checksum from one HTTPS release path has
the trust level of today's curl-to-shell path because both can be replaced together.
The detached Ed25519 signature adds an independent publisher key.

### 2.2 Filesystem and unit boundary

The installed layout is fixed:

```text
/usr/lib/basement/versions/<validated-version>/basement
/usr/lib/basement/current -> versions/<validated-version>
/usr/lib/basement/basement -> current/basement
/usr/lib/basement/updater/basement-updater

/var/lib/basement/updates/staging/pending/     basement:basement
/var/lib/basement/updates/staging/partial/     basement:basement
/var/lib/basement-updater/                     root:root, readable by basement
```

The browser and manager cannot configure any of these paths. Version components are
strictly parsed, never interpolated from arbitrary strings, and symlink targets are
created by the root updater from validated local names.

Exactly two new units exist:

- `basement-updater.path` watches the one fixed pending request and triggers the one
  fixed service;
- `basement-updater.service` runs the one fixed root-owned helper with the one fixed
  `apply` operation.

The service also runs once at boot after systemd's first start or start attempt for
`basement.service`, then accepts the selected target or restores and restarts the
previous slot from the durable journal. It is not an ordering prerequisite of a manager
start it performs itself. A missing transaction is a successful no-op. The path and
service have start-rate limits, and every invalid request is consumed into a bounded
quarantine receipt so it cannot trigger a loop.

No polkit rule is installed. If a later platform cannot provide systemd path units, that
platform needs a separate ADR rather than a broader fallback in this one.

### 2.3 Root transaction

The root-owned journal state machine is:

```text
none
  -> prepared
  -> switched
  -> target_healthy

switched
  -> rollback_switched
  -> rolled_back
  -> recovery_required
```

The helper fsyncs journal and slot changes before advancing state. It copies from
manager-owned staging into a root-owned temporary slot, then repeats signature, strict
manifest, version, size, digest and ELF verification. It never executes staging bytes.

`current` changes through a temporary symlink and atomic rename. The previous target is
recorded before the flip. The helper runs only fixed systemd operations on
`basement.service`.

Health requires all of these within a bounded window:

- systemd reports a running main process;
- `/proc/<main-pid>/exe` resolves to the expected slot;
- the effective `--listen` argument is read from that main process, not the update
  request;
- `GET /healthz` returns success, `status: ok` and the signed target version;
- that condition remains stable for the configured first-start observation period.

Wildcard listen addresses are normalized to loopback for the local probe. Other
addresses must be assigned to the local machine and must match the running process.
The helper never probes a request-supplied host.

Failed target health flips back and runs the same check against the previous version.
Failed rollback health records `recovery_required` and stops automatic retries. Slot
pruning runs only after success and retains current plus immediate rollback.

### 2.4 Manager update state

Manager update is not an engine recipe job. Its process restart and root receipt are a
different lifecycle. It has one local persisted attempt with these owner-visible states:

```text
available
checking_signature
downloading
verifying
waiting_for_root
restarting
checking_health
succeeded
rolled_back
recovery_required
failed_before_handoff
```

The manager owns pre-handoff progress. The root helper owns all status from request
consumption onward. The manager can read but cannot replace the root receipt. On startup
it reconciles the two sources by attempt id and manifest digest. A browser disconnect is
never a terminal state.

One console-session and CSRF-protected `POST /api/v1/update/apply` creates an attempt.
`GET /api/v1/update` remains the availability and eligibility view.
`GET /api/v1/update/status` reports the persisted attempt. Neither mutation accepts an
API key, release URL, asset path or target version other than the server's currently
verified candidate.

The apply admission transaction:

1. Refuses if another update exists.
2. Refuses if an engine job is non-terminal.
3. Refuses if a generation is queued or running.
4. Refuses if fleet policy does not authorize this local step.
5. Takes an update-maintenance latch that all new mutations check.
6. Persists the intent before starting the asynchronous download.

If the check and a job submission race, exactly one wins. The update is never silently
queued. Inference and reads continue until manager restart.

### 2.5 Store rollback contract

`schema_meta` becomes one authoritative row:

```text
schema_version
minimum_reader_schema_version
last_writer_manager_version
```

Migrations are an ordered registry, each applied in an explicit transaction. A binary
declares the schema range it can read. Startup refuses a database newer than that range.
A release is one-click eligible only when its migrations leave the database readable by
every signed `rollback_from` manager.

Allowed inside a rollback window:

- add a table;
- add an index;
- add a nullable column;
- add a column with an old-reader-safe default;
- write new state in a new table or column without changing old row meaning.

Refused inside a rollback window:

- drop or rename old-reader data;
- change an existing column's meaning or constraint;
- rewrite existing states to values the old binary cannot interpret safely;
- raise `minimum_reader_schema_version` above the rollback binary;
- run a non-transactional migration;
- sign a release without a target-write and previous-reader qualification receipt.

Destructive changes use an expand, migrate, contract sequence across later releases.

### 2.6 Fleet contract

Before fleet upgrade support lands, local apply reports update availability but refuses
installation on an enrolled node. A member console cannot bypass its controller, and an
old two-node setup cannot safely create unmanaged skew.

After the ADR 0016 prerequisite lands, the controller owns one durable maintenance run
with one signed target. Every node still owns its local root transaction. The controller
does not relay binary bytes or root operations.

The sequence is:

1. Freeze fleet placement and membership changes.
2. Wait for mutating jobs and generations.
3. Ask every reachable node to download and verify the same signed manifest.
4. Apply one node at a time: idle members, distributed workers, their group heads, then
   the controller.
5. Keep heartbeat, update status and rollback compatible across current and immediately
   previous versions only.
6. Refuse placement and other fleet mutations until exact versions agree again.

An unreachable member is never reported as staged, updated or healthy. The owner may
continue with reachable nodes, but maintenance remains active and the returning old node
is version-skewed and ineligible. Removing that node is the explicit ADR 0016 membership
action, not an update side effect.

## 3. Mockup contract

This is a new visual surface. Static owner approval is required before any Phase E
console code. Phase F fleet update additions need their own approved fleet-state mockup
if they were not included in the local approval.

Mockups describe information, action hierarchy and state transitions. They do not design
pixels. They must show:

1. Verified update available: current and target versions, signed status, release notes,
   expected console and `/v1` reconnection, running-container statement, primary update
   action and secondary release-page action.
2. Busy refusal: a named active job and a named queued or running generation, with the
   route back to Activity or Generate.
3. Trust and compatibility refusal: missing signature, wrong signature, missing updater
   bootstrap, unsupported protocol and a current version outside `rollback_from`.
4. Progress: check, download, verification, handoff, expected disconnect, reconnect and
   reload during root work.
5. Outcomes: success, automatic rollback and recovery required, each naming the version
   actually running.
6. Fleet: selected node and role, all-node staging, mixed-version maintenance, order,
   unreachable member and placement refusal.

The owner must be able to distinguish `update available`, `release verified`, `ready to
restart`, `manager unavailable during restart`, `running target` and `restored previous`
without relying on colour alone.

## 4. Phases and evidence

Future implementation phases run the complete documented suite appropriate to their
files. This design task runs none of it.

```text
go build ./...
go vet ./...
go test ./...
go test -race ./...
cd webui/ui && npm ci && npm run build && npm test
```

Console phases rebuild committed assets only through the clean-source procedure in
`docs/BUILDING.md`. Packaging phases also run the repository's shell checks. Changes to
protected verification or workflow paths require the project's documented gate and must
not be hidden inside the production implementation unit.

### Phase A. Key decision and signed release manifest

Create the release authenticity layer without enabling installation.

Files and packages:

- new `internal/update/manifest.go`, `signature.go` and focused tests;
- new `cmd/sign-update-manifest` and tests;
- `.github/workflows/release.yml` for manifest inputs and required draft assets;
- new `packaging/sign-linux-update.sh`, called by
  `packaging/sign-macos-release.sh` before its publish step;
- `docs/BUILDING.md` for the release ceremony and key-rotation limitation;
- the release-key public constant in `internal/update`, separate from
  `internal/recipe/signature.go`.

Implementation rules:

- Resolve the owner decision before generating the production key. Recommendation:
  dedicated macOS Keychain item on the controlled release Mac plus a separately stored
  encrypted recovery copy.
- Never place private bytes in the repository, GitHub Actions, argv, environment values,
  logs or assets. The signing command reads them through stdin or a file descriptor.
- Sign the exact generated manifest bytes after measuring the final Linux ARM64 binary.
- Strictly decode one object only after signature verification. Reject unknown fields,
  duplicate or malformed semantic version components, unknown key, wrong platform,
  wrong fixed name and unsupported protocol.
- The release draft is not published if the manifest, signature, binary, size and digest
  do not agree.
- Keep `.sha256` for existing consumers. Do not describe it as publisher identity.
- Production code contains public keys only. The recipe placeholder key at
  `internal/recipe/signature.go:10-19` is not reused.

Evidence:

- Ephemeral-key round trip signs and verifies the exact bytes.
- Changing every individual manifest field, the signature or one binary byte is refused.
- Wrong key, unknown key id, truncated signature, trailing second JSON object and
  unsupported schema are refused before an asset is staged.
- Tag, signed version, measured size and digest must all agree.
- A release-finishing rehearsal leaves a draft unpublished when any required update
  asset is absent or invalid.
- The actual published asset set is fetched and independently verified before the first
  release is promoted.

Hardware: no GB10 verification is needed. The real signing and draft-publication
rehearsal must run on the owner-controlled release Mac. Phase A remains pending if key
custody is undecided or only a placeholder key exists.

### Phase B. Enforced store compatibility

Make rollback readability a store invariant before any code can flip a slot.

Files and packages:

- `internal/store/store.go`, with migrations split into
  `internal/store/migrations.go` if useful;
- `internal/store/store_test.go` and versioned database fixtures;
- `internal/update/compatibility.go` for `rollback_from` admission;
- `cmd/basement/main.go` only for the declared reader and writer version wiring;
- `docs/BUILDING.md` for the expand, migrate, contract rule and release qualification.

Implementation rules:

- Migrate the existing one-column `schema_meta` additively. Do not drop it and recreate
  it around live data.
- Use one row and explicit ordered transactions. A partially failed migration leaves the
  prior schema version.
- Record schema version, minimum reader and writing manager separately.
- Keep every schema and state change backward-readable by all declared rollback
  versions.
- A binary that cannot read the database fails before starting API listeners or writing
  rows.
- A release whose current version is not in signed `rollback_from` remains visible but
  is not console-installable.
- Direct multi-release jumps need explicit compatibility evidence for the actual current
  version. Do not assume semantic ordering implies database readability.

Evidence:

- A database made by the current flat-install version migrates without losing jobs,
  steps, models, licences, keys, roles, generations, metrics or peers.
- Failure injected after every migration statement reopens at either the old or complete
  new schema, never a half version.
- The target opens a fixture made by each declared rollback version, performs every
  target write that can occur before health, closes, and the actual previous binary then
  opens and reads all prior rows.
- A target cannot raise minimum reader past its declared rollback binary.
- An unknown future schema is refused with no write.
- `last_writer_manager_version` changes only after successful migration and open.

Hardware: none. This phase is SQLite compatibility evidence and is shippable dormant.
It does not enable update apply.

### Phase C. Manual bootstrap, version slots and root recovery

Install the narrow privileged path and prove it independently of HTTP and the console.
This is the phase that existing machines receive manually.

Files and packages:

- new `cmd/basement-updater`;
- new `internal/update/apply.go`, `journal.go`, `health.go`, filesystem helpers and
  focused tests;
- new `packaging/systemd/basement-updater.path` and
  `packaging/systemd/basement-updater.service`;
- matching `internal/setup/assets/basement-updater.path` and
  `internal/setup/assets/basement-updater.service`;
- `packaging/systemd/basement.service` and matching
  `internal/setup/assets/basement.service` for boot ordering and the current slot;
- `packaging/install.sh`, `packaging/uninstall.sh`, `internal/setup/install.go`, setup
  assets and setup tests for flat-install adoption and unit parity;
- `.github/workflows/release.yml` for the Linux ARM64 updater-helper bootstrap asset;
- release packaging for the helper, units and updated installer.

Implementation rules:

- The root helper is a separate fixed binary. Never invoke the target manager as the
  privileged updater.
- The normal manager update payload cannot replace the helper, units or key ring.
- The path request is one fixed file written last. It carries no staged path.
- The root helper takes no caller-controlled command, unit, path or URL.
- Copy regular staging files into a root-owned temporary slot, then verify the copy.
  Reject symlinks, devices, sockets, hard-link surprises and changed size or inode state.
- The updater makes no download or nonlocal network request. Its only IP request is the
  validated local manager health probe. It writes only fixed slot, staging-consumption
  and `/var/lib/basement-updater` trees.
- Journal every transition durably. At boot, reconcile after the first manager start or
  start attempt, with the update-maintenance latch still blocking ordinary mutations.
- Derive health from the started process and actual `/healthz`, never request JSON.
- Keep the manager's existing `Restart=on-failure`; do not add `OnFailure=` rollback.
- Preserve existing listen behavior. The compatibility symlink must make both the base
  unit and an old `listen.conf` drop-in start the selected slot.
- Copy the existing flat binary into a rollback slot before turning its path into a
  symlink. If version output cannot be normalized, use a root-generated bootstrap slot
  name tied to its digest rather than trusting arbitrary output as a path.
- Do not touch `/var/lib/basement/manager.db`, artifacts, recipes, images, model config
  or Docker.
- Uninstall disables both updater units and handles slots while preserving manager data,
  matching the current policy at `packaging/uninstall.sh:25-32`. It reports whether
  update receipts were retained.

Evidence:

- A fake-root harness covers success, target health timeout, target crash, wrong version,
  wrong executable path, rollback success and rollback health failure.
- Kill or power-loss injection after every journal, copy, slot rename, symlink flip,
  service stop, service start and health response converges to target healthy, previous
  healthy or explicit recovery required.
- A staged symlink, path traversal, special file, tampered copy, signature mismatch,
  size mismatch, wrong architecture and wrong version cause no symlink flip.
- Invalid repeated requests hit bounded rate limits and cannot create an apply loop.
- A flat install with no drop-in, a loopback drop-in and a non-loopback drop-in all
  preserve address and start through the selected slot.
- Package and embedded unit files are byte-identical.
- `systemd-analyze verify` accepts all three units on the target distribution.
- Unit inspection proves no polkit rule, sudoers entry, extra manager capability or
  manager write access to `/usr/lib/basement` was added.

Hardware: required before this phase can be called production-ready. On a real GB10,
exercise the manual flat-install bootstrap, normal slot swap, deliberately unhealthy
target rollback, updater-process kill, service restart and machine power loss at a
recorded transition. Confirm the old and new listen configurations. Local fakes do not
replace this evidence.

### Phase D. Local staging, admission and persisted API status

Connect the unprivileged manager to the already qualified root path. Do not add console
controls yet.

Files and packages:

- new `internal/update/stager.go`, `status.go`, release client and tests;
- `internal/httpapi/update.go` split from `server.go`, with focused API tests;
- `internal/httpapi/server.go` for route wiring and the shared mutation-maintenance
  guard;
- `internal/store` queries or transactions that atomically detect active jobs and
  generations;
- engine, generation and fleet mutation entry points only to consult the maintenance
  guard;
- `cmd/basement/main.go` to reconcile persisted manager and root update state at start;
- `webui/ui/src/api.ts` type changes only if a nonvisual compatibility update is needed;
  no visible surface in this phase.

Implementation rules:

- `GET /api/v1/update` distinguishes available, signed, compatible and locally
  installable. A tag alone never produces `installable: true`.
- Use strict version ordering. Do not offer downgrade or treat every unequal string as an
  update.
- Apply is console session plus CSRF and origin only. API keys and fleet legacy bearer
  keys are refused.
- The server chooses the verified candidate. The request body cannot choose a version,
  URL or path.
- Bound request time, redirects, response bytes and disk use. Stream the manager binary;
  do not read it all into memory.
- Write partial, verified and request files through same-directory atomic renames and
  fsync the handoff.
- Active job, active generation and update latch admission is one transaction or one
  serialized critical section. Exactly one of racing work and update wins.
- New mutation endpoints return `409` with the exact blocking activity after update
  maintenance begins. Reads and inference remain allowed until restart.
- Do not queue an update for later automatic restart.
- On restart, reconcile attempt id and manifest digest with the root receipt. A missing
  browser is not failure. A partial pre-handoff download is resumable only if its remote
  identity and local digest state still match; otherwise restart it safely.
- Redact release transport errors and root receipts before logs or API output.
- If the updater helper or units are absent or disabled, report manual bootstrap needed
  and never write the path marker.
- If enrolled in a fleet before Phase F, report fleet coordination required and refuse
  local apply.

Evidence:

- HTTP tests cover unsigned, wrong signature, wrong tag, downgrade, development build,
  wrong platform, unsupported protocol, current version absent from `rollback_from`,
  missing bootstrap and a valid candidate.
- Body limits hold when Content-Length is missing, false or excessive.
- A partial network response never creates `request.json`.
- Reopening the verified file catches mutation before handoff.
- Concurrent update, install, start, stop, remove, benchmark, smoke-test and generation
  submissions admit exactly one mutation policy winner.
- A running job and a queued or running generation return a `409` naming the activity.
- Restart during download, after verification and after root handoff reconstructs the
  correct persisted state.
- The root status file under `/var/lib/basement-updater` can be read but not replaced by
  user `basement`, including by renaming its parent.
- No API accepts update mutation with a bearer key.

Hardware: required for end-to-end qualification with Phase C. On a real GB10, use a
signed test release through the real manager, path unit, root service, restart and
health response. Keep the console hidden until this passes.

### Phase E. Console update experience

Implement only after all local update mockups in section 3 are approved.

Files and packages:

- `webui/ui/src/api.ts` for named update availability and attempt types;
- `webui/ui/src/App.tsx` to replace the sidebar anchor with a button;
- new `webui/ui/src/views/ManagerUpdate.tsx` if the surface is large enough to justify a
  focused component;
- `webui/ui/src/styles.css` using its existing colour roles and pill family;
- focused UI tests and mock-harness fixtures;
- `internal/webui/assets` rebuilt from committed source through the clean procedure.

Implementation rules:

- Show the install action only for signed, compatible, locally bootstrapped releases.
- An external release page is secondary. It is not the primary update action.
- Before confirmation, state that the console and `/v1` proxy reconnect while model
  containers are left alone. Do not promise uninterrupted requests.
- Name the active job or generation that blocks apply and link back to its existing
  console location.
- After apply is accepted, poll persisted status. Treat expected disconnect as
  `Restarting basement`, not generic offline failure, for the bounded updater window.
- A reload reconstructs progress from the server. Do not depend on component state to
  remember the attempt.
- Success and rollback name the version actually running. Recovery required gives the
  manual recovery path without apology or blame.
- Keep one primary action in each state. Do not add an automatic-update toggle.
- Render release notes through the sanitized Markdown pattern at
  `webui/ui/src/views/Playground.tsx:33-36` if spec 14 has landed. Missing notes are not
  an authenticity failure.
- No emoji, no em dashes, no invented time or uptime guarantee.

Evidence:

- Approved mockups are attached to the implementation report before code screenshots.
- Mock fixtures cover verified available, active job, active generation, missing
  bootstrap, signature failure, incompatibility, download, verification, expected
  disconnect, reload during root work, success, rollback and recovery required.
- Accessibility checks cover keyboard operation, focus return, live status that does not
  chatter on every poll, and outcomes that do not depend on colour.
- Screenshots show every required state at the supported console viewport sizes.
- Typecheck, UI tests, build and committed-asset drift check pass.

Hardware: required for final acceptance. Drive the real console through one successful
update and one forced rollback on a GB10. Capture the state before disconnect and the
state reconstructed after reconnect. Mock screenshots alone do not qualify the action.

Mockup approval: required before this phase starts.

### Phase F. ADR 0016 fleet maintenance and per-node apply

Start only after ADR 0016's identity, fleet transport, persistent reservations and
maintenance prerequisites exist. This phase replaces the temporary enrolled-node local
refusal with controller-coordinated per-node actions.

Files and packages:

- `internal/fleet/upgrade.go` for the durable maintenance state machine and order;
- `internal/fleet` transport messages for exact release identity, stage, apply, status
  and rollback only;
- `internal/store` fleet update-run, node attempt and maintenance records;
- `internal/httpapi` controller fleet-update routes and member local refusal;
- `internal/update` only for the local node adapter to the existing apply and status
  contract;
- `webui/ui/src/views/Fleet.tsx`, manager-update surface and Activity projection after
  fleet mockup approval;
- plan 12 Phase G files where that plan and this phase overlap.

Implementation rules:

- The controller chooses one exact signed manifest digest. Every node independently
  downloads and verifies it.
- Stage all reachable nodes before the first swap. Never claim an unreachable node is
  staged.
- Maintenance blocks placement, reservations, membership changes and unrelated fleet
  mutations. Local reads, heartbeat, update status and rollback remain available across
  current and immediately previous versions.
- Local engine jobs and generations must be idle before that node enters apply. Do not
  cancel them as an update side effect.
- Apply only one node at a time. Idle members precede distributed workers, each worker
  precedes its group head, and the controller is last.
- A direct member-console update is refused with the controller's identity. The
  controller calls a narrow fleet update operation, not the public console endpoint and
  not the legacy general API key.
- Existing containers, allocations, recipes and artifacts are unchanged. Mixed manager
  versions block new placement even if containers keep serving.
- A node that rolls back remains version-skewed. Keep maintenance active and show its
  exact target, restored version and receipt.
- A node unreachable at start can be excluded only with an explicit owner decision that
  accepts continuing maintenance. When it returns, it remains ineligible until updated
  or removed through the membership flow.
- Never update a group head before its worker, the controller before reachable members,
  two nodes in parallel, or one node to a different release.
- The controller restart reconstructs its update run from its local record plus member
  receipts. It does not infer success from heartbeat absence.

Evidence:

- A fake four-node fleet stages one manifest everywhere and applies in the exact decided
  order.
- Attempts to update a head first, controller first, two nodes together or a different
  target are refused without a local path marker.
- Placement, reservation and membership endpoints refuse throughout skew; heartbeat,
  update status and rollback continue across one version.
- A worker rollback, group-head rollback and controller rollback each leave exact honest
  fleet state and no automatic second-node apply.
- Controller loss before its own turn and during its own restart reconstructs the same
  run after return.
- An unreachable member is never marked staged or healthy. Its later heartbeat shows
  version mismatch and it remains ineligible.
- Existing independent and distributed deployment records, containers, recipes and
  artifacts are byte- and identity-equivalent before and after the manager run.
- Fleet screenshots cover all-node verified staging, each role in order, mixed versions,
  unreachable member, rollback and placement refusal.

Hardware: required. A two-node hardware run must verify worker-before-head update and
forced rollback while the distributed containers are present. Any claim that distributed
inference continues through a worker-manager restart requires an observed real inference
run. Complete claims for idle-member, worker, head and controller order require enough
real machines to exercise each distinct role; otherwise the report limits the claim to
the roles actually tested.

Mockup approval: required for fleet update and mixed-version maintenance states before
their UI implementation.

## 5. Failure states that stay visible

| Failure | Owner-visible result | Mutation allowed |
| --- | --- | --- |
| Latest release has no valid signature | Release cannot be verified and is not installable | Retry check or open diagnostics only |
| Newest release does not list the running version in `rollback_from` | An older reachable release is offered instead, and the next hop is offered after it succeeds | Normal one-click update, one hop at a time |
| No release at all lists the running version | This update cannot guarantee rollback from the running version | Manual documented upgrade only |
| Updater bootstrap is absent | Run the installer once to enable console updates | No path request |
| Engine job is active | Named job must finish or be cancelled | Existing job controls only |
| Generation is queued or running | Named generation must finish or be cancelled | Existing generation controls only |
| Download interrupted | Partial download retained only when safely resumable | Retry the same attempt |
| Root request accepted and browser disconnects | Restarting basement, with reconnect in progress | Status reads when service returns |
| Target health fails and previous health passes | Target failed, previous exact version restored | Normal actions after maintenance clears |
| Target and previous health fail | Recovery required, previous slot remains selected | Local administrator recovery only |
| Fleet versions differ | Exact versions and per-node outcomes shown | Heartbeat, update status and rollback only |
| Fleet member is unreachable | No claim that it was staged or updated | Maintenance remains; explicit membership recovery only |

## 6. Owner decisions and release gates

### O1. Private-key custody

Decision required before Phase A production signing. Recommendation: dedicated macOS
Keychain item on the controlled release Mac, plus one separately stored encrypted
recovery copy. CI never receives the key.

### O2. First bootstrap presentation

Recommendation: the current link-only surface should say that the installer must be run
once to enable console updates when it can detect an update-capable release but has no
local updater. It must not label that manual step one click.

### O3. Unreachable fleet member

Recommendation: allow reachable nodes to update only after explicit confirmation, keep
the fleet in maintenance, and exclude the old returning node until it updates. Blocking
all security updates on one powered-off member is worse, but silently exiting maintenance
would violate exact-version placement.

The feature is not complete until all of these are true:

- production release key custody is decided and the public key is embedded;
- a published signed manifest has been independently verified;
- previous-reader database evidence passes for the target release;
- manual bootstrap and both rollback paths pass on real GB10 hardware;
- local mockups are approved and the real console reconnect path passes;
- fleet controls remain refused until their own protocol, mockup and hardware evidence
  pass.
