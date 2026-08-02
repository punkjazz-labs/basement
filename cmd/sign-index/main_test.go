package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

func TestSignIndexProducesASignatureVerifySignatureAccepts(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	indexBody := []byte(`{"schema_version":1,"generated_at":"2026-08-01T00:00:00Z","recipes":[]}`)
	if err := os.WriteFile(indexPath, indexBody, 0o640); err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "private.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run(indexPath, keyPath); err != nil {
		t.Fatalf("run() failed: %v", err)
	}

	sigBytes, err := os.ReadFile(indexPath + ".minisig")
	if err != nil {
		t.Fatalf("signature file was not written: %v", err)
	}
	if err := recipe.VerifySignature(indexBody, sigBytes, pub); err != nil {
		t.Fatalf("signature produced by sign-index did not verify: %v", err)
	}
}

func TestDecodePrivateKeyRejectsWrongLength(t *testing.T) {
	if _, err := decodePrivateKey([]byte(base64.StdEncoding.EncodeToString([]byte("too short")))); err == nil {
		t.Fatal("a key file of the wrong length was accepted")
	}
}

func TestDecodePrivateKeyRejectsGarbage(t *testing.T) {
	if _, err := decodePrivateKey([]byte("not base64 at all !!")); err == nil {
		t.Fatal("a non-base64 key file was accepted")
	}
}
