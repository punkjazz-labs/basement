package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

func TestHuggingFaceDownloadResumesAndVerifies(t *testing.T) {
	content := []byte("verified model bytes")
	sum := sha256.Sum256(content)
	revision := strings.Repeat("a", 40)
	rangeSeen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": revision, "siblings": []map[string]any{{"rfilename": "weights/model.bin", "size": len(content), "blobId": strings.Repeat("b", 40), "lfs": map[string]any{"sha256": hex.EncodeToString(sum[:]), "size": len(content)}}}})
		case strings.Contains(r.URL.Path, "/resolve/"):
			start := 0
			if value := r.Header.Get("Range"); value != "" {
				rangeSeen = true
				_, _ = fmt.Sscanf(value, "bytes=%d-", &start)
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = w.Write(content[start:])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	artifact := recipe.Artifact{Role: "primary", Repository: "owner/model", Revision: revision, ExpectedBytes: int64(len(content)), Licence: "test", LicenceURL: server.URL}
	target := filepath.Join(t.TempDir(), "artifact")
	if err := os.MkdirAll(filepath.Join(target, "weights"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "weights/model.bin.part"), content[:5], 0o640); err != nil {
		t.Fatal(err)
	}
	var last map[string]any
	receipt, err := client.Download(context.Background(), artifact, target, func(value any) error { last = value.(map[string]any); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !rangeSeen {
		t.Fatal("partial download was not resumed")
	}
	if receipt["bytes_verified"] != int64(len(content)) {
		t.Fatalf("receipt=%#v", receipt)
	}
	if last["percent"].(float64) != 100 {
		t.Fatalf("progress=%#v", last)
	}
	if !client.Complete(artifact, target) {
		t.Fatal("verified completion marker missing")
	}
	downloaded, _ := os.ReadFile(filepath.Join(target, "weights/model.bin"))
	if string(downloaded) != string(content) {
		t.Fatalf("download=%q", downloaded)
	}
	if err := os.WriteFile(filepath.Join(target, "weights/model.bin"), []byte("corrupted model byte"), 0o640); err != nil {
		t.Fatal(err)
	}
	if client.Complete(artifact, target) {
		t.Fatal("corrupted completed artifact was reused")
	}
}

// An artifact verified before the basement rename (spec 10) only has the
// pre-rename marker file on disk; Complete must still recognize it (falling
// back to reading it, never writing it) so an upgraded manager does not
// force a multi-gigabyte re-download of weights it already verified.
func TestHuggingFaceCompleteFallsBackToPreRenameMarker(t *testing.T) {
	content := []byte("already verified before the rename")
	sum := sha256.Sum256(content)
	revision := strings.Repeat("a", 40)
	target := filepath.Join(t.TempDir(), "artifact")
	if err := os.MkdirAll(filepath.Join(target, "weights"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "weights/model.bin"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	marker := completionMarker{
		Repository: "owner/model", Revision: revision, Bytes: int64(len(content)),
		Files: []hfFile{{
			Name: "weights/model.bin", Size: int64(len(content)),
			LFS: &struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			}{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))},
		}},
		VerifiedAt: "2026-01-01T00:00:00Z",
	}
	if err := atomicJSON(filepath.Join(target, legacyCompletionMarkerName), marker, 0o640); err != nil {
		t.Fatal(err)
	}
	artifact := recipe.Artifact{Role: "primary", Repository: "owner/model", Revision: revision, ExpectedBytes: int64(len(content))}
	client := &HFClient{}
	if !client.Complete(artifact, target) {
		t.Fatal("pre-rename completion marker was not honored")
	}
	if _, err := os.Stat(filepath.Join(target, completionMarkerName)); err == nil {
		t.Fatal("Complete must never write the current-name marker; it only reads and falls back")
	}
}

func TestHuggingFaceManifestPathTraversalIsRejected(t *testing.T) {
	revision := strings.Repeat("a", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": revision, "siblings": []map[string]any{{"rfilename": "../escape", "size": 1, "blobId": strings.Repeat("b", 40)}}})
	}))
	defer server.Close()
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	_, err := client.Download(context.Background(), recipe.Artifact{Repository: "owner/model", Revision: revision, ExpectedBytes: 1}, filepath.Join(t.TempDir(), "artifact"), nil)
	if err == nil || !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("Download()=%v", err)
	}
}

