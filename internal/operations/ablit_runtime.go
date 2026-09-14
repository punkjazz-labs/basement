package operations

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

const (
	ablitRuntimeMountPath = "/opt/basement/ablit"
	ablitPTHMountPath     = "/usr/local/lib/python3.12/dist-packages/basement_ablit.pth"
)

// The hook is first-party source embedded in the manager, not a mutable image
// layer. Its manifest is deliberately required at runtime: an enabled recipe
// without the generated pin manifest cannot silently serve the base weights.
//
//go:embed ablit_runtime/basement_ablit.py
var ablitRuntimePython []byte

//go:embed ablit_runtime/basement_ablit.pth
var ablitRuntimePTH []byte

//go:embed ablit_runtime/manifest.json
var ablitRuntimeManifest []byte

func ablitRuntimeRoot(dataDir string, r recipe.Recipe) string {
	return filepath.Join(dataDir, "configs", r.ID, fmt.Sprint(r.Version), "abliteration-runtime")
}

func (h *HostExecutor) ablitRuntimeMounts(r recipe.Recipe) map[string]string {
	if !r.Runtime.Abliteration {
		return nil
	}
	root := ablitRuntimeRoot(h.dataDir, r)
	return map[string]string{
		ablitRuntimeMountPath: filepath.Join(root, "hook"),
		ablitPTHMountPath:     filepath.Join(root, "basement_ablit.pth"),
	}
}

func (h *HostExecutor) verifyAblitRuntimeFiles(r recipe.Recipe) error {
	if !r.Runtime.Abliteration {
		return nil
	}
	root := ablitRuntimeRoot(h.dataDir, r)
	expected := map[string][]byte{
		filepath.Join(root, "hook", "basement_ablit.py"): ablitRuntimePython,
		filepath.Join(root, "hook", "manifest.json"):     ablitRuntimeManifest,
		filepath.Join(root, "basement_ablit.pth"):        ablitRuntimePTH,
	}
	for path, contents := range expected {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, contents) {
			return fmt.Errorf("the generated abliteration runtime file %s does not match this manager; reinstall the abliterated recipe before starting it", path)
		}
	}
	return nil
}

// writeAblitRuntime writes only the manager's embedded importer. Donor bytes
// stay in the separately pinned artifact and all mounts are read-only.
func (h *HostExecutor) writeAblitRuntime(r recipe.Recipe) (map[string]any, error) {
	if !r.Runtime.Abliteration {
		return nil, nil
	}
	if err := verifyAblitArtifact(r); err != nil {
		return nil, err
	}
	root := ablitRuntimeRoot(h.dataDir, r)
	hook := filepath.Join(root, "hook")
	if err := os.MkdirAll(hook, 0o750); err != nil {
		return nil, err
	}
	files := map[string][]byte{
		filepath.Join(hook, "basement_ablit.py"):  ablitRuntimePython,
		filepath.Join(hook, "manifest.json"):      ablitRuntimeManifest,
		filepath.Join(root, "basement_ablit.pth"): ablitRuntimePTH,
	}
	checksums := make(map[string]string, len(files))
	for path, contents := range files {
		if err := atomicFile(path, contents, 0o640); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(contents)
		checksums[filepath.Base(path)] = fmt.Sprintf("%x", sum)
	}
	return map[string]any{
		"mount_path": ablitRuntimeMountPath,
		"pth_path":   ablitPTHMountPath,
		"files":      checksums,
	}, nil
}

// verifyAblitArtifact binds the ranged donor recipe to the embedded loader
// manifest before a container can load any base weight. The artifact's own
// downloader verifies the same raw bytes on disk; this prevents a separately
// valid but different donor package from being accepted by this hook.
func verifyAblitArtifact(r recipe.Recipe) error {
	index, ok := r.ArtifactIndex("abliteration")
	if !ok {
		return fmt.Errorf("abliteration runtime has no donor artifact")
	}
	var manifest struct {
		Version int `json:"version"`
		Layers  map[string]struct {
			Shape  []int  `json:"shape"`
			DType  string `json:"dtype"`
			SHA256 string `json:"sha256"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(ablitRuntimeManifest, &manifest); err != nil {
		return fmt.Errorf("decode embedded abliteration manifest: %w", err)
	}
	if manifest.Version != 1 || len(manifest.Layers) != 31 || len(r.Artifacts[index].Files) != 31 {
		return fmt.Errorf("abliteration donor manifest must contain exactly L15 through L45")
	}
	for layer := 15; layer <= 45; layer++ {
		entry, found := manifest.Layers[fmt.Sprint(layer)]
		if !found || entry.DType != "BF16" || len(entry.Shape) != 2 || entry.Shape[0] <= 0 || entry.Shape[1] <= 0 {
			return fmt.Errorf("embedded abliteration manifest entry L%d is invalid", layer)
		}
		name := fmt.Sprintf("L%d.bin", layer)
		var file *recipe.ArtifactFile
		for i := range r.Artifacts[index].Files {
			candidate := &r.Artifacts[index].Files[i]
			if candidate.Name == name {
				file = candidate
				break
			}
		}
		bytes := int64(entry.Shape[0]) * int64(entry.Shape[1]) * 2
		if file == nil || file.Range == nil || file.ExpectedBytes != bytes || file.Range.SHA256 != entry.SHA256 {
			return fmt.Errorf("abliteration donor %s does not match the embedded manifest", name)
		}
	}
	return nil
}

func atomicFile(path string, contents []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, contents, mode); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return nil
}
