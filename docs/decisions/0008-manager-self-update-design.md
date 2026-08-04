# ADR 0008: One-click manager update

Date: 2026-08-04. Status: proposed revision. Design only.

This revision replaces the earlier accepted outline. Notification is implemented.
Download, signed verification, privileged apply, health gating and rollback are not.
The owner must decide the release private-key custody described below before the apply
path can ship.

## Context

The current update feature is a notification, not an updater:

- `GET /api/v1/update` is registered as a console-authenticated read route in
  `internal/httpapi/server.go:184-188`.
- `updateCheck` calls the latest-release API for `punkjazz-labs/basement`, limits the
  response body, compares the tag with the running version and caches the result for one
  hour in `internal/httpapi/server.go:1886-1927`. The former six-hour statement in this
  ADR was wrong. The comparison is string inequality, not version ordering
  (`internal/httpapi/server.go:1912-1917`).
- The console asks for that result at startup and once an hour
  (`webui/ui/src/App.tsx:125-135`). Its only update action is an external link labelled
  `Update {version} available` (`webui/ui/src/App.tsx:291-295`).

The manager cannot replace itself, and that confinement must remain true:

- `basement.service` runs as user and group `basement`, has no capabilities, enables
  `NoNewPrivileges`, mounts the system read-only with `ProtectSystem=strict`, and gives
  the process one writable tree, `/var/lib/basement`
  (`packaging/systemd/basement.service:8-41`).
- The installed executable is `/usr/lib/basement/basement`
  (`packaging/install.sh:34-37`). A non-loopback installation may also have a drop-in
  whose replacement `ExecStart` names that same path (`packaging/install.sh:63-66`).
  Updating only the base unit would therefore not migrate every existing machine.

This is a direct filesystem privilege boundary, not a claim that arbitrary malicious
manager code is contained. The service belongs to the `docker` supplementary group
(`packaging/systemd/basement.service:10-13`), and the unit itself records that Docker
group membership is privileged (`packaging/systemd/basement.service:19-21`). Inference:
the updater design avoids broadening normal authority and prevents unsigned staging from
being applied, but it does not repair the pre-existing Docker-daemon trust boundary.

The release channel does not yet provide Linux publisher authentication:

- The workflow builds `basement-linux-arm64` and writes a SHA-256 file beside it
  (`.github/workflows/release.yml:23-35`).
- It creates a draft containing the built assets (`.github/workflows/release.yml:80-99`).
- The manual signing script signs and notarizes the two Darwin manager binaries, then
  publishes the draft (`packaging/sign-macos-release.sh:29-47,55-62`). It does not sign
  the Linux manager.
- The current bootstrap downloads a binary and its checksum from the same release
  location and compares them (`packaging/setup.sh:30-47`). If that delivery path can be
  changed, both files can be changed together.

The current store follows some rollback-friendly practices but does not enforce a
rollback contract. It creates `schema_meta` with a single integer fixed at `1`
(`internal/store/store.go:145-149`). Existing migrations add a column with a default or
add new progress columns with defaults (`internal/store/store.go:261-306,309-345`). No
step in the complete current migration path reads or advances `schema_meta`, records a
writing manager version, or refuses an incompatible reader
(`internal/store/store.go:145-258`). The current implementation therefore does not
satisfy this ADR's rollback guarantee.

ADR 0016 adds a second constraint. Fleet placement requires exact signed release
identity, and a skewed node stays visible but is excluded from new reservations and
fleet mutations (`docs/decisions/0016-multi-node-fleet.md:490-498`). Its accepted upgrade
order is idle members, distributed workers, group heads and the controller last
(`docs/decisions/0016-multi-node-fleet.md:508-517`).

## Decision

### Scope and invariants

One-click update changes the basement manager release on one Linux ARM64 node. It does
not update recipes, model artifacts, images, generated configuration or model
containers. It never runs a release script. It never accepts a release URL, filesystem
path, systemd unit or command from the browser.

The local update has these gates, in order:

1. Discover a newer stable release.
2. Download and verify a signed release manifest.
3. Download the exact Linux ARM64 manager named by that manifest.
4. Verify its size and SHA-256 digest before it becomes a staged request.
5. Refuse incompatible versions and active work.
6. Hand a fixed request marker to a root updater.
7. Install into a new version slot, restart, and verify the exact expected version at
   the existing `/healthz` endpoint.
8. Keep the new slot only after health succeeds. Otherwise restore the previous slot and
   verify it again.

A development build, prerelease, downgrade, unknown manifest schema, unknown signing
key, unsupported updater protocol or release outside the declared rollback window is
not installable from the console.

### Release authenticity

Each published release carries these Linux update assets:

- `basement-linux-arm64`;
- the existing `basement-linux-arm64.sha256`, retained for people and existing
  installers;
- `basement-linux-arm64.update.json`, the signed manifest;
- `basement-linux-arm64.update.sig`, one line containing base64 of the raw 64-byte
  Ed25519 signature over the exact manifest bytes.

Manifest schema 1 contains only fixed fields:

```json
{
  "schema_version": 1,
  "key_id": "release-1",
  "release_version": "vX.Y.Z",
  "os": "linux",
  "arch": "arm64",
  "asset_name": "basement-linux-arm64",
  "asset_size": 0,
  "asset_sha256": "64 lowercase hexadecimal characters",
  "updater_protocol": 1,
  "rollback_from": ["vW.Y.Z"]
}
```

The zero and placeholder versions above describe types, not release values. The release
finishing tool writes the measured byte size, digest and real versions. JSON is encoded
once, and `crypto/ed25519.Sign` signs those exact bytes. Verification uses
`crypto/ed25519.Verify`. The detached signature format is the simple raw-Ed25519 format
already described by `internal/recipe/signature.go:32-48`, not minisign wire format. No
new cryptography dependency is added.

The update release key is separate from the recipe-feed key. The public key and key id
are public repository data embedded into both the manager and the root updater at build
time. Tests can inject ephemeral keys. Production cannot replace a key through an API,
environment variable, data-directory file or release manifest.

Before writing the fixed request marker, the unprivileged manager must:

1. Fetch bounded manifest and signature bodies from assets belonging to the release it
   just resolved. It does not accept browser-supplied locations.
2. Select a known embedded key id and verify the signature over the untouched manifest
   bytes before decoding trusted fields.
3. Decode exactly one JSON object, reject unknown fields and reject any schema or
   updater protocol it does not implement.
4. Require a strict stable `vMAJOR.MINOR.PATCH` target greater than the running release.
   The release tag and signed `release_version` must match exactly.
5. Require `linux`, `arm64` and the fixed asset name.
6. Require the running version in signed `rollback_from`. A machine several releases
   behind is not silently jumped across an unproved database compatibility window. It is
   also not abandoned to a manual upgrade: see the multi-hop rule below.
7. Stream the binary into a temporary file while enforcing a hard response limit,
   exact signed size and exact signed SHA-256. A partial file is never executable and
   never renamed into the pending request.
8. Apply the existing Linux ARM64 ELF check pattern from
   `internal/setup/install.go:144-153` as an early format error. The signature and digest,
   not ELF headers, establish trust.
9. Reopen and re-verify the on-disk bytes before atomically creating the request marker.

#### Multi-hop: a machine several releases behind still updates in one click

The compatibility window is a property of each release, so the console must not resolve
only the newest release and give up when the running version is outside its
`rollback_from`. It resolves the newest release whose signed `rollback_from` contains the
running version, and offers that. When the machine is one release behind, which is the
ordinary case, this is the newest release and nothing changes. When it is further behind,
the owner is offered a real reachable target rather than a refusal telling them to go and
read an upgrade document.

After that hop succeeds, the same resolution runs again from the new running version. The
console offers the next hop the moment it is available, and may chain the hops
automatically as one owner action provided each hop is separately verified, separately
health-gated and separately able to roll back. Chaining changes how many restarts happen,
never what is proved about each one. A hop that rolls back stops the chain and reports
the version actually running.

