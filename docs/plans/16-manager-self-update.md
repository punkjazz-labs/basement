# Spec 16: one-click manager update

Four phases, branches `spec/16a-release-signing` … `spec/16d-fleet-order`. Phase A is a
prerequisite for B; C is the console; D needs a decision record before any code.

Read first: `docs/decisions/0008-manager-self-update-design.md`, and
`docs/plans/14-release-notes.md` (the same GitHub release is the source for both).

## Problem

`GET /api/v1/update` tells the owner a new version exists. Applying it means going back
to a terminal, downloading a release binary, and rerunning the installer as root. For
the target persona (the small firm, the lawyer, the person who bought a Spark and not a
sysadmin), that is where the product ends.

The service cannot update itself as things stand, and that is deliberate.
`packaging/systemd/basement.service` runs the manager as the unprivileged `basement`
user with `ProtectSystem=strict`, `ReadWritePaths=/var/lib/basement`, an empty
`CapabilityBoundingSet`, and `NoNewPrivileges=yes`. It cannot write
`/usr/lib/basement/basement` and it cannot call `systemctl`. `NoNewPrivileges=yes` also
means `sudo` cannot work from inside the unit, so the sudoers route that a normal daemon
would take is closed by construction. Every privilege escalation in the codebase today
lives in the interactive wizard (`internal/setup/runner.go` `RunPrivileged`), running as
the human's own account.

That confinement is worth keeping. This spec adds a privileged path that is narrow
enough to be read in one sitting.

## User-visible outcome

The update dialog from spec 14 grows a primary action, `Install update`. The console
shows the download, the swap, and the manager coming back. If the new version does not
answer its own health check, the old one is put back automatically and the console says
what happened and which version is running.

## Phase A: releases are signed, not just checksummed

`.github/workflows/release.yml` publishes `basement-linux-arm64` next to
`basement-linux-arm64.sha256`. Both files come out of the same workflow run and are
served from the same place, so the checksum proves the download was not corrupted in
transit and nothing else. It is not evidence about who built the binary. Today that is
fine, because applying an update is a human with a terminal. The moment a root oneshot
applies whatever that URL serves, the checksum is no longer the right gate: a compromise
of the release path becomes root on every Spark.

So the binary is signed, and the signature is what the updater trusts.

1. Reuse the machinery that already exists for the recipe index rather than adding a new
   one. `internal/recipe/signature.go` has `VerifySignature` over raw ed25519 with a
   base64 public key constant, and `cmd/sign-index` signs with a private key read from a
   file path in `BASEMENT_SIGN_KEY`. Generalise: move the verify half into a small
   `internal/signing` package that both the recipe index and the release binary use,
   keeping `recipe.VerifySignature` as a thin wrapper so no recipe test changes.
2. A second key, not the index key. Release signing and recipe signing must fail
   independently. New constant `ReleasePublicKeyBase64` with the same honesty comment
   the index key carries if it is still a placeholder.
3. Signing happens where the macOS signing already happens: on the owner's laptop, in
   `packaging/sign-macos-release.sh`, which already downloads release assets, modifies
   them, and re-uploads with `--clobber`. Extend it (or add a sibling
   `packaging/sign-release-binaries.sh` called from it) to produce and upload
   `basement-linux-arm64.sig` for the tag. The private key never enters CI, the repo, or
   argv, exactly as `make sign-index` established.
4. Key ceremony: this needs a decision record before the real key is generated. ADR 0009
   already names a missing "decision record fixing the feed key ceremony"; write one ADR
   covering both keys (generation, storage, rotation, what happens when a key is lost)
   rather than two.

**Tests.** The generalised verify package keeps every existing signature test passing
unchanged; a release-signature round trip (sign with a test key, verify, then tamper one
byte and reject). The signing script is not unit-testable; the report documents the
manual run and its output.

## Phase B: the privileged path

Three files and one subcommand. Nothing else gains privilege.

