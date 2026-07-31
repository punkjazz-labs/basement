# 0010 — Network setup with GB10 autodiscovery

Status: accepted · implemented (`runonspark-manager setup`)

## Context

Installation previously required SSHing into the machine and running
`packaging/install.sh` with a downloaded binary. That is fine for operators
and hostile to everyone else. The product goal is: run one command on any
machine in the house — the GB10 box itself or a laptop next to it — and end
up with a paired console in the browser.

DGX Spark is not the only GB10 machine. OEM equivalents (ASUS Ascent GX10,
MSI EdgeXpert, Gigabyte AI TOP Atom, Acer Veriton GN100, Dell Pro Max GB10,
Lenovo ThinkStation PGX, …) ship the same superchip under vendor default
hostnames, so discovery must not assume NVIDIA's `spark-xxxx` naming.

## Decision

One subcommand, two modes:

- **On a GB10 machine** (`linux` + identity probe passes): install locally.
  The running binary is the artifact — no download.
- **Anywhere else**: discover candidates, let the operator pick the master,
  install over SSH, then print the pairing card and open the console in the
  operator's browser (for non-loopback listen modes).

### Discovery (internal/discovery)

- Parallel TCP:22 sweep of the local IPv4 networks, capped at /24 per
  interface; Tailscale's CGNAT range is excluded (point-to-point, not a LAN).
- One-shot mDNS browse of `_ssh._tcp.local.` with the RFC 6762
  unicast-response bit, so we never contend for port 5353 with avahi or
  mDNSResponder. Implemented directly on `x/net/dns/dnsmessage`; no
  third-party mDNS dependency.
- Reverse mDNS (`in-addr.arpa` PTR) recovers the friendly `.local` hostname
  avahi publishes for each sweep hit.
- Vendor hostname fragments (`spark`, `gx10`, `ascent`, `edgexpert`,
  `aitop`, `veriton`, `pgx`, `promax`, `gb10`) **rank** the list only. They
  never filter: identity is confirmed after connecting, by `nvidia-smi`
  reporting the GB10 chip or the device tree naming a known product. A
  non-GB10 pick warns and asks before continuing.

### Install engine (internal/setup)

The engine mirrors `packaging/install.sh` step for step (service user,
binary under /usr/lib, unit file, listen drop-in, `enable --now`, pairing
token wait) through a `Runner` interface with local and SSH
implementations, so both install paths stay behaviorally identical. The
systemd unit is embedded as a byte-for-byte copy of the packaged one; a
test fails if they drift.

Binary provisioning order: `--binary <path>` upload → self-upload when the
running binary is already linux/arm64 → download of the latest GitHub
release **on the target** with checksum verification. Uploads are refused
unless the file is an ELF/aarch64 binary, and are sha256-verified after
transfer. Release assets must be published as
`runonspark-manager-linux-arm64` (+ `.sha256`) for the download path;
signing them is ADR 0008's follow-up.

### Security posture

- SSH is the only authentication boundary: agent first, then default key
  files, then password, then keyboard-interactive (for sshd configurations
  that disable plain password auth). Passwords and passphrases are prompted
  without echo, held in memory for the session, and never persisted or
  logged. The remote username is always prompted (the GB10 account is set
  at the machine's first boot and rarely matches the operator's local
  username); `--user` skips the prompt.
- Host keys verify against `~/.ssh/known_hosts` with trust-on-first-use:
  unknown hosts show the SHA256 fingerprint and require explicit consent; a
  **changed** key is a hard failure, never re-promptable.
- sudo: passwordless probed first; otherwise prompted once per session.

### Fleet groundwork

Non-selected discovered machines are recorded on the master as
`fleet.json` (`discovered_peers`). Nothing consumes it yet; it is the seed
for multi-GB10 (master/worker) recipes so a future distributed topology
can offer known peers without a fresh discovery pass.

## Consequences

- New dependencies: `golang.org/x/crypto`, `golang.org/x/net`,
  `golang.org/x/term` (Go-team maintained; no third-party additions).
- `install.sh` remains for release-package installs; behavioral parity with
  the engine is enforced only for the systemd unit today. Any change to the
  install steps must be made in both places until the script is retired.
- The sweep is polite (4s budget, /24 cap) but still visible on the
  network; `--host` skips discovery entirely for operators who know the
  address.
- Hardware qualification of the whole flow needs a real GB10 machine and is
  part of the DGX Spark test pass.
