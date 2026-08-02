#!/usr/bin/env bash
# Builds the release binaries for every supported platform into dist/, stages
# the macOS and Windows native installer zips, and writes a dist/SHA256SUMS
# manifest covering every artifact this script produces. Run on macOS;
# cross-compilation needs no target toolchain since CGO_ENABLED=0.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

DIST_DIR="$ROOT_DIR/dist"
MANAGER_PACKAGE="./cmd/basement"
SETUP_PACKAGE="./cmd/basement-setup"
VERSION="$(git describe --tags --always --dirty)"

# goos/goarch pairs this spec ships. linux/arm64 is the GB10 machines
# themselves; the rest are the setup-wizard download targets, for both the
# manager binary (the curl path still needs it) and the native installer.
MANAGER_TARGETS=(
  "linux arm64"
  "darwin arm64"
  "darwin amd64"
  "windows amd64"
  "windows arm64"
)
SETUP_TARGETS=(
  "darwin arm64"
  "darwin amd64"
  "windows amd64"
  "windows arm64"
)

echo "Building basement $VERSION"
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

for target in "${MANAGER_TARGETS[@]}"; do
  read -r goos goarch <<<"$target"
  outdir="$DIST_DIR/$goos-$goarch"
  mkdir -p "$outdir"
  binname="basement"
  if [ "$goos" = "windows" ]; then
    binname="basement.exe"
  fi
  echo "-> $goos/$goarch basement"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$outdir/$binname" "$MANAGER_PACKAGE"
done

# basement-setup: the double-clickable installer, no terminal ever. Windows
# gets -H=windowsgui so no console window is created at all — that flag is
# specific to this binary and must never apply to basement, which keeps its
# console for servers and the CLI.
echo "Building basement-setup $VERSION"
for target in "${SETUP_TARGETS[@]}"; do
  read -r goos goarch <<<"$target"
  outdir="$DIST_DIR/$goos-$goarch"
  mkdir -p "$outdir"
  binname="basement-setup"
  ldflags="-s -w -X main.version=$VERSION"
  if [ "$goos" = "windows" ]; then
    binname="basement-setup.exe"
    ldflags="$ldflags -H=windowsgui"
  fi
  echo "-> $goos/$goarch basement-setup"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags "$ldflags" \
    -o "$outdir/$binname" "$SETUP_PACKAGE"
done

# macOS native installer: basement setup.app wraps the darwin basement-setup
# binary so Finder launches it directly with no Terminal window. Signing and
# notarization only run when the environment provides the credentials;
# otherwise the zip ships unsigned and the skip is printed so it is never
# silent.
stage_macos_zip() {
  local arch="$1"
  local src="$DIST_DIR/darwin-$arch/basement-setup"
  local stage
  stage="$(mktemp -d)"
  local app="$stage/basement setup.app"

  mkdir -p "$app/Contents/MacOS"
  cp "$ROOT_DIR/packaging/macos/Info.plist" "$app/Contents/Info.plist"
  cp "$src" "$app/Contents/MacOS/basement-setup"
  chmod +x "$app/Contents/MacOS/basement-setup"

  if [ -n "${CODESIGN_IDENTITY:-}" ]; then
    echo "  codesigning darwin-$arch .app with $CODESIGN_IDENTITY"
    codesign --force --options runtime --timestamp --sign "$CODESIGN_IDENTITY" "$app/Contents/MacOS/basement-setup"
    codesign --force --options runtime --timestamp --sign "$CODESIGN_IDENTITY" "$app"
  else
    echo "  CODESIGN_IDENTITY not set; shipping darwin-$arch .app unsigned (Gatekeeper: right-click, Open)"
  fi

  (cd "$stage" && zip -q -X -r "$DIST_DIR/basement-setup-macos-$arch.zip" "basement setup.app")
  rm -rf "$stage"

  if [ -n "${CODESIGN_IDENTITY:-}" ] && [ -n "${NOTARY_PROFILE:-}" ]; then
    echo "  notarizing darwin-$arch with profile $NOTARY_PROFILE"
    xcrun notarytool submit "$DIST_DIR/basement-setup-macos-$arch.zip" \
      --keychain-profile "$NOTARY_PROFILE" --wait
  elif [ -n "${CODESIGN_IDENTITY:-}" ]; then
    echo "  NOTARY_PROFILE not set; skipping notarization for darwin-$arch"
  fi
}

echo "Staging macOS native installer"
stage_macos_zip arm64
stage_macos_zip amd64

# Windows native installer: the windowsgui basement-setup.exe, renamed for
# the download, and nothing else — no .bat wrapper needed since the exe
# itself opens no console. No certificate exists to sign it, so it ships
# unsigned and SmartScreen will warn on first run (see the spec report for
# the distribution decision this implies).
stage_windows_zip() {
  local arch="$1"
  local src="$DIST_DIR/windows-$arch/basement-setup.exe"
  local stage
  stage="$(mktemp -d)"

  cp "$src" "$stage/basement setup.exe"
  (cd "$stage" && zip -q -X -r "$DIST_DIR/basement-setup-windows-$arch.zip" "basement setup.exe")
  rm -rf "$stage"

  echo "  windows-$arch unsigned (no certificate) - SmartScreen will warn on first run"
}

echo "Staging Windows native installer"
stage_windows_zip amd64
stage_windows_zip arm64

echo "Writing SHA256SUMS"
(
  cd "$DIST_DIR"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 >SHA256SUMS
)

echo "Done. Artifacts in $DIST_DIR/"
