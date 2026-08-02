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
