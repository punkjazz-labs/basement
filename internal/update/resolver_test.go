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

// The whole transition depends on this. A release whose manifest this build
// refuses, because its schema or its updater protocol is newer, must leave
// every older release still reachable. Aborting the check on the first
// refusal would strand a machine on the release it already has, with no way
// back to the stepping stone that would carry it forward.
func TestResolverSkipsARefusedManifestAndFallsBack(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	source := fakeReleaseSource{payloads: make(map[string][]byte)}
	reachable, manifest, signature := signedReleaseFixture(t, privateKey, "v2.0.0", "v1.0.0")
	source.releases = append(source.releases, reachable)
	source.payloads[reachable.Assets[0].URL] = manifest
	source.payloads[reachable.Assets[1].URL] = signature

	// v3.0.0 is signed by the same key and is perfectly valid, for a manager
	// one protocol newer than this one.
	refused, refusedManifest, refusedSignature := futureReleaseFixture(t, privateKey, "v3.0.0", "v2.0.0")
	source.releases = append(source.releases, refused)
	source.payloads[refused.Assets[0].URL] = refusedManifest
	source.payloads[refused.Assets[1].URL] = refusedSignature

	resolution, err := (Resolver{Source: source, Keys: KeyRing{"release-test": publicKey}}).Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("a refused manifest aborted the whole check: %v", err)
	}
	if resolution.Candidate == nil || resolution.Candidate.Manifest.ReleaseVersion != "v2.0.0" {
		t.Fatalf("candidate = %#v, want the newest release this build can read", resolution.Candidate)
	}
	if resolution.NewestPublished != "v3.0.0" {
		t.Fatalf("newest published = %q, the console still reports what exists", resolution.NewestPublished)
	}
}

// A schema-2 release names a second signed binary, and its URL is carried
// from the release listing rather than derived from the manager asset URL.
func TestResolverCarriesTheHelperAssetURL(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	release, manifest, signature := signedHelperReleaseFixture(t, privateKey, "v2.0.0", "v1.0.0")
	source := fakeReleaseSource{releases: []Release{release}, payloads: map[string][]byte{
		release.Assets[0].URL: manifest, release.Assets[1].URL: signature,
	}}
	resolution, err := (Resolver{Source: source, Keys: KeyRing{"release-test": publicKey}}).Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Candidate == nil {
		t.Fatal("a schema 2 release was not resolved by a protocol 2 manager")
	}
	want := "https://github.com/example/releases/download/v2.0.0/" + LinuxARM64HelperAssetName
	if resolution.Candidate.HelperAssetURL != want {
		t.Fatalf("helper asset URL = %q, want %q", resolution.Candidate.HelperAssetURL, want)
	}
}

// A schema-1 release names no helper, and a release missing the helper asset
// must still deliver its manager rather than being skipped over a binary the
// machine may already have.
func TestResolverAcceptsAReleaseWithoutAHelperAsset(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	release, manifest, signature := signedReleaseFixture(t, privateKey, "v2.0.0", "v1.0.0")
	source := fakeReleaseSource{releases: []Release{release}, payloads: map[string][]byte{
		release.Assets[0].URL: manifest, release.Assets[1].URL: signature,
	}}
	resolution, err := (Resolver{Source: source, Keys: KeyRing{"release-test": publicKey}}).Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Candidate == nil || resolution.Candidate.HelperAssetURL != "" {
		t.Fatalf("candidate = %#v, want the manager release with no helper URL", resolution.Candidate)
	}
}

// futureReleaseFixture signs a manifest this build must refuse: the bytes
// verify, the key is right, and the schema is one nobody here understands.
func futureReleaseFixture(t *testing.T, privateKey ed25519.PrivateKey, version, rollbackFrom string) (Release, []byte, []byte) {
	t.Helper()
	digest := sha256.Sum256([]byte(version))
	manifest := []byte(fmt.Sprintf(
		`{"schema_version":99,"key_id":"release-test","release_version":%q,"os":"linux","arch":"arm64",`+
			`"asset_name":%q,"asset_size":%d,"asset_sha256":%q,"updater_protocol":99,"rollback_from":[%q]}`+"\n",
		version, LinuxARM64AssetName, len(version), hex.EncodeToString(digest[:]), rollbackFrom))
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))), '\n')
	return releaseWithAssets(version, false), manifest, signature
}

func signedHelperReleaseFixture(t *testing.T, privateKey ed25519.PrivateKey, version, rollbackFrom string) (Release, []byte, []byte) {
	t.Helper()
	digest := sha256.Sum256([]byte(version))
	helperDigest := sha256.Sum256([]byte(version + "-helper"))
	manifest, err := MarshalManifest(Manifest{
		SchemaVersion: ManifestHelperSchemaVersion, KeyID: "release-test", ReleaseVersion: version,
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName,
		AssetSize: int64(len(version)), AssetSHA256: hex.EncodeToString(digest[:]), UpdaterProtocol: 2,
		RollbackFrom:    []string{rollbackFrom},
		HelperAssetName: LinuxARM64HelperAssetName, HelperSize: 64, HelperSHA256: hex.EncodeToString(helperDigest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))), '\n')
	return releaseWithAssets(version, true), manifest, signature
}

func releaseWithAssets(version string, withHelper bool) Release {
	prefix := "https://github.com/example/releases/download/" + version + "/"
	release := Release{TagName: version, HTMLURL: "https://github.com/example/releases/tag/" + version, Assets: []ReleaseAsset{
		{Name: ManifestAssetName, URL: prefix + ManifestAssetName},
		{Name: SignatureAssetName, URL: prefix + SignatureAssetName},
		{Name: LinuxARM64AssetName, URL: prefix + LinuxARM64AssetName},
	}}
	if withHelper {
		release.Assets = append(release.Assets, ReleaseAsset{Name: LinuxARM64HelperAssetName, URL: prefix + LinuxARM64HelperAssetName})
	}
	return release
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
