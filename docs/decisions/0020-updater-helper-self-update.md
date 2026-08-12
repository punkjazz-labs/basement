# ADR 0020: Updating the updater through the signed chain

Date: 2026-08-12. Status: proposed. Design only.

This ADR extends ADR 0008. It supersedes exactly two of its prohibitions: that the root
helper may not replace its own binary or its units, and that updating the helper and the
units is intentionally manual in version 1. Everything else in ADR 0008 stands, including
the manager's confinement, the path-unit privilege boundary, the health-gated
transaction, and the rule that the key ring is never data.

## Context

The signed one-click path installs exactly one thing. Manifest schema 1 names a single
asset, `basement-linux-arm64`, and carries its size and digest
(`internal/update/manifest.go:36-47`). Decoding is strict, and the compiled-in
`UpdaterProtocol` constant is refused on mismatch (`internal/update/manifest.go:18-24,233`).
That refusal is the negotiation lever this ADR uses.

Everything needed to sign a second asset is already present:

- The release workflow builds the helper with the same embedded key ring as the manager
  (`.github/workflows/release.yml:44-52`), so both sides verify with the same public keys.
- `packaging/sign-linux-update.sh:31-35` already downloads `basement-updater-linux-arm64`
  into the signing work directory and then uses it for nothing. The helper ships with a
  `.sha256` only, which is HTTPS trust, not publisher authentication.

What is missing is identity and reach:

- The helper is 25 lines with one verb (`cmd/basement-updater/main.go`). It has no
  `version` subcommand and no version ldflag, so nothing on a machine can say which helper
  build is installed. `Stager.checkBootstrap` only stats five paths
  (`internal/update/stager.go:385-402`).
- Installers overwrite the helper and both units unconditionally, fetching the helper from
  the latest release over HTTPS with a checksum, no signature and no version comparison
  (`internal/setup/install.go`, `packaging/install.sh`). There is no rollback copy of the
  helper.

The helper's own unit already permits the swap. `ReadWritePaths` includes
`/usr/lib/basement` (`packaging/systemd/basement-updater.service:34`), and the helper
binary lives at `/usr/lib/basement/updater/basement-updater`, inside its own writable set.
`/etc/systemd/system` is not writable. `UMask=0077` strips group and other bits from every
file the helper creates (`packaging/systemd/basement-updater.service:15`), a lesson already
paid for on hardware: every new file needs an explicit `Chmod`.
`MemoryDenyWriteExecute=yes` is set (line 32), and `systemctl` works from the helper over
AF_UNIX as uid 0, proven on hardware.

The unit texts are embedded byte-identical in every signed manager binary
(`internal/setup/assets/`, parity test `internal/setup/setup_test.go:363-380`), with
`packaging/systemd/` as the source of truth. The helper can embed the same texts from the
same commit by the same mechanism.

## Decision

### 1. Updater protocol 2 and manifest schema 2

Schema 2 adds three fields and changes nothing existing:

```json
{
  "helper_asset_name": "basement-updater-linux-arm64",
  "helper_size": 0,
  "helper_sha256": "64 lowercase hexadecimal characters"
}
```

`UpdaterProtocol` becomes 2. A protocol-1 manager refuses a schema-2 manifest, both on the
schema constant and on strict unknown-field decoding. That refusal is correct and is the
transition mechanism, not a bug to work around.

A protocol-2 manager accepts schema 1 and schema 2. The single-constant equality check at
`internal/update/manifest.go:233` becomes a supported-set check. A schema-1 manifest simply
carries no helper digest, so helper staleness is not evaluated for that release.

Dual publishing was considered and rejected. A second manifest asset under a second fixed
name would mean two signatures per release that can silently diverge and a per-protocol
selection rule that never goes away, and it buys nothing the existing multi-hop resolver
does not already provide. The chosen shape is one signed manifest per release and a staged
transition, described under Transition plan. The installer remains the fallback for any
machine outside the reachable window.

### 2. Release chain

The workflow stamps the helper with `-X main.version=${GITHUB_REF_NAME}` exactly as it
stamps the manager (`.github/workflows/release.yml:38`), so a helper build is nameable.

`packaging/sign-linux-update.sh` passes the already-downloaded helper to
`cmd/sign-update-manifest`, which measures its size and digest and writes them into the
signed manifest. The verify passes measure the helper again. One signature still covers the
exact manifest bytes, so the helper is authenticated by the same key as the manager with no
new cryptography and no second trust root.

The helper gains a `version` subcommand. It prints its embedded version and the SHA-256 of
`/proc/self/exe`, so it reports the bytes actually executing rather than the bytes at its
path. It takes no lock, writes nothing, reads no state directory, and requires no
privilege.