This keeps the safety property that motivated `rollback_from` while removing the outcome
it would otherwise produce: an owner who skipped a couple of releases being told the
one-click button does not apply to them. If no release at all lists the running version,
the refusal is honest and manual upgrade is the answer, but that is the rare tail rather
than the standing behaviour for anyone who did not update promptly.

The privileged updater independently repeats signature, manifest, version, size, digest
and ELF verification on a root-owned copy. Verification by the manager improves user
feedback. Verification by the updater is the security boundary.

A checksum fetched beside a binary over the same HTTPS release path is the same trust
level as today's curl-to-shell installation: it detects accidental corruption, but an
attacker who can replace the binary can replace the checksum. Ed25519 signing adds an
independent publisher-authentication key. A release-hosting or transport compromise
cannot produce accepted replacement bytes without that private key. Signing does not
protect against theft of the private key, a compromised signing machine, or the owner
deliberately signing a bad build.

#### Owner decision: private-key custody

The owner must choose and record the private-key location before a real key is generated.
Recommendation: keep a dedicated update-signing key in the macOS Keychain on the same
controlled Mac used to finish releases, with one separately stored encrypted recovery
copy. The finishing tool should receive key bytes through a pipe or file descriptor and
must not put them in the repository, GitHub Actions, command arguments, environment
values, logs or release assets.

The key remains outside CI. The current workflow already leaves releases as drafts, and
the manual Mac step publishes them last. A new Linux-update signing step runs before
that publication. The publish step refuses a draft missing a valid update manifest and
signature.

The root updater is deliberately not replaced by an ordinary one-click manager update.
A release-key rotation or updater-unit change therefore needs either a separately
designed old-key-signed transition or a manual installer upgrade. Version 1 chooses the
manual route. This is narrower than letting the manager replace its own privileged
helper.

### Privilege path

Two paths were evaluated.

| Path | Benefit | Cost and risk |
| --- | --- | --- |
| systemd path unit watches a fixed request marker | The manager only writes inside its existing data directory. It gains no systemd, D-Bus or authorization power. The root service has no caller arguments. | A compromised manager can create invalid requests repeatedly, so the path and service need trigger limits and must consume or quarantine every request. |
| polkit permits user `basement` to start one unit | The trigger is explicit and does not depend on a filesystem watch. | It adds a distribution-sensitive authorization rule, a D-Bus/systemd call from the confined service, and another policy surface whose unit and verb matching must remain exact. It provides no stronger payload boundary because the same manager still supplies staging bytes. |

The decision is the systemd path unit. It has the smaller authority and audit surface.
No polkit rule, sudoers entry, setuid binary or added manager capability is installed.

Exactly two new unit files are required, with matching embedded setup assets:

1. `basement-updater.path` watches only
   `/var/lib/basement/updates/staging/pending/request.json`. It starts only
   `basement-updater.service`, is enabled at boot and has bounded trigger rate. The
   manager writes the request last with an atomic rename. The request names a version
   and digests but contains no filesystem path.
2. `basement-updater.service` is a root `Type=oneshot` service. Its only command is the
   root-owned fixed helper `/usr/lib/basement/updater/basement-updater apply`, with no
   request-derived arguments. It is also enabled as a boot-time no-op/recovery check.
   At boot it runs after systemd's first start or start attempt for `basement.service`,
   inspects the durable transaction and either accepts the selected target or restores
   the previous slot. It must not be an ordering prerequisite of a manager start it
   performs itself.

`basement.service` remains an unprivileged service. Its base unit moves to the versioned
layout. The legacy executable path remains a compatibility symlink so an existing
`listen.conf` drop-in such as the one written at `packaging/install.sh:63-66` still starts
the selected slot.

The updater service may:

- read the fixed staging directory;
- copy candidate bytes into a root-owned temporary slot and verify that copy;
- create root-owned version directories below `/usr/lib/basement/versions`;
- atomically replace the fixed `/usr/lib/basement/current` symlink;
- start, stop and restart only `basement.service`;
- read the started manager's systemd main PID and command line to locate its effective
  listen address;
