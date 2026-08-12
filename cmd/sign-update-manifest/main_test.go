package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	assetPath := writeELF(t, filepath.Join(temporary, "basement-linux-arm64"), "manager")
	manifestPath := filepath.Join(temporary, "update.json")
	signaturePath := filepath.Join(temporary, "update.sig")
	signingKeyRingPath := filepath.Join(temporary, "signing-key-ring.txt")
	if err := sign(
		assetPath, "", "v2.0.0", keyID, "v1.0.0", manifestPath, signaturePath,
		signingKeyRingPath, bytes.NewReader(privateKey.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	if err := verify(assetPath, "", "v2.0.0", manifestPath, signaturePath, string(bytes.TrimSpace(keyRingBytes))); err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, manifestPath)
	if manifest.SchemaVersion != update.ManifestSchemaVersion || manifest.UpdaterProtocol != 1 {
		t.Fatalf("manifest = %+v, a release signed without a helper stays readable by a protocol-1 manager", manifest)
	}
	if manifest.HelperAssetName != "" || manifest.HelperSize != 0 || manifest.HelperSHA256 != "" {
		t.Fatalf("schema 1 manifest carries a helper identity: %+v", manifest)
	}
}

// Signing the helper is what makes a release schema 2, and schema 2 is what
// every manager older than protocol 2 refuses. That is the whole transition
// lever, so the flag has to be the only thing that decides it.
func TestSigningTheHelperEmitsSchemaTwoAndVerifiesBothBinaries(t *testing.T) {
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
	assetPath := writeELF(t, filepath.Join(temporary, "basement-linux-arm64"), "manager")
	helperPath := writeELF(t, filepath.Join(temporary, update.LinuxARM64HelperAssetName), "helper")
	manifestPath := filepath.Join(temporary, "update.json")
	signaturePath := filepath.Join(temporary, "update.sig")
	if err := sign(
		assetPath, helperPath, "v2.0.0", keyID, "v1.0.0", manifestPath, signaturePath,
		filepath.Join(temporary, "signing-key-ring.txt"), bytes.NewReader(privateKey.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	keyRingValue := string(bytes.TrimSpace(keyRingBytes))
	if err := verify(assetPath, helperPath, "v2.0.0", manifestPath, signaturePath, keyRingValue); err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, manifestPath)
	if manifest.SchemaVersion != update.ManifestHelperSchemaVersion || manifest.UpdaterProtocol != 2 {
		t.Fatalf("manifest = %+v, want schema 2 at protocol 2", manifest)
	}
	if manifest.HelperAssetName != update.LinuxARM64HelperAssetName || manifest.HelperSize != 64 {
		t.Fatalf("manifest helper identity = %+v", manifest)
	}
	digest := sha256.Sum256(elfBytes("helper"))
	if manifest.HelperSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("manifest helper digest = %q, want the measured helper", manifest.HelperSHA256)
	}

	// A helper whose bytes drifted after signing must fail the verify pass
	// rather than reaching the release page.
	if err := os.WriteFile(helperPath, elfBytes("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(assetPath, helperPath, "v2.0.0", manifestPath, signaturePath, keyRingValue); err == nil {
		t.Fatal("verify accepted a helper that does not match the signed manifest")
	}
	if err := verify(assetPath, "", "v2.0.0", manifestPath, signaturePath, keyRingValue); err == nil {
		t.Fatal("verify skipped the helper a schema 2 manifest names")
	}
}

func elfBytes(suffix string) []byte {
	payload := make([]byte, 64)
	copy(payload, []byte{0x7f, 'E', 'L', 'F'})
	payload[18] = 183
	copy(payload[32:], suffix)
	return payload
}

func writeELF(t *testing.T, path, suffix string) string {
	t.Helper()
	if err := os.WriteFile(path, elfBytes(suffix), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readManifest(t *testing.T, path string) update.Manifest {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest update.Manifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
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
