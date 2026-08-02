#!/usr/bin/env bash
# Builds the release binaries for every supported platform into dist/, stages
# the macOS and Windows double-click zips, and writes a dist/SHA256SUMS
# manifest covering every artifact this script produces. Run on macOS;
# cross-compilation needs no target toolchain since CGO_ENABLED=0.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

DIST_DIR="$ROOT_DIR/dist"
PACKAGE="./cmd/runonspark-manager"
VERSION="$(git describe --tags --always --dirty)"

# goos/goarch pairs this spec ships. linux/arm64 is the GB10 machines
# themselves; the rest are the setup-wizard download targets.
TARGETS=(
  "linux arm64"
  "darwin arm64"
  "darwin amd64"
  "windows amd64"
  "windows arm64"
)

echo "Building runonspark-manager $VERSION"
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

for target in "${TARGETS[@]}"; do
  read -r goos goarch <<<"$target"
  outdir="$DIST_DIR/$goos-$goarch"
  mkdir -p "$outdir"
  binname="runonspark-manager"
  if [ "$goos" = "windows" ]; then
    binname="runonspark-manager.exe"
  fi
  echo "-> $goos/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$outdir/$binname" "$PACKAGE"
done

# macOS double-click artifact: the darwin binary plus a .command launcher
# that execs `setup` in place, so double-clicking opens Terminal straight
# into the same wizard the curl one-liner reaches. Signing and notarization
# only run when the environment provides the credentials; otherwise the zip
# ships unsigned and the skip is printed so it is never silent.
stage_macos_zip() {
  local arch="$1"
  local src="$DIST_DIR/darwin-$arch/runonspark-manager"
  local stage
  stage="$(mktemp -d)"

  cp "$src" "$stage/runonspark-manager"
  cat >"$stage/RunOnSpark Setup.command" <<'CMD'
#!/bin/sh
cd "$(dirname "$0")"
exec ./runonspark-manager setup
CMD
  chmod +x "$stage/runonspark-manager" "$stage/RunOnSpark Setup.command"

  if [ -n "${CODESIGN_IDENTITY:-}" ]; then
    echo "  codesigning darwin-$arch with $CODESIGN_IDENTITY"
    codesign --force --options runtime --timestamp --sign "$CODESIGN_IDENTITY" "$stage/runonspark-manager"
  else
    echo "  CODESIGN_IDENTITY not set; shipping darwin-$arch unsigned (Gatekeeper: right-click, Open)"
  fi

  (cd "$stage" && zip -q -X -r "$DIST_DIR/RunOnSpark-Setup-macos-$arch.zip" .)
  rm -rf "$stage"

  if [ -n "${CODESIGN_IDENTITY:-}" ] && [ -n "${NOTARY_PROFILE:-}" ]; then
    echo "  notarizing darwin-$arch with profile $NOTARY_PROFILE"
    xcrun notarytool submit "$DIST_DIR/RunOnSpark-Setup-macos-$arch.zip" \
      --keychain-profile "$NOTARY_PROFILE" --wait
  elif [ -n "${CODESIGN_IDENTITY:-}" ]; then
    echo "  NOTARY_PROFILE not set; skipping notarization for darwin-$arch"
  fi
}

echo "Staging macOS double-click artifacts"
stage_macos_zip arm64
stage_macos_zip amd64

# Windows double-click artifact: just the .exe. There is no launcher script
# because double-clicking a console .exe already opens its own window; the
# console-ownership pause (cmd/runonspark-manager/console_windows.go) is what
# keeps that window from vanishing before it can be read. No certificate
# exists to sign it, so it is unsigned and SmartScreen will warn on first run
# (see the spec report for the distribution decision this implies).
stage_windows_zip() {
  local arch="$1"
  local src="$DIST_DIR/windows-$arch/runonspark-manager.exe"
  local stage
  stage="$(mktemp -d)"

  cp "$src" "$stage/runonspark-manager.exe"
  (cd "$stage" && zip -q -X -r "$DIST_DIR/RunOnSpark-Setup-windows-$arch.zip" .)
  rm -rf "$stage"

  echo "  windows-$arch unsigned (no certificate) - SmartScreen will warn on first run"
}

echo "Staging Windows artifacts"
stage_windows_zip amd64
stage_windows_zip arm64

echo "Writing SHA256SUMS"
(
  cd "$DIST_DIR"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 >SHA256SUMS
)

echo "Done. Artifacts in $DIST_DIR/"
