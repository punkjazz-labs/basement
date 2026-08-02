# Spec 07: macOS and Windows installers

Branch `spec/07-installers`. Commit each section separately.

The website will offer three actions side by side: a macOS download, a Windows
download, and a curl one-liner. All three must land the user in the exact same
experience: the terminal setup wizard (`runonspark-manager setup` in
`cmd/runonspark-manager/setup.go`). That flow is the product — discovery listing,
machine choice, username prompt, listen choice, progress lines, the summary card
with console URL and pairing token. This spec is packaging and platform
affordances only. **The setup flow itself must not change**: same prompts, same
copy, same order, no GUI wrapper, no divergent "installer edition".

## A. Release build script

**Problem.** The only way to produce binaries today is a manual `go build` per
target. There is no reproducible artifact set to put behind download buttons.

**Change.**
1. `scripts/release.sh`, plain bash, runnable on macOS. Build matrix, all with
   `CGO_ENABLED=0` and `-trimpath`:
   - `linux/arm64` (the GB10 machines)
   - `darwin/arm64`, `darwin/amd64`
   - `windows/amd64`, `windows/arm64`
2. Output layout: `dist/<goos>-<goarch>/runonspark-manager` (`.exe` on windows),
   plus a `dist/SHA256SUMS` covering every artifact this spec produces. `dist/`
   goes into `.gitignore`.
3. Version stamping. **Investigate first**: find how `cfg.Version` is populated
   (start at `internal/config`) and whether an ldflags `-X` hook already exists.
   Wire the script to stamp the version as `git describe --tags --always --dirty`.
   If there is no user-visible way to print the version, add a
   `runonspark-manager version` subcommand that prints it and exits; nothing
   fancier. Record findings in the report.

**Acceptance.** Running `scripts/release.sh` on the build Mac produces all five
targets plus `SHA256SUMS`; the report lists each artifact with `file` output
(architecture check) and size. `go build ./... && go vet ./... && go test ./...`
green.

## B. macOS double-click artifact

**Problem.** A downloaded bare binary is not double-clickable, and macOS users
should not need to know what a terminal is before the wizard opens one for them.

**Change.**
1. The release script stages `dist/RunOnSpark-Setup-macos-<arch>.zip` containing:
   - the darwin binary, and
   - `RunOnSpark Setup.command` — a small shell script that `cd`s to its own
     directory (`"$(dirname "$0")"`) and execs `./runonspark-manager setup`.
   Double-clicking the `.command` opens Terminal and runs the wizard; that is
   the entire macOS installer.
2. Code signing and notarization run only when the environment provides
   `CODESIGN_IDENTITY` (and, for notarization, `NOTARY_PROFILE`); when unset the
   script prints exactly what was skipped and continues. Do not fabricate
   identities or entitlements; unsigned artifacts are the honest current state
   (Gatekeeper requires right-click → Open; note this in the report).

**Acceptance.** The zip exists and unzipping + double-clicking the `.command`
on the build Mac opens Terminal and reaches the setup wizard's first output
(then Ctrl-C). Report includes the transcript of
`dist/darwin-arm64/runonspark-manager setup` reaching the discovery listing (or
its no-machines error) on the build Mac.

## C. Windows double-click behavior

**Problem.** A Go console binary launched by double-click opens its own console
window — and that window vanishes the instant the process exits, taking the
summary card (console URL, pairing token) or any error message with it. That
breaks the "same exact UX" promise on the platform most likely to double-click.

**Change.**
1. Windows-only, build-tagged file in `cmd/runonspark-manager`: when the process
   is the sole owner of its console (the double-click case — investigate the
   standard detection via `GetConsoleProcessList` returning 1; check whether
   `golang.org/x/sys` is already in go.mod before adding it, and record), the
   program waits for Enter before exiting, on success and on failure alike, with
   the line `Press Enter to close this window.` When launched from an existing
   cmd or PowerShell session, behavior is unchanged — no pause.
2. The release script stages `dist/RunOnSpark-Setup-windows-<arch>.zip`
   containing the `.exe`. No signing (no certificate exists); record the
   SmartScreen consequence in the report.
3. No behavior change of any kind on darwin or linux.

**Acceptance.** `GOOS=windows GOARCH=amd64 go build ./...` compiles. The pause
logic is isolated enough to unit test its decision function where feasible; the
double-click behavior itself gets documented manual test steps in the report
(the owner runs them on a Windows machine — do not claim they passed).

## Out of scope, record in report

- Hosting and download URLs. `internal/setup/install.go` `releaseURL` points at
  this repo's GitHub latest release; while the repo is private that URL cannot
  serve anonymous downloads. Do not change the code for this; state it in the
  report so the distribution decision (GitHub release vs basement.punkjazz.ai)
  is made deliberately.
- CI. Do not touch `.github/`.
- The curl install script itself (lives with the website).
