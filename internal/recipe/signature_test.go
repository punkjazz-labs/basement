package recipe

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// testKeypair generates an ephemeral ed25519 keypair inside the test
// process. The private key never leaves this function's stack and is never
// written anywhere; it exists only to make VerifySignature exercise a real
// signature instead of a fixture. See the spec 04 executor report for why
// this approach (over a committed test key) was chosen.
func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sign(priv ed25519.PrivateKey, message []byte) []byte {
	signature := ed25519.Sign(priv, message)
	return []byte(base64.StdEncoding.EncodeToString(signature) + "\n")
}

func TestVerifySignatureAccepts(t *testing.T) {
	pub, priv := testKeypair(t)
	message := []byte(`{"schema_version":1}`)
	if err := VerifySignature(message, sign(priv, message), pub); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifySignatureRejectsWrongKey(t *testing.T) {
	_, priv := testKeypair(t)
	otherPub, _ := testKeypair(t)
	message := []byte(`{"schema_version":1}`)
	if err := VerifySignature(message, sign(priv, message), otherPub); err == nil {
		t.Fatal("signature from a different key was accepted")
	}
}

func TestVerifySignatureRejectsTamperedPayload(t *testing.T) {
	pub, priv := testKeypair(t)
	message := []byte(`{"schema_version":1,"recipes":[]}`)
	signature := sign(priv, message)
	tampered := []byte(`{"schema_version":1,"recipes":[{"id":"evil"}]}`)
	if err := VerifySignature(tampered, signature, pub); err == nil {
		t.Fatal("signature verified against a different payload than it was signed over")
	}
}

func TestVerifySignatureRejectsTruncated(t *testing.T) {
	pub, priv := testKeypair(t)
	message := []byte(`{"schema_version":1}`)
	full := sign(priv, message)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(full)))
	if err != nil {
		t.Fatal(err)
	}
	truncated := []byte(base64.StdEncoding.EncodeToString(decoded[:32]))
	if err := VerifySignature(message, truncated, pub); err == nil {
		t.Fatal("truncated signature was accepted")
	}
}

func TestVerifySignatureRejectsEmpty(t *testing.T) {
	pub, _ := testKeypair(t)
	if err := VerifySignature([]byte("hello"), []byte("   \n"), pub); err == nil {
		t.Fatal("empty signature file was accepted")
	}
}

func TestVerifySignatureRejectsGarbageBase64(t *testing.T) {
	pub, _ := testKeypair(t)
	if err := VerifySignature([]byte("hello"), []byte("not-base64!!"), pub); err == nil {
		t.Fatal("non-base64 signature file was accepted")
	}
}

func TestIndexPublicKeyIsWellFormed(t *testing.T) {
	pub := IndexPublicKey()
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("embedded public key has length %d, want %d", len(pub), ed25519.PublicKeySize)
	}
}
