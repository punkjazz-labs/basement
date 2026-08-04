package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestVerifySignedManifest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := testManifestBytes(t, "release-test")
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))), '\n')

	manifest, err := VerifySignedManifest(payload, signature, KeyRing{"release-test": publicKey})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ReleaseVersion != "v2.0.0" {
		t.Fatalf("release version = %q", manifest.ReleaseVersion)
	}
	if err := ValidateCandidate(manifest, "v2.0.0", "v1.5.0"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifySignedManifestRejectsTampering(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := testManifestBytes(t, "release-test")
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))), '\n')
	tampered := append([]byte(nil), payload...)
	tampered[len(tampered)-2] ^= 1
	if _, err := VerifySignedManifest(tampered, signature, KeyRing{"release-test": publicKey}); err == nil {
		t.Fatal("tampered manifest was accepted")
	}
}

func TestVerifySignedManifestRejectsWrongKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := testManifestBytes(t, "release-test")
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))), '\n')
	if _, err := VerifySignedManifest(payload, signature, KeyRing{"release-test": wrongPublicKey}); err == nil {
		t.Fatal("signature from the wrong key was accepted")
	}
}

func TestVerifySignedManifestRejectsTrailingObject(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := append(testManifestBytes(t, "release-test"), []byte("{}\n")...)
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))), '\n')
	if _, err := VerifySignedManifest(payload, signature, KeyRing{"release-test": publicKey}); err == nil {
		t.Fatal("manifest with a trailing object was accepted")
	}
}

func testManifestBytes(t *testing.T, keyID string) []byte {
	t.Helper()
	digest := sha256.Sum256([]byte("asset"))
	payload, err := MarshalManifest(Manifest{
		SchemaVersion: ManifestSchemaVersion, KeyID: keyID, ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: 5, AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: UpdaterProtocol,
		RollbackFrom: []string{"v1.5.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
