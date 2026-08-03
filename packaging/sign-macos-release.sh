#!/bin/sh
# Signs and notarizes the macOS binaries of a published release, then
# replaces the release assets with the signed ones.
#
# Run on the Mac that holds the Developer ID identity and the notarytool
# keychain profile (created with: xcrun notarytool store-credentials).
#
#   packaging/sign-macos-release.sh v0.9.3
#
# The workflow publishes unsigned binaries from Linux CI; this script is
# the signing step that cannot run there. It downloads the darwin assets,
# signs them with the hardened runtime, notarizes the zips, verifies, and
# uploads the signed artifacts back over the originals.
set -eu

TAG="${1:?usage: sign-macos-release.sh <tag>}"
IDENTITY="${SIGN_IDENTITY:-Developer ID Application: the owner (TEAMID0000)}"
PROFILE="${NOTARY_PROFILE:-basement}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for arch in arm64 amd64; do
  bin="basement-darwin-${arch}"
  echo "==> ${bin}"
  gh release download "$TAG" --pattern "$bin" --dir "$WORK" --clobber
  chmod +x "$WORK/$bin"

  codesign --force --options runtime --timestamp \
    --sign "$IDENTITY" "$WORK/$bin"
  codesign --verify --strict "$WORK/$bin"

  # Bare executables cannot be stapled; notarize a zip so Gatekeeper can
  # confirm the ticket online, and ship the signed binary itself.
  ditto -c -k "$WORK/$bin" "$WORK/$bin.zip"
  xcrun notarytool submit "$WORK/$bin.zip" \
    --keychain-profile "$PROFILE" --wait

  (cd "$WORK" && shasum -a 256 "$bin" > "$bin.sha256")
  gh release upload "$TAG" "$WORK/$bin" "$WORK/$bin.sha256" --clobber
done

echo "==> done: signed and notarized darwin binaries replaced on $TAG"