### 3. Staleness detection in the manager

The installed helper is root-owned and world-readable at mode 0755, so the unprivileged
manager can hash it. When the resolved candidate carries a schema-2 manifest, the stager
compares that hash with `helper_sha256`.

When they differ, the stager downloads `helper_asset_name` from the same release under the
same bounded-download rules as the manager payload, enforces the signed `helper_size` and
`helper_sha256`, applies the same Linux ARM64 ELF format check, and stages the bytes into
`pending/` at mode 0750 with an explicit `Chmod`. It records `helper_sha256` in a schema-2
`request.json`. When they match, no helper bytes are staged and the request stays otherwise
identical.

A helper file that cannot be read or hashed is reported as unknown, never as stale. Failing
to read the installed helper must not cause a helper download.

`checkBootstrap` gains one non-fatal step: it executes `basement-updater version` and
records the result. A helper that will not run is surfaced as a warning in update status.
It never blocks an update, because the update is the repair.

### 4. Helper self-replacement

The helper swaps itself only after the manager transaction reaches `target_healthy`. Never
before, never on any rollback path, never at boot recovery.

The sequence is: verify the staged helper bytes against the signed manifest's
`helper_sha256` using the helper's own key ring; write `updater/basement-updater.next` with
an explicit `Chmod` to 0755, because `UMask=0077` would otherwise leave it root-only; copy
the current binary to `updater/basement-updater.previous`, also 0755; fsync both files and
the directory; rename `.next` over `updater/basement-updater`.

Rename over the path of a running executable is safe on Linux. The running process holds
its inode and keeps executing the old bytes until it exits. The live path is never opened
for writing, which is what would return `ETXTBSY`. `MemoryDenyWriteExecute` is not violated,
because the helper writes a file and never maps the new bytes executable in this run.

The helper swap never fails the manager update. The receipt gains an optional
`helper_state` field with the values `updated`, `unchanged`, and `swap_failed:<reason>`. A
failed swap leaves the previous helper in place and the manager update recorded as
succeeded, which is the honest outcome: the manager did become healthy.

`.previous` is retained until the next successful swap. It is a forensic and manual-repair
copy, not an automatic rollback: nothing reverts a bad helper on its own.

### 5. Units, generation 2

The helper embeds `basement-updater.service`, `basement-updater.path` and `basement.service`
from its build commit, using the same embedding and parity test as
`internal/setup/assets/`.

It reconciles those three unit files only when its own unit's `ReadWritePaths` includes
`/etc/systemd/system`. Detection is a probe: create and immediately remove a fixed-name
temporary file in that directory. A failed probe means generation 1, is not an error, and
is recorded in the receipt as `units: not_permitted`. Reading the unit text states intent;
only the probe states truth, because drop-ins can change the effective sandbox.

When permitted, each unit whose bytes differ from the embedded text is copied to a
`.previous` sibling, then written as a temporary file with an explicit `Chmod` to 0644 and
renamed into place. A single `systemctl daemon-reload` follows. `fixedSystemctl`
(`internal/update/updater.go:78-84`) is generalized to take a unit argument and to stop
hard-coding `basement.service` in its error string (line 81); `daemon-reload` becomes a
one-line addition on top of that.

The chicken-and-egg is real and is stated plainly: the unit that grants
`/etc/systemd/system` access can only arrive through one more installer run, because no
currently installed helper can write it. Generation 1, the helper binary swap, needs no unit
change at all and therefore ships immediately. That is where most of the value is: helper
logic changes with releases, unit texts rarely change.

### 6. Receipt and journal schema

Receipts are read across a version boundary in both directions. After a success the new
manager reads a receipt written during the old manager's transaction. After a rollback the
old manager reads a receipt written by a newer helper. The second case is the hazard.

The rule: the helper writes the receipt at the schema version of the request it is serving.
A schema-1 request produces a schema-1 receipt with no `helper_state`, even when the helper
is protocol 2 and did swap itself. Every schema-1 receipt field keeps its meaning
unchanged, and schema 2 adds only optional fields. Every manager reads both schemas.

The visible cost is a blind spot: in the mixed case the manager cannot learn `helper_state`
from the receipt. The `basement-updater version` smoke check from decision 3 covers it,
which is the second reason that subcommand exists.

The private journal at `/var/lib/basement-updater/journal.json` is read only by the helper,
so it advances to schema 2 unconditionally with an explicit migration rule: a schema-1
journal found at boot recovery is interpreted with schema-1 semantics and completed, never
upgraded in place mid-transaction. Both sides keep `DisallowUnknownFields`, so schema
evolution stays explicit.

