package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
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

// A protocol-2 manager reads both schemas, and each schema stays strict about
// the fields the other one owns. Getting this matrix wrong in either
// direction breaks the transition: too strict strands machines on schema 1,
// too loose accepts a release whose helper identity means nothing.
func TestManifestSchemaMatrix(t *testing.T) {
	base := func() Manifest {
		digest := sha256.Sum256([]byte("asset"))
		return Manifest{
			SchemaVersion: ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
			OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
			AssetSize: 5, AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: 1,
			RollbackFrom: []string{"v1.0.0"},
		}
	}
	withHelper := func() Manifest {
		manifest := base()
		helperDigest := sha256.Sum256([]byte("helper"))
		manifest.SchemaVersion = ManifestHelperSchemaVersion
		manifest.UpdaterProtocol = 2
		manifest.HelperAssetName = LinuxARM64HelperAssetName
		manifest.HelperSize = 64
		manifest.HelperSHA256 = hex.EncodeToString(helperDigest[:])
		return manifest
	}
	cases := []struct {
		name     string
		manifest Manifest
		accepted bool
	}{
		{name: "schema 1 as published before protocol 2", manifest: base(), accepted: true},
		{name: "schema 1 republished at protocol 2", manifest: func() Manifest {
			manifest := base()
			manifest.UpdaterProtocol = 2
			return manifest
		}(), accepted: true},
		{name: "schema 2 with a signed helper", manifest: withHelper(), accepted: true},
		{name: "schema 1 carrying a helper digest", manifest: func() Manifest {
			manifest := base()
			manifest.HelperSHA256 = withHelper().HelperSHA256
			return manifest
		}()},
		{name: "schema 2 without a helper digest", manifest: func() Manifest {
			manifest := withHelper()
			manifest.HelperSHA256 = ""
			return manifest
		}()},
		{name: "schema 2 naming another asset", manifest: func() Manifest {
			manifest := withHelper()
			manifest.HelperAssetName = "basement-linux-arm64"
			return manifest
		}()},
		{name: "schema 2 with a zero helper size", manifest: func() Manifest {
			manifest := withHelper()
			manifest.HelperSize = 0
			return manifest
		}()},
		{name: "schema 2 claiming protocol 1", manifest: func() Manifest {
			manifest := withHelper()
			manifest.UpdaterProtocol = 1
			return manifest
		}()},
		{name: "a schema this build does not know", manifest: func() Manifest {
			manifest := withHelper()
			manifest.SchemaVersion = 3
			return manifest
		}()},
		{name: "a protocol this build does not speak", manifest: func() Manifest {
			manifest := withHelper()
			manifest.UpdaterProtocol = 3
			return manifest
		}()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := MarshalManifest(testCase.manifest)
			if testCase.accepted && err != nil {
				t.Fatalf("a manifest a protocol-2 manager must read was refused: %v", err)
			}
			if !testCase.accepted && err == nil {
				t.Fatal("an inconsistent manifest was accepted")
			}
		})
	}
}

// The helper fields are appended last and omitted when empty, so a schema-1
// release still marshals to exactly the bytes it always did. A byte that
// moved would invalidate every signature already published.
func TestSchemaOneManifestBytesAreUnchanged(t *testing.T) {
	digest := sha256.Sum256([]byte("asset"))
	payload, err := MarshalManifest(Manifest{
		SchemaVersion: ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: 5, AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: 1,
		RollbackFrom: []string{"v1.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"schema_version":1,"key_id":"release-test","release_version":"v2.0.0","os":"linux","arch":"arm64",` +
		`"asset_name":"basement-linux-arm64","asset_size":5,"asset_sha256":"` + hex.EncodeToString(digest[:]) +
		`","updater_protocol":1,"rollback_from":["v1.0.0"]}` + "\n"
	if string(payload) != expected {
		t.Fatalf("schema 1 manifest bytes changed:\n got %s", payload)
	}
}

// A schema-2 manifest reaches a protocol-1 manager as an unknown field as
// well as an unknown schema. Both refusals are the transition working.
func TestSchemaTwoManifestCarriesTheHelperFields(t *testing.T) {
	digest := sha256.Sum256([]byte("asset"))
	helperDigest := sha256.Sum256([]byte("helper"))
	payload, err := MarshalManifest(Manifest{
		SchemaVersion: ManifestHelperSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: 5, AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: 2,
		RollbackFrom:    []string{"v1.0.0"},
		HelperAssetName: LinuxARM64HelperAssetName, HelperSize: 64, HelperSHA256: hex.EncodeToString(helperDigest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"helper_asset_name":"basement-updater-linux-arm64"`, `"helper_size":64`, `"helper_sha256":"` + hex.EncodeToString(helperDigest[:]) + `"`} {
		if !strings.Contains(string(payload), field) {
			t.Fatalf("schema 2 manifest is missing %s:\n%s", field, payload)
		}
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
