package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
)

type fakeReleaseSource struct {
	releases []Release
	payloads map[string][]byte
}

func (source fakeReleaseSource) Releases(context.Context) ([]Release, error) {
	return source.releases, nil
}

func (source fakeReleaseSource) Fetch(_ context.Context, location string, _ int64) ([]byte, error) {
	payload, ok := source.payloads[location]
	if !ok {
		return nil, fmt.Errorf("missing fixture %s", location)
	}
	return append([]byte(nil), payload...), nil
}

func TestResolverChoosesNewestReachableHop(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	source := fakeReleaseSource{payloads: make(map[string][]byte)}
	for _, fixture := range []struct {
		version      string
		rollbackFrom string
	}{
		{version: "v4.0.0", rollbackFrom: "v3.0.0"},
		{version: "v2.0.0", rollbackFrom: "v1.0.0"},
		{version: "v3.0.0", rollbackFrom: "v2.0.0"},
	} {
		release, manifest, signature := signedReleaseFixture(t, privateKey, fixture.version, fixture.rollbackFrom)
		source.releases = append(source.releases, release)
		source.payloads[release.Assets[0].URL] = manifest
		source.payloads[release.Assets[1].URL] = signature
	}
	resolver := Resolver{Source: source, Keys: KeyRing{"release-test": publicKey}}

	first, err := resolver.Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if first.Candidate == nil || first.Candidate.Manifest.ReleaseVersion != "v2.0.0" {
		t.Fatalf("first hop = %#v, want v2.0.0", first.Candidate)
	}

	second, err := resolver.Resolve(context.Background(), "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if second.Candidate == nil || second.Candidate.Manifest.ReleaseVersion != "v3.0.0" {
		t.Fatalf("second hop = %#v, want v3.0.0", second.Candidate)
	}
}

func TestResolverReportsManualUpgradeWhenNoHopIsReachable(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	release, manifest, signature := signedReleaseFixture(t, privateKey, "v3.0.0", "v2.0.0")
	source := fakeReleaseSource{releases: []Release{release}, payloads: map[string][]byte{
		release.Assets[0].URL: manifest,
		release.Assets[1].URL: signature,
	}}
	resolution, err := (Resolver{Source: source, Keys: KeyRing{"release-test": publicKey}}).Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Candidate != nil || !resolution.ManualUpgrade {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func signedReleaseFixture(t *testing.T, privateKey ed25519.PrivateKey, version, rollbackFrom string) (Release, []byte, []byte) {
	t.Helper()
	digest := sha256.Sum256([]byte(version))
	manifest, err := MarshalManifest(Manifest{
		SchemaVersion: ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: version,
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: int64(len(version)), AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: UpdaterProtocol,
		RollbackFrom: []string{rollbackFrom},
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))), '\n')
	prefix := "https://github.com/example/releases/download/" + version + "/"
	release := Release{TagName: version, HTMLURL: "https://github.com/example/releases/tag/" + version, Assets: []ReleaseAsset{
		{Name: ManifestAssetName, URL: prefix + ManifestAssetName},
		{Name: SignatureAssetName, URL: prefix + SignatureAssetName},
		{Name: LinuxARM64AssetName, URL: prefix + LinuxARM64AssetName},
	}}
	return release, manifest, signature
}
