#!/bin/sh
# Generate and verify the signed Linux manager update assets on the release
# Mac. The Ed25519 private key is read from macOS Keychain and is passed only
# through stdin to the signing command.
#
# SIGN_UPDATER_HELPER=1 signs the root updater helper into the same manifest,
# which makes it manifest schema 2 (ADR 0020). Schema 2 is the release from
# which a machine updates its own helper through the signed chain, and it is
# also the release every manager older than updater protocol 2 refuses: those
# managers fall back to the newest schema-1 release they can read and reach
# schema 2 on the hop after that.
#
# The default is deliberately off. The first release carrying a protocol-2
# manager and a protocol-2 helper must be published as schema 1 so that every
# manager already in the field can install it; only the release after that
# one is signed with SIGN_UPDATER_HELPER=1. Turning this on too early strands
# every machine that has not yet reached protocol 2, and the release before it
# must stay published permanently as their stepping stone.
set -eu

if [ "$#" -lt 2 ]; then
  echo "usage: sign-linux-update.sh <tag> <rollback-from> [rollback-from ...]" >&2
  exit 2
fi

tag=$1
shift
key_id=${UPDATE_KEY_ID:?UPDATE_KEY_ID must name the embedded release key}
keychain_service=${UPDATE_KEYCHAIN_SERVICE:?UPDATE_KEYCHAIN_SERVICE must name the macOS Keychain item}
keychain_account=${UPDATE_KEYCHAIN_ACCOUNT:-$(id -un)}
repository=${UPDATE_REPOSITORY:-punkjazz-labs/basement}

rollback_csv=""
for version in "$@"; do
  if [ -z "$rollback_csv" ]; then
    rollback_csv=$version
  else
    rollback_csv=$rollback_csv,$version
  fi
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# The rollback versions have been consumed into rollback_csv, so the positional
# parameters are free to carry the optional helper flag without any quoting
# that word splitting could get wrong.
if [ "${SIGN_UPDATER_HELPER:-0}" = "1" ]; then
  set -- -helper "$work/basement-updater-linux-arm64"
else
  set --
fi

gh release download "$tag" --repo "$repository" \
  --pattern basement-linux-arm64 \
  --pattern basement-updater-linux-arm64 \
  --dir "$work" --clobber

embedded_keys=$(gh variable get BASEMENT_UPDATE_PUBLIC_KEYS --repo "$repository" --json value --jq .value)
if [ -z "$embedded_keys" ]; then
  echo "BASEMENT_UPDATE_PUBLIC_KEYS is not configured for release builds" >&2
  exit 1
fi

security find-generic-password -a "$keychain_account" -s "$keychain_service" -w |
  go run ./cmd/sign-update-manifest \
    -mode sign \
    -asset "$work/basement-linux-arm64" \
    "$@" \
    -version "$tag" \
    -key-id "$key_id" \
    -rollback-from "$rollback_csv" \
    -manifest "$work/basement-linux-arm64.update.json" \
    -signature "$work/basement-linux-arm64.update.sig" \
    -public-key-out "$work/public-key.txt"

generated_key=$(tr -d '\n' < "$work/public-key.txt")
case ",$embedded_keys," in
  *",$generated_key,"*) ;;
  *)
    echo "the Keychain signing key does not match a public key embedded in release builds" >&2
    exit 1
    ;;
esac

go run ./cmd/sign-update-manifest \
  -mode verify \
  -asset "$work/basement-linux-arm64" \
  "$@" \
  -version "$tag" \
  -manifest "$work/basement-linux-arm64.update.json" \
  -signature "$work/basement-linux-arm64.update.sig" \
  -key-ring "$embedded_keys"

gh release upload "$tag" --repo "$repository" \
  "$work/basement-linux-arm64.update.json" \
  "$work/basement-linux-arm64.update.sig" \
  --clobber

rm -f "$work/basement-linux-arm64.update.json" "$work/basement-linux-arm64.update.sig"
gh release download "$tag" --repo "$repository" \
  --pattern basement-linux-arm64.update.json \
  --pattern basement-linux-arm64.update.sig \
  --dir "$work" --clobber

go run ./cmd/sign-update-manifest \
  -mode verify \
  -asset "$work/basement-linux-arm64" \
  "$@" \
  -version "$tag" \
  -manifest "$work/basement-linux-arm64.update.json" \
  -signature "$work/basement-linux-arm64.update.sig" \
  -key-ring "$embedded_keys"

echo "==> signed Linux update manifest verified for $tag"
