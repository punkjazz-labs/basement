#!/bin/sh
# Builds, signs, notarizes and staples the double-clickable macOS installer,
# then uploads it to the release.
#
#   packaging/build-macos-installer.sh v0.9.3
#
# Run on the Mac that holds the Developer ID identity and the notarytool
# keychain profile. packaging/sign-macos-release.sh calls this as its last
# step, so a normal release only ever runs that one script; this one is here
# on its own so the installer can be rebuilt without re-signing the manager
# binaries.
#
# What it produces: basement-setup-macos.dmg, one download that works on both
# Apple Silicon and Intel, containing Basement Setup.app.
#
# Universal binary. CI (.github/workflows/release.yml) runs on Linux and
# publishes two separate darwin slices of cmd/basement-setup. Those are build
# inputs, not the user-facing download: this script fetches both and lipos
# them into one universal executable inside the bundle. The lipo has to happen
# on a Mac, and a Mac is required for this step regardless, because the
# Developer ID identity and the notarization credentials live here and nowhere
# else. Moving only the lipo into a macOS CI runner would remove nothing from
# this script.
#
# DMG contents. One item: Basement Setup.app. No /Applications symlink, and
# that is deliberate. This is a one-shot installer that opens a browser tab,
# installs basement onto a GB10 over SSH, and is then finished; it is not
# something to keep. An Applications symlink would invite people to drag a
# tool they will use once into the folder they keep their real apps in, and
# then wonder later what it is and whether deleting it breaks their Spark.
# Running it straight from the mounted disk image works: macOS may relocate a
# freshly downloaded app to a randomised read-only path (App Translocation),
# which is harmless here because the bundle reads nothing beside itself, its
# assets being compiled in.
#
# Notarizing twice is intentional. The app is notarized and stapled first, so
# it carries its own ticket if someone copies it out of the disk image; then
# the disk image is notarized and stapled, so the download itself opens
# offline with no Gatekeeper warning. Stapling is the entire point of this
# script: without a stapled ticket the first launch needs a working network
# path to Apple, and fails scarily when there is not one.
#
# Rehearsal. REHEARSE=1 with SETUP_SLICE_DIR pointing at locally built slices
# assembles and signs the bundle and stops there: nothing is notarized,
# stapled or uploaded, and the output is named so it cannot be mistaken for a
# release artifact.
#
#   GOOS=darwin GOARCH=arm64 go build -o /tmp/s/basement-setup-darwin-arm64 ./cmd/basement-setup
#   GOOS=darwin GOARCH=amd64 go build -o /tmp/s/basement-setup-darwin-amd64 ./cmd/basement-setup
#   REHEARSE=1 SETUP_SLICE_DIR=/tmp/s packaging/build-macos-installer.sh v0.0.0
set -eu

TAG="${1:?usage: build-macos-installer.sh <tag>}"
IDENTITY="${SIGN_IDENTITY:?SIGN_IDENTITY must be set to the Developer ID Application identity, e.g. \"Developer ID Application: Your Name (TEAMID)\"}"
PROFILE="${NOTARY_PROFILE:-basement}"
REHEARSE="${REHEARSE:-0}"

REPO_ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
APP_NAME="Basement Setup"
DMG_NAME="basement-setup-macos.dmg"
if [ "$REHEARSE" = 1 ]; then
  DMG_NAME="basement-setup-macos-REHEARSAL.dmg"
fi

