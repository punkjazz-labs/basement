# ADR 0008: Manager self-update — design

Status: design accepted; notification implemented, apply/rollback not yet built

## Implemented now

`GET /api/v1/update` compares the running version against the latest GitHub
release (6-hour cache) and the console shows an update banner linking to the
release. Applying an update remains an operator action: download the release
package and rerun `install.sh`, which is idempotent.

## Design for full self-update (future work)

1. **Signed releases.** Every release ships `runonspark-manager` (ARM64), a
   SHA-256 checksum file, and a minisign/ed25519 signature. The public key is
   embedded in the manager binary at build time; a release that does not
   verify is never applied.
2. **Privilege boundary.** The manager runs as the unprivileged `runonspark`
   user and cannot write `/usr/lib/runonspark-manager`. Updates are applied by
   a separate `runonspark-updater.service` (root, `Type=oneshot`) that the
   manager triggers; the updater re-verifies the signature itself and never
   accepts a payload path outside the manager's staging directory.
3. **Slots and rollback.** Binaries install into versioned slots
   (`/usr/lib/runonspark-manager/versions/<v>/`) with a `current` symlink.
   The updater flips the symlink and restarts the service; a systemd
   `OnFailure=` hook flips back to the previous slot if the new version fails
   its first health check. The two most recent slots are retained.
4. **State compatibility.** SQLite schema changes must be additive across one
   version so a rollback never meets a database it cannot read. The schema
   version table records the writing manager version.
5. **Separation rule (PRD §11.1).** Updating the manager stays separate from
   updating models or recipes; an update never mutates recipes, model
   artifacts, or containers.

Verification before shipping: kill-during-swap, corrupted download, wrong
signature, rollback-after-failed-health, and a schema round-trip test.
