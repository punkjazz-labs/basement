package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestStagerSeparatesFleetVerificationFromUpdaterHandoff(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager := arm64ELF([]byte("fleet target"))
	digest := sha256.Sum256(manager)
	manifestBytes, err := MarshalManifest(Manifest{SchemaVersion: ManifestSchemaVersion, KeyID: "release-test", ReleaseVersion: "v2.0.0",
		OS: "linux", Arch: "arm64", AssetName: LinuxARM64AssetName, AssetSize: int64(len(manager)), AssetSHA256: hex.EncodeToString(digest[:]),
		UpdaterProtocol: UpdaterProtocol, RollbackFrom: []string{"v1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifestBytes))), '\n')
	candidate := Candidate{Release: Release{TagName: "v2.0.0"}, ManifestBytes: manifestBytes, Signature: signature,
		AssetURL: "https://github.com/example/releases/download/v2.0.0/" + LinuxARM64AssetName}
	root := t.TempDir()
	stager := NewStager(root, KeyRing{"release-test": publicKey})
	stager.BootstrapCheck = func() error { return nil }
	stager.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(manager)), Body: io.NopCloser(bytes.NewReader(manager)), Header: make(http.Header)}, nil
	})}
	status, err := stager.Prepare(candidate, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	status, err = stager.StageOnly(context.Background(), candidate, status)
	if err != nil || status.State != "staged" {
		t.Fatalf("stage status=%+v err=%v", status, err)
	}
	requestPath := filepath.Join(root, "updates", "staging", "pending", requestFileName)
	if _, err := os.Stat(requestPath); !os.IsNotExist(err) {
		t.Fatalf("verification barrier created an updater request: %v", err)
	}
	status, err = stager.ApplyStaged(status)
	if err != nil || status.State != "waiting_for_root" {
		t.Fatalf("handoff status=%+v err=%v", status, err)
	}
	if _, err := os.Stat(requestPath); err != nil {
		t.Fatalf("handoff did not create updater request: %v", err)
	}
}