### 7. Scope guard

The helper still may not touch the key ring as data, the listen drop-in, or any service
other than `basement.service`. ADR 0008's prohibition bullet is replaced by exactly:

> may replace its own binary and reconcile its own units and `basement.service` from its
> embedded, release-signed texts

One consequence must be said out loud. A new helper binary carries whatever key ring was
compiled into it at that release, so key rotation now flows through the helper swap. The
ring is still never data and still never comes from a manifest, an API or a file. The swap
that installs a new ring is itself authorized by the old ring verifying the manifest.
Rotation must therefore publish an overlap ring containing both the old and the new key for
at least one release. Publishing a helper whose ring drops the current key in the same
release that introduces the new key breaks the chain for any machine that has not yet
swapped.

### 8. Hardware verification

None of this can be verified now; the fleet is reserved. The acceptance steps are listed
below and are a precondition for moving this ADR to accepted.

## What the update can now change, and what it still cannot

It can now change: the manager binary; the root helper binary, including the helper's
verification logic and its embedded key ring; and, at generation 2, the three unit files
`basement-updater.service`, `basement-updater.path` and `basement.service`.

It still cannot change: the key ring as data, in any file, API, environment value or
manifest field; the listen drop-in written at install time; any service other than
`basement.service`; the database, recipes, artifacts, images, generated model configuration
or containers; or anything named by the request rather than compiled in. The helper still
downloads nothing and contacts no nonlocal address. It still executes nothing from the
manager-writable staging directory.

## Failure modes and honest limits

A new helper's health is only proven on its next run. The swap happens after the manager is
healthy, so a broken helper is not detected during the transaction that installed it. The
mitigations are partial and are all of them: the bytes are signature-verified before the
rename; the manager runs `basement-updater version` as a smoke check and warns when the
binary will not run; `.previous` is kept; and the repair paths are an installer run or the
next release's swap. There is no automatic helper rollback, and this ADR does not claim one.

A helper that runs but misbehaves is worse than one that will not run, and the smoke check
does not catch it. The blast radius is bounded by the unit sandbox, which a generation-1
helper cannot widen, and by the fact that a helper refusing every request leaves the machine
on its current healthy manager.

If the explicit `Chmod` on the new helper is skipped or fails, `UMask=0077` leaves the
binary at mode 0700. Root still executes it, so updates continue, but the manager can no
longer hash it and staleness detection silently degrades to unknown. This is exactly the
mode failure already seen on hardware, and it is why the chmod is spelled out at every
write site in this ADR.

The probe write for generation 2 creates a file in `/etc/systemd/system`. If the process
dies between create and remove, a stray zero-byte file remains. It uses a fixed name so the
next run removes it, and systemd ignores a file without a unit suffix.

A unit reconcile that writes a bad unit is repaired by `.previous` plus a manual
`daemon-reload`, which requires console or SSH access to the node. `daemon-reload` failure
after a successful write leaves new unit text on disk and old text in systemd's memory until
the next reload or boot; this is recorded in the receipt rather than treated as a rollback
trigger.

Fleet rolling upgrades distribute manifest bytes, signature bytes and the asset URL
verbatim, and every node re-verifies independently. The controller must distribute the
helper asset URL explicitly rather than deriving it by editing the manager asset URL. A
wrong helper URL fails closed on the signed `helper_sha256` at every node.

If the manager update rolls back, the staged helper bytes are discarded with the consumed
request. Nothing is left behind in `pending/` for a later transaction to pick up.

A helper swap followed by a boot-recovery rollback of the manager leaves a new helper
serving an old manager. This is supported, and is the case decision 6 exists to make safe.

## Transition plan

Release N ships a manager that speaks protocol 2 and accepts manifest schemas 1 and 2, and
a helper that speaks protocol 2, under a schema-1 manifest. A protocol-1 manager verifies
that schema-1 manifest and installs it through the existing one-click path. No installer
run, no dual publishing.

Release N+1 is the first schema-2 manifest and the first release whose helper digest is
signed. Managers from release N read it and evaluate helper staleness. Managers older than
N refuse it, and ADR 0008's resolver falls back to the newest release they can accept, which
is release N. The multi-hop rule then carries them to N and, on the next check, to N+1. The
resolver must skip a refused manifest and keep resolving; a refused schema must never abort
the whole check.

Release N must stay published permanently as the stepping stone, and its signed
`rollback_from` window defines how far behind a machine can be and still reach protocol 2 in
one click. A machine outside that window uses the installer, which is the same answer ADR
0008 already gives.

