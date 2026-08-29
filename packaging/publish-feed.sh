#!/bin/bash
# Publishes the signed recipe feed (docs/RECIPE-FEED.md). Applies the safe
# subset of upstream pin moves, commits and pushes those, builds and signs
# index.json, pushes it to the feed repository, and then verifies the
# published bytes match what was pushed. Every step fails closed: a step this
# script cannot complete stops the run rather than publishing a partial or
# unverified feed.
#
# Mirrors packaging/sign-linux-update.sh's conventions: a temporary work
# directory cleaned up in a trap on every exit path, the signing key read
# from the macOS Keychain and never written to this repository or a command
# line, and a plain PASS/FAIL that means the file bytes were verified, not
# merely that no command returned an error.
#
# Usage:
#   packaging/publish-feed.sh
#
# Run on the owner's laptop, from a clean checkout of main. See docs/
# RECIPE-FEED.md section 6 for the scheduled job that runs this unattended.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# The feed repository (docs/RECIPE-FEED.md section 2c) and the raw URL a
# manager fetches the feed from (internal/recipefeed/fetch.go's IndexURL).
# Keep these in sync with that Go constant if the feed ever moves host.
feed_repo_url="git@github.com:punkjazz-labs/runonspark-recipes.git"
index_url="https://raw.githubusercontent.com/punkjazz-labs/runonspark-recipes/main/index.json"

keychain_account="simo"
keychain_service="basement-recipe-feed"

work="$(mktemp -d)"
key_file=""
trap 'rm -f "$key_file"; rm -rf "$work"' EXIT INT TERM

echo "==> checking the working tree is clean"
if [ -n "$(git status --porcelain)" ]; then
  echo "publish-feed: the working tree is not clean; commit or stash before publishing" >&2
  exit 1
fi

echo "==> feed-watch -mode bump"
bump_report="$work/feed-watch-report.json"
# feed-watch speaks through its exit code (0 clean, 3 drift left for a
# maintainer, 4 upstream unreachable), and `go run` collapses every nonzero
# program exit into its own exit 1, so the code must come from the compiled
# binary itself.
feed_watch_bin="$work/feed-watch"
go build -o "$feed_watch_bin" ./cmd/feed-watch
set +e
"$feed_watch_bin" -mode bump -out "$bump_report"
bump_exit=$?
set -e
case "$bump_exit" in
  0) ;;
  3) echo "==> feed-watch left drift for a maintainer to judge; see the summary above and $bump_report" ;;
  4) echo "==> feed-watch could not reach one or more upstream sources; see the summary above and $bump_report" ;;
  *)
    echo "publish-feed: feed-watch -mode bump failed unexpectedly (exit $bump_exit); see $bump_report" >&2
    exit 1
    ;;
esac

if [ -n "$(git status --porcelain -- internal/recipe/recipes)" ]; then
  echo "==> feed-watch bumped one or more recipes; testing and committing"
  go test ./internal/recipe/...
  changed_recipes="$(git status --porcelain -- internal/recipe/recipes | awk '{print $2}' | xargs -n1 basename)"
  git add internal/recipe/recipes
  git commit -m "manual: feed-watch pin bumps" -m "$changed_recipes"
  git push origin main
else
  echo "==> feed-watch found nothing to bump"
fi

echo "==> build-index"
index_path="$work/index.json"
revoked_args=()
if [ -n "${REVOKED:-}" ]; then
  revoked_args=(-revoked "$REVOKED")
fi
# The +-expansion keeps macOS's bash 3.2 happy: expanding an empty array
# under set -u is an unbound-variable error there.
build_summary="$(go run ./cmd/build-index -out "$index_path" ${revoked_args[@]+"${revoked_args[@]}"})"
echo "$build_summary"
recipe_count="$(printf '%s\n' "$build_summary" | grep -oE '[0-9]+ recipe' | grep -oE '[0-9]+')"
revocation_count="$(printf '%s\n' "$build_summary" | grep -oE '[0-9]+ revocation' | grep -oE '[0-9]+')"

echo "==> reading the signing key from the keychain"
key_file="$(mktemp)"
chmod 600 "$key_file"
security find-generic-password -a "$keychain_account" -s "$keychain_service" -w >"$key_file"

echo "==> sign-index"
go run ./cmd/sign-index -index "$index_path" -key "$key_file"
rm -f "$key_file"
key_file=""

echo "==> publishing to $feed_repo_url"
feed_clone="$work/runonspark-recipes"
if ! git clone --quiet "$feed_repo_url" "$feed_clone" 2>"$work/clone.err"; then
  cat "$work/clone.err" >&2
  echo "publish-feed: could not clone $feed_repo_url; create an empty repository named punkjazz-labs/runonspark-recipes with a main branch, then rerun this script" >&2
  exit 1
fi

cp "$index_path" "$feed_clone/index.json"
cp "$index_path.sig" "$feed_clone/index.json.sig"
timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
(
  cd "$feed_clone"
  git checkout -B main
  git add index.json index.json.sig
  git commit -m "feed: ${recipe_count} recipes, ${revocation_count} revocations, ${timestamp}"
  git push origin main
)

echo "==> verifying the published feed"
published_index="$work/published-index.json"
published_sig="$work/published-index.json.sig"
# raw.githubusercontent.com caches a file for up to 300 seconds, so the first
# read after a good push can still return the bytes of the previous feed. That
# is cache lag, not a bad publish, and it clears itself. Ask again every 30
# seconds for up to 7 minutes, which covers the whole cache window, and pass on
# the first byte match. A mismatch that outlives the window is a real failure
# and still fails the run. The curls are silent here because a retry must cost
# one line at most; the failure below says what a failed run needs to know.
verify_deadline=$((SECONDS + 420))
verified=""
while :; do
  if curl -fsL "$index_url" -o "$published_index" &&
    curl -fsL "$index_url.sig" -o "$published_sig" &&
    cmp -s "$index_path" "$published_index" &&
    cmp -s "$index_path.sig" "$published_sig"; then
    verified=yes
    break
  fi
  if [ "$SECONDS" -ge "$verify_deadline" ]; then
    break
  fi
  echo "==> the published feed still reads as the old one; asking again in 30s"
  sleep 30
done

if [ -n "$verified" ]; then
  echo "PASS: the published feed matches what was pushed, byte for byte"
else
  echo "FAIL: the published feed does not match what was pushed" >&2
  exit 1
fi
