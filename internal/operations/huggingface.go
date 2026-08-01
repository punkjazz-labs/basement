package operations

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

type HFClient struct {
	client  *http.Client
	baseURL string
}

type hfManifest struct {
	SHA      string   `json:"sha"`
	Siblings []hfFile `json:"siblings"`
}

type hfFile struct {
	Name   string `json:"rfilename"`
	Size   int64  `json:"size"`
	BlobID string `json:"blobId"`
	LFS    *struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"lfs"`
}

type completionMarker struct {
	Repository string   `json:"repository"`
	Revision   string   `json:"revision"`
	Bytes      int64    `json:"bytes"`
	Files      []hfFile `json:"files"`
	VerifiedAt string   `json:"verified_at"`
}

func NewHFClient() *HFClient {
	return &HFClient{client: &http.Client{Timeout: 0}, baseURL: "https://huggingface.co"}
}

func (h *HFClient) Download(ctx context.Context, artifact recipe.Artifact, target string, progress Progress) (map[string]any, error) {
	manifest, err := h.manifest(ctx, artifact)
	if err != nil {
		return nil, err
	}
	if manifest.SHA != artifact.Revision {
		return nil, fmt.Errorf("repository resolved to %s, expected %s", manifest.SHA, artifact.Revision)
	}
	var total int64
	for _, file := range manifest.Siblings {
		total += file.Size
	}
	if total != artifact.ExpectedBytes {
		return nil, fmt.Errorf("snapshot size %d does not match pinned %d", total, artifact.ExpectedBytes)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}
	var completed int64
	for _, file := range manifest.Siblings {
		path, err := safeJoin(target, file.Name)
		if err != nil {
			return nil, err
		}
		if ok := verifyFile(path, file); ok {
			completed += file.Size
			if progress != nil {
				if err := progress(downloadReceipt(artifact, file.Name, completed, total, true)); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
		delta := func(written int64, existing bool) error {
			if progress != nil {
				return progress(downloadReceipt(artifact, file.Name, completed+written, total, existing))
			}
			return nil
		}
		if err := h.downloadFile(ctx, artifact, file, path, delta); err != nil {
			return nil, fmt.Errorf("download %s: %w", file.Name, err)
		}
		if !verifyFile(path, file) {
			return nil, fmt.Errorf("content verification failed for %s", file.Name)
		}
		completed += file.Size
	}
	marker := completionMarker{Repository: artifact.Repository, Revision: artifact.Revision, Bytes: total, Files: manifest.Siblings, VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := atomicJSON(filepath.Join(target, ".runonspark-complete.json"), marker, 0o640); err != nil {
		return nil, err
	}
	return map[string]any{"repository": artifact.Repository, "revision": artifact.Revision, "bytes_verified": total, "path": target}, nil
}

func (h *HFClient) Complete(artifact recipe.Artifact, target string) bool {
	data, err := os.ReadFile(filepath.Join(target, ".runonspark-complete.json"))
	if err != nil {
		return false
	}
	var marker completionMarker
	if json.Unmarshal(data, &marker) != nil || marker.Repository != artifact.Repository || marker.Revision != artifact.Revision || marker.Bytes != artifact.ExpectedBytes || len(marker.Files) == 0 {
		return false
	}
	var total int64
	for _, file := range marker.Files {
		path, err := safeJoin(target, file.Name)
		if err != nil || !verifyFile(path, file) {
			return false
		}
		total += file.Size
	}
	return total == artifact.ExpectedBytes
}

func (h *HFClient) CheckAccess(ctx context.Context, artifact recipe.Artifact) (map[string]any, error) {
	manifest, err := h.manifest(ctx, artifact)
	if err != nil {
		return nil, err
	}
	if manifest.SHA != artifact.Revision {
		return nil, fmt.Errorf("repository resolved to %s, expected %s", manifest.SHA, artifact.Revision)
	}
	var total int64
	for _, file := range manifest.Siblings {
		total += file.Size
	}
	if total != artifact.ExpectedBytes {
		return nil, fmt.Errorf("snapshot size %d does not match pinned %d", total, artifact.ExpectedBytes)
	}
	return map[string]any{"repository": artifact.Repository, "revision": manifest.SHA, "expected_bytes": total, "accessible": true}, nil
}

func (h *HFClient) manifest(ctx context.Context, artifact recipe.Artifact) (hfManifest, error) {
	endpoint := h.baseURL + "/api/models/" + escapeRepository(artifact.Repository) + "/revision/" + url.PathEscape(artifact.Revision) + "?blobs=true"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if token := os.Getenv("HF_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return hfManifest{}, fmt.Errorf("fetch model manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hfManifest{}, fmt.Errorf("model manifest returned %s", resp.Status)
	}
	var manifest hfManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&manifest); err != nil {
		return hfManifest{}, err
	}
	for i := range manifest.Siblings {
		if manifest.Siblings[i].LFS != nil && manifest.Siblings[i].Size == 0 {
			manifest.Siblings[i].Size = manifest.Siblings[i].LFS.Size
		}
	}
	return manifest, nil
}

func (h *HFClient) downloadFile(ctx context.Context, artifact recipe.Artifact, file hfFile, finalPath string, progress func(written int64, existing bool) error) error {
	tempPath := finalPath + ".part"
	var offset int64
	if stat, err := os.Stat(tempPath); err == nil {
		offset = stat.Size()
		if offset == file.Size && verifyFile(tempPath, file) {
			if err := os.Rename(tempPath, finalPath); err != nil {
				return err
			}
			return progress(offset, true)
		}
		if offset > file.Size {
			_ = os.Remove(tempPath)
			offset = 0
		}
	}
	if err := progress(offset, true); err != nil {
		return fmt.Errorf("check download capacity: %w", err)
	}
	endpoint := h.baseURL + "/" + escapeRepository(artifact.Repository) + "/resolve/" + url.PathEscape(artifact.Revision) + "/" + escapeFilePath(file.Name) + "?download=true"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if token := os.Getenv("HF_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 && resp.StatusCode == http.StatusPartialContent {
		flags |= os.O_APPEND
	} else {
		offset = 0
		flags |= os.O_TRUNC
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	out, err := os.OpenFile(tempPath, flags, 0o640)
	if err != nil {
		return err
	}
	defer out.Close()
	buffer := make([]byte, 4<<20)
	written := offset
	lastReport := time.Now()
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := out.Write(buffer[:n]); err != nil {
				return err
			}
			written += int64(n)
			if time.Since(lastReport) >= time.Second {
				if err := progress(written, false); err != nil {
					return err
				}
				lastReport = time.Now()
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := progress(written, false); err != nil {
		return err
	}
	return os.Rename(tempPath, finalPath)
}

func verifyFile(path string, file hfFile) bool {
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != file.Size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var digest hash.Hash
	var expected string
	if file.LFS != nil && file.LFS.SHA256 != "" {
		digest, expected = sha256.New(), file.LFS.SHA256
	} else if len(file.BlobID) == 40 {
		digest, expected = sha1.New(), file.BlobID
		_, _ = io.WriteString(digest, "blob "+strconv.FormatInt(file.Size, 10)+"\x00")
	} else {
		return false
	}
	if _, err := io.Copy(digest, f); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expected)
}

// downloadReceipt reports transfer progress; checkingExisting marks bytes
// that were already on disk (resume verification), which move at disk speed
// and must not be mistaken for network throughput.
func downloadReceipt(artifact recipe.Artifact, file string, completed, total int64, checkingExisting bool) map[string]any {
	percent := float64(0)
	if total > 0 {
		percent = float64(completed) * 100 / float64(total)
	}
	return map[string]any{"repository": artifact.Repository, "revision": artifact.Revision, "file": file, "bytes_complete": completed, "bytes_total": total, "percent": percent, "checking_existing": checkingExisting}
}

func safeJoin(root, name string) (string, error) {
	if filepath.IsAbs(name) || strings.Contains(name, "\\") {
		return "", errors.New("artifact path is unsafe")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("artifact path traversal denied")
	}
	path := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("artifact path escaped root")
	}
	return path, nil
}

func escapeRepository(repository string) string {
	parts := strings.Split(repository, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func escapeFilePath(name string) string {
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func atomicJSON(path string, value any, mode os.FileMode) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".atomic-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(body, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