- poll only that local manager's `/healthz` endpoint;
- write a root-owned transaction journal and a sanitized, read-only status receipt;
- restore the recorded previous slot and prune slots after a successful transaction.

It may not:

- download anything or contact a nonlocal network address;
- execute a file from the manager-writable staging directory;
- accept a path, URL, command, systemd unit name or health endpoint from the request;
- invoke a shell or a release-supplied script;
- modify the database, data-directory contents outside its update subdirectories,
  recipes, artifacts, images, generated model configuration or containers;
- replace its own helper, its units, the manager unit, a listen drop-in or a public key;
- manage any service other than `basement.service`.

The service uses a read-only system view with explicit write access only to the version
slots, manager staging and `/var/lib/basement-updater`. It allows Unix sockets for
systemd and IP sockets for the local health probe. The helper validates that the probe
address belongs to the local machine and makes no release or nonlocal network request.
Its fixed helper copies regular files into a root-owned directory and verifies the
copies, so a service-account race cannot make it execute a different inode after
verification.

### Slots, restart and rollback

The update-capable manual installer creates this layout:

```text
/usr/lib/basement/versions/vX.Y.Z/basement
/usr/lib/basement/current -> versions/vX.Y.Z
/usr/lib/basement/basement -> current/basement
/usr/lib/basement/updater/basement-updater

/var/lib/basement/updates/staging/       owned by basement
/var/lib/basement-updater/               owned by root, readable by basement
```

The second symlink preserves the executable path used by existing listen drop-ins. New
base units use `/usr/lib/basement/current/basement`.

The root updater records every transition in a root-owned journal and fsyncs the new
file and its directory before the next state. The minimum states are `prepared`,
`switched`, `target_healthy`, `rollback_switched`, `rolled_back` and
`recovery_required`. The journal records target, previous slot, manifest digest and
sanitized failure text. It never records private key material, credentials or release
response bodies.

Apply is:

1. Acquire one root-owned update lock.
2. Consume the fixed pending request into a root-owned transaction.
3. Copy and re-verify all signed input into a new temporary version directory.
4. Confirm that the selected previous slot still matches the running manager.
5. Stop accepting new mutations through the manager's already-persisted maintenance
   latch, then stop `basement.service` cleanly.
6. Atomically rename the complete slot into place and atomically flip `current`.
7. Start `basement.service` and poll the existing `/healthz` route. That route currently
   returns `status: ok` and the manager version (`internal/httpapi/server.go:268-274`).
   The updater derives the effective listen address from the new systemd main process,
   not the untrusted request, and requires the expected version and executable slot.
8. Treat success as bounded stable health, not one early response. Record success, clear
   maintenance and retain the selected slot plus one immediate rollback slot.
9. If start or health fails, stop the target, flip `current` to the recorded previous
   slot, start it, and run the same health check for the previous version. Record
   `rolled_back` only if that check passes. If it also fails, leave `current` on the last
   known-good previous slot, record `recovery_required`, and stop automatic retries.

The boot-time updater resumes from the journal. A loss before the symlink flip leaves the
old slot selected. A loss after the flip lets systemd make its first start attempt from
the atomically selected slot, then the boot-time updater either completes target health
or restores and restarts the previous slot. The persisted maintenance latch prevents
ordinary manager mutations before that decision. A fixed request is never replayed as a
new transaction after it has been consumed.

Systemd `OnFailure=` is not the rollback mechanism. It detects a process failure, not a
manager that remains alive while serving a wrong version or an unhealthy API. The
bounded updater transaction owns first-start health and rollback. The ordinary
`Restart=on-failure` policy in `basement.service` remains useful inside that health
window (`packaging/systemd/basement.service:13-16`).

Two known-good slots are retained after success: current and immediate rollback. A
failed target can be removed after its manifest digest and failure receipt are durable.
Pruning never runs before health or after a failed rollback.

#### Running model behavior