# CFBundleVersion and CFBundleShortVersionString must both be dotted integers;
# Gatekeeper does not care but the plist is malformed to the system if they
# are not. A tag like v1.2.3-rc4 contributes 1.2.3.
VERSION=$(printf '%s' "${TAG#v}" | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*$/\1/')
if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+(\.[0-9]+){0,2}$'; then
  echo "build-macos-installer: cannot read a version out of tag '$TAG'" >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------- slices ---
if [ -n "${SETUP_SLICE_DIR:-}" ]; then
  echo "==> using locally built slices from $SETUP_SLICE_DIR"
  for arch in arm64 amd64; do
    cp "$SETUP_SLICE_DIR/basement-setup-darwin-${arch}" "$WORK/"
  done
elif [ "$REHEARSE" = 1 ]; then
  echo "build-macos-installer: REHEARSE=1 needs SETUP_SLICE_DIR" >&2
  exit 1
else
  echo "==> downloading darwin slices of $TAG"
  for arch in arm64 amd64; do
    gh release download "$TAG" --pattern "basement-setup-darwin-${arch}" \
      --dir "$WORK" --clobber
  done
fi

lipo -create -output "$WORK/basement-setup" \
  "$WORK/basement-setup-darwin-arm64" "$WORK/basement-setup-darwin-amd64"
chmod +x "$WORK/basement-setup"
lipo -info "$WORK/basement-setup"

# ---------------------------------------------------------------- bundle ---
APP="$WORK/${APP_NAME}.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
mv "$WORK/basement-setup" "$APP/Contents/MacOS/basement-setup"
cp "$REPO_ROOT/packaging/macos/basement.icns" "$APP/Contents/Resources/basement.icns"
cp "$REPO_ROOT/packaging/macos/Info.plist" "$APP/Contents/Info.plist"

/usr/libexec/PlistBuddy \
  -c "Set :CFBundleShortVersionString $VERSION" \
  -c "Set :CFBundleVersion $VERSION" \
  "$APP/Contents/Info.plist" >/dev/null
plutil -lint "$APP/Contents/Info.plist"

# ------------------------------------------------------------------ sign ---
echo "==> signing ${APP_NAME}.app"
codesign --force --options runtime --timestamp --sign "$IDENTITY" "$APP"
codesign --verify --strict --verbose=2 "$APP"

if [ "$REHEARSE" = 1 ]; then
  echo "==> rehearsal: skipping notarization, stapling and upload"
fi

# -------------------------------------------------------------- notarize ---
if [ "$REHEARSE" != 1 ]; then
  echo "==> notarizing ${APP_NAME}.app"
  ditto -c -k --keepParent "$APP" "$WORK/app.zip"
  xcrun notarytool submit "$WORK/app.zip" --keychain-profile "$PROFILE" --wait
  xcrun stapler staple "$APP"
  codesign --verify --strict "$APP"
fi

# ------------------------------------------------------------------- dmg ---
echo "==> building $DMG_NAME"
STAGE="$WORK/stage"
mkdir -p "$STAGE"
# ditto, not cp: it is the copy that keeps a signed bundle's extended
# attributes and its stapled ticket intact.
ditto "$APP" "$STAGE/${APP_NAME}.app"
DMG="$WORK/$DMG_NAME"
hdiutil create -quiet -volname "$APP_NAME" -srcfolder "$STAGE" \
  -fs HFS+ -format UDZO -ov "$DMG"

codesign --force --timestamp --sign "$IDENTITY" "$DMG"
codesign --verify --strict --verbose=2 "$DMG"

if [ "$REHEARSE" = 1 ]; then
  cp "$DMG" "$REPO_ROOT/$DMG_NAME"
  echo "==> rehearsal complete: $REPO_ROOT/$DMG_NAME (not notarized, do not ship)"
  exit 0
fi

echo "==> notarizing $DMG_NAME"
xcrun notarytool submit "$DMG" --keychain-profile "$PROFILE" --wait
xcrun stapler staple "$DMG"

# ----------------------------------------------------------------- verify ---
# Every check below is fatal. A disk image that reaches people without a
# stapled ticket is the failure this script exists to prevent, so it must
# never be possible to get an upload out of a run that could not prove the
# ticket is there.
echo "==> verifying"
xcrun stapler validate "$DMG"
spctl --assess --type open --context context:primary-signature --verbose=2 "$DMG"

MOUNT="$WORK/mnt"
mkdir -p "$MOUNT"
hdiutil attach -quiet -nobrowse -readonly -mountpoint "$MOUNT" "$DMG"
mounted_ok=0
if xcrun stapler validate "$MOUNT/${APP_NAME}.app" &&
   spctl --assess --type execute --verbose=2 "$MOUNT/${APP_NAME}.app"; then
  mounted_ok=1
fi
hdiutil detach -quiet "$MOUNT"
if [ "$mounted_ok" != 1 ]; then
  echo "build-macos-installer: the app inside the disk image did not verify" >&2
  exit 1
fi

# ----------------------------------------------------------------- upload ---
(cd "$WORK" && shasum -a 256 "$DMG_NAME" > "$DMG_NAME.sha256")
gh release upload "$TAG" "$DMG" "$DMG.sha256" --clobber

echo "==> done: $DMG_NAME signed, notarized, stapled and uploaded to $TAG"