func TestHuggingFaceChecksCapacityBeforeStartingFileTransfer(t *testing.T) {
	revision := strings.Repeat("a", 40)
	fileRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": revision, "siblings": []map[string]any{{"rfilename": "model.bin", "size": 10, "blobId": strings.Repeat("b", 40)}}})
			return
		}
		fileRequested = true
		_, _ = w.Write(make([]byte, 10))
	}))
	defer server.Close()
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	artifact := recipe.Artifact{Repository: "owner/model", Revision: revision, ExpectedBytes: 10}
	_, err := client.Download(context.Background(), artifact, filepath.Join(t.TempDir(), "artifact"), func(any) error {
		return fmt.Errorf("disk reserve reached")
	})
	if err == nil || !strings.Contains(err.Error(), "disk reserve reached") {
		t.Fatalf("Download()=%v", err)
	}
	if fileRequested {
		t.Fatal("file transfer began before capacity guard passed")
	}
}

// downloadFile hashes bytes as it writes them, so a completed transfer
// never has to read the file back to prove it. Every assertion below is
// on the flag downloadFile returns, which is true only when the
// streaming digest matched; a re-read would report through verifyFile
// instead and would not set it.

func resolveServer(t *testing.T, body []byte, ranged *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/") {
			http.NotFound(w, r)
			return
		}
		start := 0
		if value := r.Header.Get("Range"); value != "" {
			if ranged != nil {
				*ranged++
			}
			_, _ = fmt.Sscanf(value, "bytes=%d-", &start)
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write(body[start:])
	}))
	t.Cleanup(server.Close)
	return server
}

func lfsSibling(name string, size int64, sum string) hfFile {
	return hfFile{Name: name, Size: size, LFS: &struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}{SHA256: sum, Size: size}}
}

