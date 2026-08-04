#!/bin/sh
# The macOS half of cutting a release: everything that cannot run in Linux CI
# because it needs the Developer ID identity and the notarytool credentials.
#
# Run on the Mac that holds both (the profile is created with:
# xcrun notarytool store-credentials).
#
#   packaging/sign-macos-release.sh v0.9.3 v0.9.2
#
# Two things happen, in this order:
#
#   1. The manager binaries (basement-darwin-arm64, basement-darwin-amd64) are
#      signed and notarized in place. These are what packaging/setup.sh
#      downloads for the curl|sh path and what `basement setup --binary`
#      installs, so their names and their being bare executables are fixed.
#   2. packaging/build-macos-installer.sh builds the double-clickable
#      installer: the two darwin slices of cmd/basement-setup lipo'd into one
#      universal binary inside Basement Setup.app, wrapped in a signed,
#      notarized and stapled basement-setup-macos.dmg.
#   3. packaging/sign-linux-update.sh signs the exact Linux manager manifest
#      with the dedicated Ed25519 key from macOS Keychain.
set -eu

if [ "$#" -lt 2 ]; then
  echo "usage: sign-macos-release.sh <tag> <rollback-from> [rollback-from ...]" >&2
  exit 2
fi
TAG=$1
shift
IDENTITY="${SIGN_IDENTITY:?SIGN_IDENTITY must be set to the Developer ID Application identity, e.g. \"Developer ID Application: Your Name (TEAMID)\"}"
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

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SIGN_IDENTITY="$IDENTITY" NOTARY_PROFILE="$PROFILE" \
  "$HERE/build-macos-installer.sh" "$TAG"

"$HERE/sign-linux-update.sh" "$TAG" "$@"

# The release is created as a draft by CI, because everything it uploads for
# macOS is unsigned until the steps above replace it. Publishing here, last,
# is what makes the release page go from nothing to fully signed in one move:
# nobody can download an unsigned binary, or a checksum that will stop
# matching, out of a window between the two. A release that never reaches
# this line stays a draft, which is the right outcome for one nobody signed.
echo "==> publishing $TAG"
gh release edit "$TAG" --draft=false --latest
echo "==> done: $TAG is published"
