# Spec 10: the product is basement

Branch `spec/10-rename-basement`. Commit per section.

The owner's decision: every mention of "RunOnSpark", "RunOnSpark Manager", and
"run on spark" becomes **basement**. The brand is lowercase in prose, UI copy,
and filenames; capitalize only where a sentence starts. The public host is
already `basement.punkjazz.ai`.

## Naming table (authoritative — do not improvise)

| Was | Becomes |
|---|---|
| product name in any copy: RunOnSpark / RunOnSpark Manager | basement |
| manager binary `runonspark-manager` | `basement` |
| setup binary `runonspark-setup` | `basement-setup` |
| `cmd/runonspark-manager`, `cmd/runonspark-setup` | `cmd/basement`, `cmd/basement-setup` |
| systemd unit `runonspark(.service)` (verify actual name in `internal/setup/install.go`) | `basement.service` |
| service user/group `runonspark` | `basement` |
| server data dir (verify actual path in install.go) | same path with `basement` in place of `runonspark` |
| container labels `ai.runonspark.*` | `ai.basement.*` (see migration note below) |
| macOS app `RunOnSpark Setup.app` | `basement setup.app` (lowercase, deliberate) |
| bundle id `ai.runonspark.setup` | `ai.punkjazz.basement.setup` |
| zips `RunOnSpark-Setup-<os>-<arch>.zip` | `basement-setup-<os>-<arch>.zip` |
| env `RUNONSPARK_SIGN_KEY` | `BASEMENT_SIGN_KEY` |
| client config dir `os.UserConfigDir()/runonspark-manager` | `.../basement` (fall back to reading the old dir's known-users.json when the new one is absent; write only the new) |
| `recipe_by: RunOnSpark` in recipe yamls | `recipe_by: basement` |
| wizard/CLI copy ("RunOnSpark setup", "RunOnSpark Manager is running", …) | "basement setup", "basement is running", … |
| console sidebar wordmark (currently the R logo + "RunOnSpark") | the word `basement`, lowercase, using the console's existing type — remove the R logo image from the sidebar (leave the file on disk; recipe logos are untouched) |

**Unchanged, deliberately — do NOT rename these:**
- The Go module path `github.com/punkjazz-labs/basement` and every
  import path. It renames together with the GitHub repository, which is
  the owner's action, not this branch's.
- `releaseURL` in `internal/setup/install.go` (points at the real repo; moves
  with the repo rename). Same for any other literal GitHub URL.
- Filesystem paths in docs that describe the repo checkout location.
- Recipe IDs, artifact repositories, revisions, digests — pinned identifiers
  are sacred (conventions rule 3).
- Git history, CHANGELOG-style docs quoting old output, and the executor
  reports quoted inside docs/plans specs 01–09: specs are historical records;
  update only `00-conventions.md` (project description) and `06-deferred.md`
  (naming section: basement is now the name outright, drop "working name").

## Migration for existing installs (the dangerous part — test it)

`edgexpert-alpha` runs a live install under the old names. `setup` against a
machine with the old unit must ADOPT it, not orphan it:

1. In the install path (`internal/setup/install.go`), before installing the new
   unit: if the old unit exists (`systemctl list-unit-files` or the unit file
   path), stop and disable it, and if the old data dir exists and the new one
   does not, move it (`mv`) — the SQLite database, artifacts, receipts, and
   pairing state all carry over. Never copy-then-delete (double disk); never
   touch the dir when the new one already exists (report and keep going with
   the new one).
2. The old service user: rename with `usermod -l` + `groupmod -n` when present
   (preserves uid/gid, so moved files keep their owner); create fresh as today
   when absent.
3. Running containers carry old `ai.runonspark.*` labels and old
   `runonspark-<recipe>-v<N>`-style names (verify the actual scheme in
   `containerName`). Do NOT rename live containers. Keep reading BOTH label
   namespaces and container-name prefixes wherever the manager lists or matches
   its own containers (`ManagedContainers`, `containerName` lookups); write only
   the new ones for containers created from now on. A recipe update naturally
   retires the old-named container via the existing stop/remove machinery.
4. Unit tests over the fake runner for: fresh install (all new names), adopt
   (old unit + dir present → stopped, disabled, moved, renamed user), and
   mixed (new already present, old remnants → old left alone, reported).

## Acceptance

- `grep -ri runonspark --include='*.go' --include='*.tsx' --include='*.ts' --include='*.yaml' --include='*.sh' --include='*.html'`
  over the tree returns ONLY: import paths / module path, releaseURL and other
  real GitHub URLs, the compatibility label/name-prefix readers from the
  migration note, and the old-config-dir fallback. List every survivor in the
  report with its reason.
- Full verify suite green; `./scripts/release.sh` produces the renamed artifact
  set (list it); the terminal wizard transcript shows the new name; console
  builds and a screenshot shows the `basement` wordmark.
- Migration tests green, including `-race`.
