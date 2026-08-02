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

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
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

func TestVLLMArgumentsAreStructuredAndPinned(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("Qwen 35 recipe missing")
	}
	args := vllmArgs(r)
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
