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

	"github.com/punkjazz-labs/basement/internal/recipe"
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
	files, total, err := selectFiles(artifact, manifest)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}
	var completed int64
	for _, file := range files {
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
		verified, err := h.downloadFile(ctx, artifact, file, path, delta)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", file.Name, err)
		}
		if !verified && !verifyFile(path, file) {
			return nil, fmt.Errorf("content verification failed for %s", file.Name)
		}
		completed += file.Size
	}
	marker := completionMarker{Repository: artifact.Repository, Revision: artifact.Revision, Bytes: total, Files: files, VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := atomicJSON(filepath.Join(target, completionMarkerName), marker, 0o640); err != nil {
		return nil, err
	}
	return map[string]any{"repository": artifact.Repository, "revision": artifact.Revision, "bytes_verified": total, "path": target}, nil
}

// completionMarkerName records a fully verified artifact download; new
// downloads only ever write it. legacyCompletionMarkerName is the pre-
// rename (spec 10) name — Complete falls back to reading it so an artifact
// already verified before the rename is not forced through a full re-hash
// of what can be tens of gigabytes of weights after an upgrade. This never
// weakens verification: Complete still re-checks every file's hash against
// whichever marker it reads.
const completionMarkerName = ".basement-complete.json"
const legacyCompletionMarkerName = ".runonspark-complete.json"

func (h *HFClient) Complete(artifact recipe.Artifact, target string) bool {
	data, err := os.ReadFile(filepath.Join(target, completionMarkerName))
	if err != nil {
		data, err = os.ReadFile(filepath.Join(target, legacyCompletionMarkerName))
	}
	if err != nil {
		return false
	}
	var marker completionMarker
	if json.Unmarshal(data, &marker) != nil || marker.Repository != artifact.Repository || marker.Revision != artifact.Revision || marker.Bytes != artifact.ExpectedBytes || len(marker.Files) == 0 {
		return false
	}
	// A marker left by an earlier version of the same recipe can carry the
	// right repository, revision and total while covering a different set of
	// files: swap one pinned quantization for another of equal size and the
	// totals still agree. The pinned list is compared name by name, so what
	// is verified on disk is what this recipe asks for and not what some
	// previous one happened to leave there.
	if len(artifact.Files) > 0 {
		if len(marker.Files) != len(artifact.Files) {
			return false
		}
		for index, pinned := range artifact.Files {
			if marker.Files[index].Name != pinned.Name || marker.Files[index].Size != pinned.ExpectedBytes {
				return false
			}
		}
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
	files, total, err := selectFiles(artifact, manifest)
	if err != nil {
		return nil, err
	}
	receipt := map[string]any{"repository": artifact.Repository, "revision": manifest.SHA, "expected_bytes": total, "accessible": true}
	if len(artifact.Files) > 0 {
		names := make([]string, 0, len(files))
		for _, file := range files {
			names = append(names, file.Name)
		}
		receipt["pinned_files"] = names
	}
	return receipt, nil
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

// selectFiles narrows a repository revision to what the artifact pins, and
// reports the byte total that everything downstream measures against.
//
// An artifact that pins no files means the whole snapshot, exactly as before:
// same files, same order, same total, same error when the total disagrees.
// An artifact that pins files means those files and only those, in the order
// the recipe declares, and each one is checked twice over — the revision must
// carry a file by that name, and that file must be the exact size the recipe
// pinned. A repository that quietly republished a different quantization
// under the same name fails here, before a byte is fetched. Per-file content
// hashing is unchanged and still runs afterwards on whatever is selected.
func selectFiles(artifact recipe.Artifact, manifest hfManifest) ([]hfFile, int64, error) {
	var total int64
	if len(artifact.Files) == 0 {
		for _, file := range manifest.Siblings {
			total += file.Size
		}
		if total != artifact.ExpectedBytes {
			return nil, 0, fmt.Errorf("snapshot size %d does not match pinned %d", total, artifact.ExpectedBytes)
		}
		return manifest.Siblings, total, nil
	}
	available := make(map[string]hfFile, len(manifest.Siblings))
	for _, file := range manifest.Siblings {
		available[file.Name] = file
	}
	selected := make([]hfFile, 0, len(artifact.Files))
	for _, pinned := range artifact.Files {
		file, ok := available[pinned.Name]
		if !ok {
			return nil, 0, fmt.Errorf("pinned file %s is not in revision %s", pinned.Name, artifact.Revision)
		}
		if file.Size != pinned.ExpectedBytes {
			return nil, 0, fmt.Errorf("pinned file %s is %d bytes, expected %d", pinned.Name, file.Size, pinned.ExpectedBytes)
		}
		selected = append(selected, file)
		total += file.Size
	}
	if total != artifact.ExpectedBytes {
		return nil, 0, fmt.Errorf("pinned files total %d bytes, expected %d", total, artifact.ExpectedBytes)
	}
	return selected, total, nil
}

// pageCacheWindow is how much freshly touched file data may stay in the
// page cache before it is written back and dropped.
const pageCacheWindow = 64 << 20

// downloadFile reports whether the bytes it left at finalPath already
// hashed to the manifest digest, so the caller can skip re-reading a
// file that was hashed while it streamed.
func (h *HFClient) downloadFile(ctx context.Context, artifact recipe.Artifact, file hfFile, finalPath string, progress func(written int64, existing bool) error) (bool, error) {
	tempPath := finalPath + ".part"
	var offset int64
	if stat, err := os.Stat(tempPath); err == nil {
		offset = stat.Size()
		if offset == file.Size && verifyFile(tempPath, file) {
			if err := os.Rename(tempPath, finalPath); err != nil {
				return false, err
			}
			return true, progress(offset, true)
		}
		if offset > file.Size {
			_ = os.Remove(tempPath)
			offset = 0
		}
	}
	if err := progress(offset, true); err != nil {
		return false, fmt.Errorf("check download capacity: %w", err)
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
		return false, err
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
		return false, fmt.Errorf("server returned %s", resp.Status)
	}
	// The hasher is seeded only once the response status has settled
	// whether the existing prefix is being appended to or discarded.
	digest, expected, streaming := fileDigest(file, file.Size)
	if streaming && offset > 0 {
		if err := hashPrefix(digest, tempPath, offset); err != nil {
			streaming = false
		}
	}
	out, err := os.OpenFile(tempPath, flags, 0o640)
	if err != nil {
		return false, err
	}
	defer out.Close()
	buffer := make([]byte, 4<<20)
	written := offset
	evicted := offset
	lastReport := time.Now()
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := out.Write(buffer[:n]); err != nil {
				return false, err
			}
			if streaming {
				digest.Write(buffer[:n])
			}
			written += int64(n)
			if written-evicted >= pageCacheWindow {
				syncAndEvict(out, evicted, written-evicted)
				evicted = written
			}
			if time.Since(lastReport) >= time.Second {
				if err := progress(written, false); err != nil {
					return false, err
				}
				lastReport = time.Now()
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return false, readErr
		}
	}
	if err := out.Sync(); err != nil {
		return false, err
	}
	syncAndEvict(out, evicted, written-evicted)
	if err := out.Close(); err != nil {
		return false, err
	}
	if err := progress(written, false); err != nil {
		return false, err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return false, err
	}
	verified := streaming && written == file.Size && strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expected)
	return verified, nil
}

