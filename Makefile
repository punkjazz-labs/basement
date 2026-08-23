# build-index builds index.json, the unsigned document at the heart of the
# remote recipe feed (ADR 0009), from the recipes embedded in this binary.
# See cmd/build-index/main.go for the flags and docs/RECIPE-FEED.md for the
# complete publish flow: build-index, then sign-index below, then push both
# files to the feed repository.
#
# Usage:
#   make build-index OUT=path/to/index.json
.PHONY: build-index
build-index:
	go run ./cmd/build-index -out "$${OUT:-index.json}"

# sign-index produces index.json.minisig for the remote recipe index
# (docs/plans/04-remote-recipe-index.md). The private key never appears in
# this repository, in this Makefile, or in CI config: it must already exist
# as a file on the machine running this target, and its path — not its
# contents — is passed in via BASEMENT_SIGN_KEY so the key material never
# appears as a command-line argument either. See cmd/sign-index/main.go for
# the key file format and how to generate one.
#
# Usage:
#   BASEMENT_SIGN_KEY=/path/to/private.key make sign-index INDEX=path/to/index.json
.PHONY: sign-index
sign-index:
	@if [ -z "$$BASEMENT_SIGN_KEY" ]; then \
		echo "sign-index: set BASEMENT_SIGN_KEY to the path of the ed25519 private key file (never its contents)" >&2; \
		exit 1; \
	fi
	go run ./cmd/sign-index -index "$${INDEX:-index.json}" -key "$$BASEMENT_SIGN_KEY"

# publish-feed runs the complete recipe feed publish flow: apply the safe
# subset of upstream pin moves (cmd/feed-watch), build and sign a fresh
# index.json, push it to the feed repository, and verify the published bytes
# match what was pushed. See packaging/publish-feed.sh and docs/RECIPE-FEED.md
# section 4. It reads the feed signing key from the macOS Keychain itself, so
# it needs no BASEMENT_SIGN_KEY.
#
# Usage:
#   make publish-feed
.PHONY: publish-feed
publish-feed:
	packaging/publish-feed.sh