Current packaging explicitly records that model containers are detached from the
manager service and survive a service stop (`packaging/uninstall.sh:9-13`). The manager
talks to Docker through the daemon socket (`internal/operations/docker.go:85-90`) and
creates the container through the Docker API (`internal/operations/docker.go:348-380`).
Its SIGTERM path shuts down HTTP, token accounting and the database but calls no
container stop operation (`cmd/basement/main.go:152-171`). Inference: because the updater
is forbidden to touch containers, a running model container also survives its manager
restart.

The model process can therefore keep running on its loopback port. External clients
temporarily lose basement's authenticated `/v1` proxy, because that route belongs to the
manager (`internal/httpapi/server.go:201`) and model ports are bound to loopback
(`internal/operations/docker.go:310-318`). If the container itself fails during the
manager outage, this design does not claim that update kept inference alive.

On every manager start, the store marks a formerly ready active model as `recovering`
(`internal/store/store.go:123-130`). Startup creates a start job for that model
(`internal/engine/engine.go:297-312`), and the start operation reuses an already running
container or recreates a missing or stale one (`internal/operations/host.go:297-361`).
The update receipt must distinguish manager health from completion of this model
reconciliation. A successful manager update promises that basement came back at the
expected version. It does not promise uninterrupted in-flight `/v1` requests.

### SQLite state compatibility

Rollback restores code, not a database snapshot. The target may have opened and written
`manager.db` before its health check fails. The previous binary must therefore be able to
read every database state the target can produce during the rollback window.

Every one-click-eligible release must follow these rules relative to every version in its
signed `rollback_from` list:

- Schema changes use an explicit ordered migration transaction.
- Allowed changes are new tables, new indexes and new columns that are nullable or have
  defaults older code can tolerate.
- Existing columns, tables and indexes read by the previous version are not dropped,
  renamed or changed in meaning during that window.
- Existing state values are not rewritten to values the previous version treats as
  impossible or as a different action.
- New behavior writes new state into new columns or tables. Old rows remain valid.
- A destructive change uses expand, migrate, contract across later releases. The
  contract step cannot be one-click eligible while the immediate rollback binary still
  needs the old shape.
- Migration failure rolls back its transaction and leaves a database the previous
  version can open.
- The release qualification opens a previous-version fixture with the target, exercises
  target writes and then opens and reads it with the actual previous binary.

`schema_meta` becomes one authoritative row with `schema_version`,
`minimum_reader_schema_version` and `last_writer_manager_version`. Each binary declares
the schema range it reads. Startup refuses a database whose minimum reader version is
newer than the binary. A target eligible for automatic rollback must not raise the
minimum reader beyond its declared previous reader. The writing manager version is an
audit field, not a substitute for the reader rule.

The current store does not meet this contract. It demonstrates the additive technique,
especially in `migrateGenerationProgress`, but the fixed, unread `schema_meta` at
`internal/store/store.go:145-149` cannot prove compatibility. The migration registry,
reader check and cross-version evidence are prerequisites to enabling apply.

### First upgrade on existing machines

There is no bootstrap paradox hidden behind the console. The current installer writes
the flat binary and `basement.service`, then enables only that service
(`packaging/install.sh:23-37,69-70`). Machines installed by it have no updater helper,
updater units or version slots. The running manager cannot install those under its
current confinement.

The first upgrade to the update-capable release is manual and requires root through the
existing installer or setup flow. That installer must:

1. preserve `/var/lib/basement`, the service user and the effective listen address;
2. copy the existing flat binary into a rollback slot before replacing its path;
3. install the new manager into a signed version slot;
4. create `current` and the legacy compatibility symlink;
5. install the fixed root updater and both units;
6. create manager-owned staging under `/var/lib/basement` and the separate root-owned
   `/var/lib/basement-updater` state directory;
7. reload systemd, enable the updater units and restart the manager;
8. verify the manager and updater installation before reporting success.

That bootstrap is not one click. If its binary, checksum and public key all arrive over
the same current download channel, its first trust decision is still today's HTTPS
trust. The embedded Ed25519 key protects subsequent console updates. Documentation and
the console must say `Run the installer once to enable console updates`, not pretend the
current link can install the privilege path.

