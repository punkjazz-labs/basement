#!/bin/sh
# basement one-line installer bootstrap:
#   curl -fsSL https://github.com/punkjazz-labs/runonspark-manager/releases/latest/download/setup.sh | sh
# Downloads the manager binary for THIS machine, verifies its checksum, and
# runs `basement setup` — which discovers GB10 machines on the network (or
# installs locally when run on one).
set -eu

repo="punkjazz-labs/runonspark-manager"
base="https://github.com/${repo}/releases/latest/download"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *)
    echo "unsupported architecture: $arch" >&2
    exit 1
    ;;
esac
case "$os" in
  linux | darwin) ;;
  *)
    echo "unsupported operating system: $os (run setup from macOS or Linux)" >&2
    exit 1
    ;;
esac

asset="basement-${os}-${arch}"
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT INT TERM

echo "Downloading basement (${os}/${arch})…"
curl -fsSL "${base}/${asset}" -o "${dir}/basement"
curl -fsSL "${base}/${asset}.sha256" -o "${dir}/checksum"

expected=$(awk 'NR == 1 { print $1 }' "${dir}/checksum")
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${dir}/basement" | awk '{ print $1 }')
else
  actual=$(shasum -a 256 "${dir}/basement" | awk '{ print $1 }')
fi
if ! printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || [ "$actual" != "$expected" ]; then
  echo "checksum verification failed" >&2
  exit 1
fi
chmod +x "${dir}/basement"

# When piped through `curl | sh`, stdin is the script itself — reattach the
# terminal so setup's prompts work.
status=0
if [ -t 0 ]; then
  "${dir}/basement" setup "$@" || status=$?
elif (: </dev/tty) 2>/dev/null; then
  "${dir}/basement" setup "$@" </dev/tty || status=$?
else
  echo "no interactive terminal available; download the binary and run: basement setup" >&2
  exit 1
fi
exit "$status"