func TestDownloadFileStreamsLFSDigest(t *testing.T) {
	// sha256("abc"), a published vector rather than one this test computes.
	file := lfsSibling("model.bin", 3, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	server := resolveServer(t, []byte("abc"), nil)
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	path := filepath.Join(t.TempDir(), "model.bin")
	verified, err := client.downloadFile(context.Background(), recipe.Artifact{Repository: "owner/model", Revision: strings.Repeat("a", 40)}, file, path, func(int64, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("streamed sha256 did not verify the download")
	}
	if body, _ := os.ReadFile(path); string(body) != "abc" {
		t.Fatalf("download=%q", body)
	}
	if !verifyFile(path, file) {
		t.Fatal("streamed digest disagrees with a full re-read")
	}
}

func TestDownloadFileStreamsGitBlobDigest(t *testing.T) {
	// git hash-object of "hello\n": sha1 over "blob 6\x00hello\n".
	file := hfFile{Name: "config.json", Size: 6, BlobID: "ce013625030ba8dba906f756967f9e9ca394464a"}
	server := resolveServer(t, []byte("hello\n"), nil)
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	path := filepath.Join(t.TempDir(), "config.json")
	verified, err := client.downloadFile(context.Background(), recipe.Artifact{Repository: "owner/model", Revision: strings.Repeat("a", 40)}, file, path, func(int64, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("streamed git-blob sha1 did not verify the download")
	}
	if !verifyFile(path, file) {
		t.Fatal("streamed digest disagrees with a full re-read")
	}
	if verifyFile(path, hfFile{Name: "config.json", Size: 6, BlobID: strings.Repeat("d", 40)}) {
		t.Fatal("git-blob verification accepted a wrong blob id")
	}
}

func TestDownloadFileRejectsCorruptedBody(t *testing.T) {
	revision := strings.Repeat("a", 40)
	corrupt := []byte("xyz")
	file := lfsSibling("model.bin", 3, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": revision, "siblings": []map[string]any{{"rfilename": "model.bin", "size": 3, "lfs": map[string]any{"sha256": file.LFS.SHA256, "size": 3}}}})
			return
		}
		_, _ = w.Write(corrupt)
	}))
	defer server.Close()
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	target := filepath.Join(t.TempDir(), "artifact")
	path := filepath.Join(target, "model.bin")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	verified, err := client.downloadFile(context.Background(), recipe.Artifact{Repository: "owner/model", Revision: revision}, file, path, func(int64, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if verified {
		t.Fatal("corrupted body passed the streamed digest")
	}
	artifact := recipe.Artifact{Repository: "owner/model", Revision: revision, ExpectedBytes: 3}
	_, err = client.Download(context.Background(), artifact, filepath.Join(t.TempDir(), "artifact"), nil)
	if err == nil || !strings.Contains(err.Error(), "content verification failed") {
		t.Fatalf("Download()=%v", err)
	}
}

func TestDownloadFileResumeHashesExistingPrefix(t *testing.T) {
	file := lfsSibling("model.bin", 3, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	ranged := 0
	server := resolveServer(t, []byte("abc"), &ranged)
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	path := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(path+".part", []byte("a"), 0o640); err != nil {
		t.Fatal(err)
	}
	verified, err := client.downloadFile(context.Background(), recipe.Artifact{Repository: "owner/model", Revision: strings.Repeat("a", 40)}, file, path, func(int64, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ranged != 1 {
		t.Fatalf("resume did not issue a ranged request: %d", ranged)
	}
	if !verified {
		t.Fatal("resumed transfer did not verify from the prefix hash")
	}
	if body, _ := os.ReadFile(path); string(body) != "abc" {
		t.Fatalf("download=%q", body)
	}
}

func TestDownloadFileResumeRejectsCorruptedPrefix(t *testing.T) {
	file := lfsSibling("model.bin", 3, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	server := resolveServer(t, []byte("abc"), nil)
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	path := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(path+".part", []byte("z"), 0o640); err != nil {
		t.Fatal(err)
	}
	verified, err := client.downloadFile(context.Background(), recipe.Artifact{Repository: "owner/model", Revision: strings.Repeat("a", 40)}, file, path, func(int64, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if verified {
		t.Fatal("resume accepted a prefix that was never part of the file")
	}
	if verifyFile(path, file) {
		t.Fatal("a full re-read should also reject the salvaged prefix")
	}
}

func TestDownloadFileWithoutAKnownDigestFallsBackToVerifyFile(t *testing.T) {
	file := hfFile{Name: "model.bin", Size: 3, BlobID: "short"}
	server := resolveServer(t, []byte("abc"), nil)
	client := &HFClient{client: server.Client(), baseURL: server.URL}
	path := filepath.Join(t.TempDir(), "model.bin")
	verified, err := client.downloadFile(context.Background(), recipe.Artifact{Repository: "owner/model", Revision: strings.Repeat("a", 40)}, file, path, func(int64, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if verified {
		t.Fatal("a file with no usable digest must not report itself verified")
	}
}

func TestHashReaderMatchesFullRead(t *testing.T) {
	body := make([]byte, (64<<20)+4096)
	for i := range body {
		body[i] = byte(i)
	}
	path := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	digest := sha256.New()
	if err := hashReader(digest, f, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)
	if hex.EncodeToString(digest.Sum(nil)) != hex.EncodeToString(want[:]) {
		t.Fatal("windowed hashing over more than one eviction window changed the digest")
	}
}

func TestVLLMArgumentsAreStructuredAndPinned(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}
	args := vllmArgs(r, Placement{})
	joined := strings.Join(args, " ")
	for _, required := range []string{"serve /model", "--reasoning-parser qwen3", "--tool-call-parser qwen3_coder", "--linear-backend flashinfer_b12x", "--speculative-config", "--served-model-name unsloth/Qwen3.6-35B-A3B-NVFP4"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("arguments missing %q: %s", required, joined)
		}
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, ";\n\r") {
			t.Fatalf("shell-like argument accepted: %q", arg)
		}
	}
}