1. **Staging, by the manager, unprivileged.** New code in `internal/httpapi` plus a
   small `internal/selfupdate` package:
   - Resolve the asset URLs from the release the update check already found.
   - Stream `basement-linux-arm64`, `.sha256` and `.sig` into
     `/var/lib/basement/updates/<version>/`, which is inside the unit's only
     `ReadWritePaths`. Reuse the streaming-with-digest pattern from
     `internal/operations/huggingface.go` (`fileDigest`, `hashReader`, `verifyFile`)
     rather than reading the whole binary into memory.
   - Verify sha256, verify the ed25519 signature with the embedded public key, and run
     the ELF guard that already exists, `internal/setup/install.go` `validateARM64ELF`
     (magic plus `e_machine == 183`). All three, in that order, before anything is
     handed on.
   - Write `/var/lib/basement/updates/request.json` last, atomically
     (`writeFileAtomic` in `internal/recipefeed/fetch.go` is the pattern), carrying
     `{version, sha256, staged_path, requested_at}`.
2. **The trigger, with no privilege at all.** `packaging/systemd/basement-updater.path`,
   `PathExists=/var/lib/basement/updates/request.json`, `Unit=basement-updater.service`,
   `WantedBy=multi-user.target`. A path unit run by the system manager watches a file the
   manager is already allowed to write. The manager therefore needs no capability, no
   polkit rule, and no D-Bus access to start an update, and there is no API through
   which anything can ask the updater to do something other than read that one file.
3. **The updater.** `packaging/systemd/basement-updater.service`, `Type=oneshot`,
   root, `ExecStart=/usr/lib/basement/current/basement apply-update`. It is the
   *currently installed* binary that applies the update, so the code doing the applying
   is code that was already trusted on this machine.
   `apply-update` (new subcommand in `cmd/basement/`, hidden from help):
   - refuses to run unless euid is 0 and unless the staged path is inside
     `/var/lib/basement/updates/`, resolved with symlinks followed, no exceptions and no
     configuration knob;
   - re-verifies sha256, signature, and ELF itself. The manager already verified; the
     updater verifies again because it is the one with privilege and it does not trust
     the process that asked;
   - installs to `/usr/lib/basement/versions/<version>/basement`, mode 0755, root-owned;
   - flips `/usr/lib/basement/current` to that directory with an atomic rename of a
     temporary symlink;
   - `systemctl restart basement.service`;
   - polls `http://127.0.0.1:<listen port>/api/v1/health` until it answers `status: ok`
     with the expected version, up to 60 seconds;
   - on failure, flips `current` back, restarts again, and writes the outcome;
   - writes `/var/lib/basement/updates/result.json`
     (`{version, state, message, finished_at}`) and deletes `request.json` in every
     path, including failure. A request file that outlives its attempt is an update that
     runs again on the next boot;
   - keeps the two most recent version directories and removes older ones.
4. **The unit moves to the symlink.** `ExecStart` becomes
   `/usr/lib/basement/current/basement`. This touches
   `packaging/systemd/basement.service` **and** `internal/setup/assets/basement.service`,
   which a test asserts are byte identical, and `packaging/install.sh` plus
   `internal/setup/install.go`, which install into `/usr/lib/basement/basement`. Both
   installers must create `versions/<version>/` and the `current` symlink, and must
   adopt an existing flat install (binary at the old path) the way
   `adoptLegacyInstall` already adopts the pre-rename layout. An install that leaves a
   Spark unbootable is the worst outcome in this spec; the adoption path gets its own
   test.

**Do not** use systemd's `OnFailure=` as the rollback trigger, despite ADR 0008 point 3.
`OnFailure=` fires when the process exits non-zero. A new manager that starts, binds the
port, and answers nothing useful never trips it. The updater's own health poll is the
gate, and the deviation goes in the ADR as an amendment, not silently in code.