### Console and API experience

The sidebar link becomes a button that opens a manager-update surface. The external
release page remains a secondary action, not the update action.

Before apply, the owner sees:

- running and target versions;
- release notes when that separate feature supplies them;
- `Signed release verified` only after the manifest signature is actually verified;
- a plain refusal if the signature, version, platform, rollback compatibility or local
  updater installation is wrong;
- the consequence that the console and `/v1` proxy will reconnect while a running model
  container is left alone;
- the exact node name and fleet consequence when applicable.

The primary action starts one asynchronous local attempt. The server first takes an
update-maintenance latch and atomically checks for active work. It returns `409` if any
engine job is non-terminal. Today's terminal set is `ready`, `failed`, `cancelled`,
`stopped` and `removed` (`internal/httpapi/server.go:2055-2060`). It also refuses while a
media generation is queued or running, because restart explicitly interrupts those
states (`internal/store/store.go:131-139`). The response names the blocking activity and
tells the owner to finish or cancel it. The update is not queued to surprise the owner
later.

After that check wins, new mutating jobs and generations are refused until staging fails
or the root transaction finishes. Read-only console calls and inference continue until
the restart. This closes the race where an install could begin after the update checked
for idle work.

During apply, the surface reports persisted states: checking, downloading, verifying,
ready to install, restarting and checking health. Loss of the HTTP connection during
restart is expected. The browser reconnects and reads status rather than changing the
attempt to failed locally.

After apply, the new or restored manager reads the root-owned sanitized receipt:

- success names the running target version;
- rollback says that the target did not become healthy and names the restored version;
- recovery required says the manager could not verify either start and gives a local
  recovery instruction;
- a pre-handoff interruption resumes or safely restarts the partial download;
- a post-handoff interruption is resolved from the root journal, including after power
  loss.

This is a new visual surface. Static mockups require owner approval before console
implementation. They describe states and hierarchy, not pixels. Required mockups are:

1. Available and verified, including versions, restart consequence, model-container
   statement, release-notes area, primary update action and secondary release-page link.
2. Refused, covering a running job, running generation, missing updater bootstrap,
   unverifiable release and incompatible rollback window.
3. In progress, covering download, verification, expected disconnect, reconnect and a
   reload while the root updater is still working.
4. Success, automatic rollback and recovery-required outcomes with the version that is
   actually running.
5. Fleet context, covering the selected node, mixed-version maintenance state, required
   order and exact-version placement refusal.

The mockups must not choose component dimensions, colours or animation. Those remain the
implementation's use of `webui/ui/src/styles.css`.

### Multi-node fleet interaction

The privileged action remains local on every node. The controller never writes a remote
slot or sends a root command. It sends only an exact signed release identity and update
intent over the narrow fleet upgrade protocol; each node downloads, verifies, stages,
applies, health-checks and rolls back itself.

Until ADR 0016's maintenance and previous-version upgrade-status protocol is
implemented, one-click apply refuses on an enrolled fleet. A direct member-console
mutation would violate the managed-node rule in `docs/decisions/0016-multi-node-fleet.md:68-80`,
and an uncoordinated update would create version skew without a path to finish it.

When fleet support exists, a per-node update behaves as one step in a controller-owned
maintenance run:

1. Stop admission of new fleet jobs and membership changes.
2. Wait for all mutating jobs and generations on the selected nodes to finish.
3. Stage and independently verify the same target on every reachable node before the
   first swap.
4. Apply one node at a time: idle members first, then each distributed worker before its
   group head, one group at a time, and the controller last.
5. Keep heartbeat, update status and rollback available across current and immediately
   previous versions. Refuse placement and other fleet mutations while versions differ.
6. Leave maintenance only when every reachable enrolled node reports the exact target
   and local health. A node that was unreachable returns as version-skewed and remains
   ineligible until it is updated or explicitly removed.

The per-node action refuses:

- a local member-console request rather than a controller maintenance request;
- a different target from the fleet's staged signed release;
- a node with active mutating work or generation work;
- parallel node swaps;
- a group head before its selected workers;
- the controller before other reachable members;
- any new placement, reservation or membership mutation during version skew;
- a request to declare an unreachable node updated or healthy.

Existing model containers are not manager-updated. ADR 0016 already records that a
manager restart can briefly remove the console and `/v1` proxy while a container keeps
running (`docs/decisions/0016-multi-node-fleet.md:519-524`). Inference: a distributed
worker container can also remain running while its manager restarts, but that continuity
is not a product claim until the real two-node hardware test in the implementation plan
passes.

## Alternatives not chosen

### Polkit start authorization

An exact rule for user `basement`, verb `start` and unit
`basement-updater.service` can be made narrow. It still grants an explicit systemd
management action, needs rule installation and testing across target distributions, and
does not remove the need for fixed staging and root re-verification. The path unit has
fewer moving authorization parts.

### Sudo or setuid

`NoNewPrivileges=yes` deliberately prevents this shape, and granting a shell-shaped root
path is broader than a fixed oneshot transaction.

### Let the manager write its binary

Making `/usr/lib/basement` writable or granting capabilities would turn every manager
bug into a persistent system-binary write. The existing service boundary is kept.

### Systemd `OnFailure=` rollback

Process exit is not API health. The root updater already owns the bounded transition and
can verify both target and rollback versions directly.

### Checksum-only apply

It detects corruption, not independent publisher identity. It is not enough for an
automatic root install.

### Containerized or package-manager update

Basement is currently installed as one binary and one systemd service. Adding another
runtime or distribution repository is a larger delivery decision and does not remove
the need for signing, rollback compatibility and first-upgrade bootstrap.

## Consequences

- The manager remains unprivileged and obtains no general service-management power.
- The design does not claim to sandbox malicious manager code from the already trusted
  Docker daemon.
- A release-hosting compromise alone cannot produce an accepted root-installed binary.
- Update availability and update installability become different states. A latest tag is
  not installable until its signed manifest verifies and its compatibility list admits
  the running release.
- Existing machines need one manual root upgrade before the console action exists.
- A failed target causes bounded manager downtime and automatic code rollback, not a
  database rollback.
- Database evolution gains a release-by-release compatibility cost and cross-version
  qualification requirement.
- A manager restart leaves model containers alone but interrupts the console and `/v1`
  proxy. In-flight requests are not promised to survive.
- A fleet update temporarily reduces schedulable capacity and blocks placement during
  version skew.
- Updating the updater helper, units or trust key is intentionally manual in version 1.

## What changed from the previous ADR text

- Product, user, service and path names now use `basement`.
- The implemented cache is recorded as one hour, and the current link-only console
  behavior is stated exactly.
- The privilege decision now compares a path unit with polkit and chooses the path unit.
- The two new units and their allowed operations are fixed.
- A separate immutable root helper replaces the vague instruction to run the manager as
  root.
- The signature now covers an exact release manifest using Go's standard Ed25519
  implementation. Key custody is an explicit owner decision.
- Checksum trust and the additional property supplied by signing are stated separately.
- Health rollback is owned by the updater transaction, not systemd `OnFailure=`.
- Slots preserve the existing flat executable path as a compatibility symlink.
- Power-loss recovery uses a root-owned journal.
- The actual model-container and `/v1` behavior is grounded in the Docker and shutdown
  code.
- The current store is recorded as insufficient for rollback compatibility, with
  concrete schema rules and a reader-version contract.
- The first update-capable installation is stated as a manual root upgrade.
- The console has persisted interruption, refusal and rollback states and is explicitly
  mockup-gated.
- The per-node action is integrated with ADR 0016's exact-version fleet maintenance
  rules.
- `docs/plans/13-one-click-update.md` is the implementation plan. It supersedes the
  older `docs/plans/16-manager-self-update.md` where they differ.

## Sources

Repository sources are cited inline with file and line numbers. No external URL is cited
by this revision, so no external reachability claim is made.
