# Spec 08: native installers — the wizard without a terminal

Branch `spec/08-native-installers`. Commit each section separately.

the owner's requirement, verbatim intent: double-clicking the downloaded artifact must
never show a terminal. The wizard opens as a page in the user's default browser,
served by a loopback-only server inside the installer binary. No GUI framework, no
cgo, no new Go dependencies: a minimal macOS `.app` bundle launches a plain Go
binary without Terminal, and a Windows binary built with `-H=windowsgui` has no
console window at all. The terminal wizard (`runonspark-manager setup`) remains
for the curl path, byte-for-byte unchanged.

## A. Extract the wizard flow behind an interface

**Problem.** The wizard orchestration (discovery listing, machine choice, the
non-GB10 warning, username prompt with remembered users, listen choice, progress
lines, summary card) lives as terminal I/O inside `cmd/runonspark-manager/setup.go`.
The engine underneath (`setup.Install`, `DialSSH`, `Probe`, `discovery.Discover`)
is already UI-free.

**Change.** Move the flow into `internal/setup` (new `wizard.go`) driven by an
interface the flow calls outward:

```go
type WizardUI interface {
    Prompter                      // Password, Confirm — unchanged, SSH needs them mid-flow
    ChooseMachine(candidates []discovery.Candidate) (int, error)
    ConfirmNonGB10(name string) (bool, error)
    AskUsername(target, suggested string) (string, error)
    ChooseListen(remote bool) (ListenMode, error)
    Progress(line string)
    Summary(result InstallResult)
}
```

`cmd/runonspark-manager/setup.go` becomes the terminal implementation of that
interface plus flag parsing. Every prompt string, ordering, default, and color
stays exactly as it is — the acceptance check is a before/after transcript of
`setup` reaching the machine list, identical modulo timing. The remembered-users
store moves with the flow (it is flow logic, not terminal logic).

## B. The browser wizard

**New binary** `cmd/runonspark-setup`: binds `127.0.0.1:0`, generates a single-use
URL token, opens the default browser at `http://127.0.0.1:<port>/setup/<token>`,
and serves a WizardUI implementation over HTTP to that page. When the flow
finishes, the summary screen links to the installed machine's console URL.

Security, non-negotiable (line-by-line review):
- Bind loopback only. Random port. The token is ≥128 bits from crypto/rand,
  required on every request, constant-time compared; one wizard run per process.
- Reject requests whose Host is not the bound loopback address, and any request
  with an Origin header that is not the wizard's own origin (browsers send none
  for direct navigation; a foreign origin means a malicious web page probing
  localhost).
- The SSH password and sudo password travel only in POST bodies over loopback,
  are never placed in URLs, never logged, never echoed back in any response, and
  are held only as long as `DialSSH`/`Install` need them (the existing Prompter
  contract).
- No directory listings, no serving files outside the embedded wizard assets.

UI: one page, console design system (dark, pill buttons, mono for commands and
tokens, no emoji). Steps mirror the terminal flow exactly: scanning state → the
machine list with GB10-class badges (non-GB10 rows dimmed, picking one shows the
same warning copy with proceed/cancel) → username (pre-filled from remembered
users) → password prompts appear only when SSH actually asks (agent and default
keys are tried first, same as the CLI) → the listen choice as three options with
the same copy and recommended default → live progress lines → the summary card:
console URL as a link, pairing token in mono with a copy button, and the same
loopback note when applicable. Progress arrives via SSE or 1s polling — match
whichever pattern is simpler to keep dependency-free.

Implementation shape: wizard assets live embedded next to the server code
(`internal/setupweb`), written as plain HTML/CSS/JS (no React build step — this
page is small, and keeping it out of `webui/ui` avoids coupling installer
releases to console builds). Reuse the console's visual tokens by copying the
few needed values, and note in a comment they are copies.

## C. Packaging

`scripts/release.sh` changes:
1. Build `cmd/runonspark-setup` for darwin arm64/amd64 and windows arm64/amd64.
   Windows uses `-ldflags "-H=windowsgui ..."` — that flag applies to this binary
   only, never to `runonspark-manager` (which must keep its console for servers
   and the CLI).
2. macOS zips now contain `RunOnSpark Setup.app`: `Contents/Info.plist` (bundle
   name RunOnSpark Setup, identifier ai.runonspark.setup, LSMinimumSystemVersion
   fine at 11.0, no special entitlements) and `Contents/MacOS/runonspark-setup`.
   Finder double-click launches it with no Terminal. Codesign/notarize hooks sign
   the whole `.app` when credentials are present, same env-var behavior as spec 07.
3. Windows zips now contain `RunOnSpark Setup.exe` (the windowsgui build). The
   `.bat` and `.command` wrappers are removed — superseded by the native
   artifacts. The five `runonspark-manager` binaries keep shipping unchanged.

## Acceptance

- Terminal path unchanged: `runonspark-manager setup` transcript to the machine
  list is identical to before the refactor (paste both in the report).
- `open "dist/.../RunOnSpark Setup.app"` on the build Mac launches with no
  Terminal window and the browser opens the wizard; the real network scan renders
  the machine list with correct GB10 badges. Do not proceed past the machine list
  against real machines; do not enter any credentials.
- Later steps (username, listen, progress, summary) are exercised against a mock:
  drive the wizard flow with a fake Runner/prober in a test or a screenshot
  harness, and screenshot every step. The summary screenshot must show a fake
  pairing token, clearly fake (e.g. `PAIR-000000`).
- Security tests: request without token → 404/403; wrong token → same; foreign
  Origin header → rejected; password value provably absent from all logs and all
  GET URLs (grep the test server's request log).
- `GOOS=windows go build -ldflags -H=windowsgui ./cmd/runonspark-setup` compiles
  for both arches. Manual Windows steps documented for the owner.
- Full verify suite green; `file` table + checksums for the new artifact set.

## Out of scope

Signing identities (hooks only), download hosting, the curl script, and any
change to the manager's own console or API. The GUI wizard talks SSH outward
exactly like the CLI; it never touches a manager API.