**Tests.** `apply-update` is where the risk is, so it takes the harness treatment: a
fake root filesystem under `t.TempDir()`, an injected command runner for `systemctl`
(the `internal/setup` `Runner` interface is the existing idiom), and an injected health
prober. Cases: happy path leaves `current` pointing at the new version and a success
result; a health check that never passes leaves `current` on the old version, the
service restarted twice, and a result explaining it; a staged path outside the updates
directory is refused before any file is touched; a tampered binary fails signature and
is refused; a symlink pointing outside the directory is refused; a kill between install
and symlink flip leaves the old version serving; the request file is removed in all of
the above. Plus a store test that an older schema-writing binary can open a database a
newer one wrote (ADR 0008 point 4), noting in the report that `schema_meta` exists in
`internal/store/store.go` and is currently never read.

**Hardware runbook (for the owner, not the executor).** Real update, real rollback (stage a
deliberately broken binary), and a reboot with a stale `request.json` present.

## Phase C: the console

Mockup-gated only if it goes beyond spec 14's dialog. Preferred shape, which is an
incremental edit and therefore not gated: the same dialog gains a primary
`Install update` next to the existing ghost `Open release page`, and the dialog body
becomes a small progress view while the update runs.

1. `POST /api/v1/update/apply`, console session plus CSRF like every other mutation,
   never a bearer key. It stages, verifies, and writes the request file, then returns.
   409 with a plain sentence when a job is running in the engine: restarting the manager
   mid-install is not a thing to do quietly.
2. `GET /api/v1/update/status` reports staging progress from the manager and, after the
   restart, reads `result.json`. The console will lose its connection during the
   restart; the poll must treat "not answering" as expected for up to 90 seconds and say
   `Restarting the manager` rather than showing a connection error.
3. Copy, honest about the two outcomes: on success, `Running v0.9.4.` On rollback,
   `v0.9.4 did not come up, so v0.9.3 was put back. Nothing else changed.` Never
   apologise, never imply the owner did something wrong.
4. The separation rule from ADR 0008 point 5 shows up here too: nothing in this flow
   touches models, recipes, containers, or artifacts, and the dialog says so in one
   line.

**Acceptance.** Mock harness screenshots: idle dialog with the new action, staging,
restarting, success, rollback. Typecheck, build, committed assets regenerated.

## Phase D: fleet ordering

Not buildable yet, and the reason is a recorded decision, not missing code. ADR 0013
scopes a peer's bearer key to preflight and install only: "Start, stop, smoke-test,
benchmark and remove stay console-session-only." Restarting the peer's manager is a
strictly larger power than any of those. Updating the fleet from one console needs an
ADR that decides whether that power belongs to a peer key, and it should be written
alongside the key ceremony ADR from phase A.

The ordering question, so the ADR has the argument in front of it:

- **Workers first, head last.** During the window the fleet runs a new worker and an old
  head. The head is the machine that drives delegated steps and the machine the owner is
  looking at, so it changes last and its version is the one the console reports. If the
  new worker refuses something the old head sends, the owner sees a failure on the
  console they are already using, with the head still on the version that worked
  yesterday.
- **Head first** puts new code in charge of old workers. That is the direction which is
  easier to make compatible deliberately (new code can know about old peers), but the
  failure lands on a machine the owner is not looking at.
- Either way: one machine at a time, never in parallel, never while a job is running on
  either machine, and the fleet view must show mixed versions plainly rather than
  hiding the window.

The internal node API (`internal/httpapi/node.go`) is the surface that has to survive a
version skew in both directions. Whatever the ADR decides, it should also decide how
long a skew is supported, because "one release" and "forever" are very different tests.

## Open questions (owner)

- Is a real release-signing key going to exist, and who holds it? Phase B is not safe to
  ship on sha256 alone, and phase A's key is the whole basis of that safety.
- Should the update be applied automatically when the owner opted in, or always by a
  click? This spec assumes always by a click.
- The listen port the updater health-checks comes from the unit's `--listen` flag, which
  a drop-in (`/etc/systemd/system/basement.service.d/listen.conf`) can override. Should
  `apply-update` parse the effective unit, or should the manager write the address it is
  actually bound to into `request.json`? The second is simpler and is what this spec
  assumes; confirm.
