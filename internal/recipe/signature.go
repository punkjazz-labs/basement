package recipe

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
)

// IndexPublicKeyBase64 verifies the detached signature over the remote
// recipe index. This is a PLACEHOLDER key: it was generated once to produce
// a syntactically real ed25519 public key, and the matching private key was
// discarded immediately after generation and was never written to disk, a
// commit, a log, or anywhere else — nothing signed with it should ever be
// trusted. Before the recipe index is published for real, generate a real
// keypair out of band (see `make sign-index` and docs/plans/04, "reporting"),
// keep the private key off this machine and out of version control, and
// replace this constant with the real public key.
const IndexPublicKeyBase64 = "R1LNvKqdbMBWhGcRY4I9KUr3LV1wfkI8kpowaMk5JtM="

// IndexPublicKey decodes the embedded placeholder constant. It panics on a
// malformed constant, which can only happen if this file itself is edited
// incorrectly — there is no untrusted input on this path.
func IndexPublicKey() ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(IndexPublicKeyBase64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		panic("recipe: IndexPublicKeyBase64 is not a valid ed25519 public key")
	}
	return ed25519.PublicKey(raw)
}

// VerifySignature checks a detached signature over the exact message bytes.
// Callers MUST call this before the message bytes are handed to any
// YAML/JSON parser: an attacker who can serve arbitrary bytes but not forge
// a signature must never reach the parser, let alone the recipe validator.
//
// The signature file holds exactly one line: the base64 encoding of the raw
// 64-byte ed25519 signature. Leading and trailing whitespace is ignored so a
// trailing newline from a text editor or `make sign-index` does not matter.
// This is a deliberately simple, self-contained format — not byte-compatible
// with the upstream minisign CLI's on-disk format (which additionally wraps
// the signature in untrusted/trusted comment lines and a second signature
// over the trusted comment). Full minisign wire compatibility was judged not
// worth an external dependency or an unverifiable from-scratch
// reimplementation of that wire format; the trust properties spec 04 asks
// for — an embedded ed25519 public key, a detached signature, verification
// strictly before parsing — are identical either way. See the spec 04
// executor report for this decision.
func VerifySignature(message, signatureFile []byte, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("verify index signature: public key is malformed")
	}
	trimmed := strings.TrimSpace(string(signatureFile))
	if trimmed == "" {
		return errors.New("verify index signature: signature file is empty")
	}
	signature, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return errors.New("verify index signature: signature is not valid base64")
	}
	if len(signature) != ed25519.SignatureSize {
		return errors.New("verify index signature: signature has the wrong length (truncated or corrupt)")
	}
	if !ed25519.Verify(publicKey, message, signature) {
		return errors.New("verify index signature: signature does not match")
	}
	return nil
}
