#!/usr/bin/env bash
# Builds the release binaries for every supported platform into dist/, plus a
# dist/SHA256SUMS manifest covering every artifact this script produces.
# Run on macOS; cross-compilation needs no target toolchain since
# CGO_ENABLED=0.
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

echo "Writing SHA256SUMS"
(
  cd "$DIST_DIR"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 >SHA256SUMS
)

echo "Done. Artifacts in $DIST_DIR/"