// fileDigest returns the hash to run over a file's content and the
// expected hex digest. The git-blob sha1 covers a "blob <size>\0"
// header, so it can only be computed while the bytes stream past when
// the final size is already known; size is that final size.
func fileDigest(file hfFile, size int64) (hash.Hash, string, bool) {
	if file.LFS != nil && file.LFS.SHA256 != "" {
		return sha256.New(), file.LFS.SHA256, true
	}
	if len(file.BlobID) == 40 {
		digest := sha1.New()
		_, _ = io.WriteString(digest, "blob "+strconv.FormatInt(size, 10)+"\x00")
		return digest, file.BlobID, true
	}
	return nil, "", false
}

// hashPrefix feeds the first length bytes of path into digest so a
// resumed transfer can keep hashing in place instead of re-reading the
// whole file once it completes.
func hashPrefix(digest hash.Hash, path string, length int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return hashReader(digest, f, length)
}

// hashReader hashes length bytes of f (to EOF when length is negative),
// dropping each window from the page cache behind it. Hashing hundreds
// of gigabytes must not leave that many pages resident on a host whose
// memory is already committed to a served model.
func hashReader(digest hash.Hash, f *os.File, length int64) error {
	buffer := make([]byte, 4<<20)
	var read, evicted int64
	for length < 0 || read < length {
		chunk := buffer
		if length >= 0 && length-read < int64(len(chunk)) {
			chunk = buffer[:length-read]
		}
		n, err := f.Read(chunk)
		if n > 0 {
			digest.Write(chunk[:n])
			read += int64(n)
			if read-evicted >= pageCacheWindow {
				evict(f, evicted, read-evicted)
				evicted = read
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	evict(f, evicted, read-evicted)
	if length >= 0 && read != length {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func verifyFile(path string, file hfFile) bool {
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != file.Size {
		return false
	}
	digest, expected, ok := fileDigest(file, file.Size)
	if !ok {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := hashReader(digest, f, file.Size); err != nil {
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
