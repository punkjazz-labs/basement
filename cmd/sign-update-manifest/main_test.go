package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/punkjazz-labs/basement/internal/update"
)

func TestKeygenRoundTripsThroughSigningAndVerification(t *testing.T) {
	temporary := t.TempDir()
	keyID := "release-2026"
	keyRingPath := filepath.Join(temporary, "key-ring.txt")
	var privateKey bytes.Buffer
	if err := keygen(keyID, keyRingPath, &privateKey); err != nil {
		t.Fatal(err)
	}

	keyRingBytes, err := os.ReadFile(keyRingPath)
	if err != nil {
		t.Fatal(err)
	}
	keyRing, err := update.ParseKeyRing(string(bytes.TrimSpace(keyRingBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(keyRing) != 1 || len(keyRing[keyID]) == 0 {
		t.Fatalf("generated key ring = %#v", keyRing)
	}

	assetPath := filepath.Join(temporary, "basement-linux-arm64")
	asset := make([]byte, 64)
	copy(asset, []byte{0x7f, 'E', 'L', 'F'})
	asset[18] = 183
	if err := os.WriteFile(assetPath, asset, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(temporary, "update.json")
	signaturePath := filepath.Join(temporary, "update.sig")
	signingKeyRingPath := filepath.Join(temporary, "signing-key-ring.txt")
	if err := sign(
		assetPath, "v2.0.0", keyID, "v1.0.0", manifestPath, signaturePath,
		signingKeyRingPath, bytes.NewReader(privateKey.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	if err := verify(assetPath, "v2.0.0", manifestPath, signaturePath, string(bytes.TrimSpace(keyRingBytes))); err != nil {
		t.Fatal(err)
	}
}

func TestKeygenRejectsInvalidKeyIDBeforeWriting(t *testing.T) {
	keyRingPath := filepath.Join(t.TempDir(), "key-ring.txt")
	var privateKey bytes.Buffer
	if err := keygen("Release Key", keyRingPath, &privateKey); err == nil {
		t.Fatal("keygen accepted an invalid key id")
	}
	if privateKey.Len() != 0 {
		t.Fatal("keygen wrote a private key for an invalid key id")
	}
	if _, err := os.Stat(keyRingPath); !os.IsNotExist(err) {
		t.Fatalf("public key file exists after invalid key id: %v", err)
	}
}

func TestKeygenRefusesPrivateKeyFile(t *testing.T) {
	temporary := t.TempDir()
	privateKeyPath := filepath.Join(temporary, "private-key.txt")
	privateKey, err := os.Create(privateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { privateKey.Close() })
	keyRingPath := filepath.Join(temporary, "key-ring.txt")
	if err := keygen("release-2026", keyRingPath, privateKey); err == nil {
		t.Fatal("keygen wrote the private key to a regular file")
	}
	info, err := privateKey.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("private key file size = %d", info.Size())
	}
	if _, err := os.Stat(keyRingPath); !os.IsNotExist(err) {
		t.Fatalf("public key file exists after refused private output: %v", err)
	}
}