Generation 2 needs one more installer run, and only one, to deliver a
`basement-updater.service` whose `ReadWritePaths` includes `/etc/systemd/system`. Until then
every machine is at generation 1 and gets helper binary updates but not unit reconciliation.
The console must say `Run the installer once to enable unit updates`, in the same voice as
the existing bootstrap message, and must not imply that the console can grant that access
itself.

The installers keep overwriting the helper and the units unconditionally. They remain the
recovery path for a broken helper and must not delete a `.previous` file.

Amended 2026-08-13, found during implementation: a machine can reach a protocol-2 manager
through the signed chain while still running the installer's older helper, and that helper's
strict decoder rejects a schema-2 `request.json` as unknown fields, which would fail a
manager update over the one component this design says must never fail one. The handoff
schema is therefore chosen per machine: the stager asks the installed helper
`basement-updater version` first and writes a schema-2 request only when the helper reports
protocol 2 or newer; otherwise it writes an unchanged schema-1 handoff, the manager release
applies normally, and that machine reaches helper self-update after one installer run. The
"no installer run" property of release N holds for the manager update itself, not for
gaining helper self-update on machines whose helper predates protocol 2.

## Superseded decisions

ADR 0008 lists among the things the updater service may not do
(`docs/decisions/0008-manager-self-update-design.md:266`):

> - replace its own helper, its units, the manager unit, a listen drop-in or a public key;

That bullet is replaced by:

> - replace the manager unit's listen drop-in or a public key as data; it may replace its
>   own binary and reconcile its own units and `basement.service` from its embedded,
>   release-signed texts.

ADR 0008 records as a consequence (`docs/decisions/0008-manager-self-update-design.md:588`):

> - Updating the updater helper, units or trust key is intentionally manual in version 1.

That bullet is replaced by:

> - The updater helper is updated through the signed chain from protocol 2 onward. Unit
>   reconciliation requires one further installer run to widen the helper's sandbox. A trust
>   key rolls forward with the helper binary and requires an overlap release.

The surrounding paragraph at
`docs/decisions/0008-manager-self-update-design.md:207-211` is narrowed the same way: a
release-key rotation or updater-unit change no longer requires a manual installer upgrade
in every case, but an overlap-signed transition is still mandatory for a key, and a sandbox
widening is still a one-time installer step.

ADR 0008 is not edited. The supersession lives here.

## Hardware acceptance checklist

Run on a reserved node, in this order, recording the receipt and journal after each step.

1. Baseline. Install release N through the installer. Confirm `basement-updater version`
   prints the release tag and a digest matching `sha256sum` of the installed path, and that
   the manager surfaces the helper version in update status.
2. Helper staleness with no change. Offer a schema-2 release whose `helper_sha256` matches
   the installed helper. Confirm no helper bytes are staged, the request is schema 2, and
   the receipt reports `helper_state: unchanged`.
3. Helper swap. Offer a schema-2 release whose helper differs. Confirm the helper is staged
   at 0750, the manager reaches `target_healthy` first, the live helper is then 0755
   root-owned with the new digest, `.previous` holds the old digest, and the receipt reports
   `helper_state: updated`. Confirm `basement-updater version` reports the new version on
   its next run.
4. Rollback leaves the helper untouched. Force the target manager to fail its health check.
   Confirm the transaction rolls back, the installed helper digest is unchanged, no
   `.previous` was written, no staged helper bytes remain under `pending/`, and the receipt
   carries no `helper_state`.
5. Mixed-schema receipt. From a schema-1 manager, apply a release through a protocol-2
   helper. Confirm the receipt is schema 1, contains no `helper_state`, and renders without
   error in the older console.
6. Generation 1 unit probe. Confirm the probe fails on the current unit, leaves no file in
   `/etc/systemd/system`, and is recorded as `units: not_permitted` without failing the
   update.
7. Generation 2 unit reconcile. Install the widened unit through one installer run. Offer a
   release whose embedded unit texts differ. Confirm `.previous` files are written, the new
   units are 0644, `daemon-reload` succeeds, and both `basement.service` and the updater
   units still start.
8. Tamper. Corrupt one byte of the staged helper before the swap. Confirm the helper refuses
   the swap on `helper_sha256`, the manager update still reports success, and the receipt
   reports `swap_failed`.
9. Interruption. Power-cut the node between `target_healthy` and the rename. Confirm boot
   recovery leaves a consistent helper at either the old or the new digest, never a partial
   file, and never a `.next` file at the live path.

## Sources

Repository sources are cited inline with file and line numbers. No external URL is cited, so
no external reachability claim is made. Nothing in this ADR has been observed on hardware
yet, which is what the checklist above is for.
