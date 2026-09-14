package operations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

func TestAblitRuntimeIsOptInAndStagesReadOnlyInputs(t *testing.T) {
	h := &HostExecutor{dataDir: t.TempDir()}
	plain := recipe.Recipe{ID: "glm53-flash-exl3-ablit-2s", Version: 1}
	if mounts := h.ablitRuntimeMounts(plain); mounts != nil {
		t.Fatalf("ordinary runtime mounts=%#v, want none", mounts)
	}
	if receipt, err := h.writeAblitRuntime(plain); err != nil || receipt != nil {
		t.Fatalf("ordinary runtime receipt=%#v err=%v, want nil", receipt, err)
	}

	enabled := plain
	enabled.Runtime.Abliteration = true
	var sourceManifest struct {
		Layers map[string]struct {
			Shape  []int  `json:"shape"`
			SHA256 string `json:"sha256"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(ablitRuntimeManifest, &sourceManifest); err != nil {
		t.Fatal(err)
	}
	donor := recipe.Artifact{Role: "abliteration"}
	for layer := 15; layer <= 45; layer++ {
		entry := sourceManifest.Layers[fmt.Sprint(layer)]
		donor.Files = append(donor.Files, recipe.ArtifactFile{
			Name:          fmt.Sprintf("L%d.bin", layer),
			ExpectedBytes: int64(entry.Shape[0]) * int64(entry.Shape[1]) * 2,
			Range:         &recipe.ArtifactRange{SHA256: entry.SHA256},
		})
	}
	enabled.Artifacts = []recipe.Artifact{donor}
	receipt, err := h.writeAblitRuntime(enabled)
	if err != nil {
		t.Fatal(err)
	}
	if receipt["mount_path"] != ablitRuntimeMountPath || receipt["pth_path"] != ablitPTHMountPath {
		t.Fatalf("receipt=%#v", receipt)
	}
	mounts := h.ablitRuntimeMounts(enabled)
	identity := containerMounts(enabled, nil, "/managed/cache", nil, mounts)
	if identity[ablitRuntimeMountPath] != mounts[ablitRuntimeMountPath] || identity[ablitPTHMountPath] != mounts[ablitPTHMountPath] {
		t.Fatalf("container identity omitted the ablit mounts: %#v", identity)
	}
	for mountPoint, source := range mounts {
		info, err := os.Stat(source)
		if err != nil {
			t.Fatalf("%s source: %v", mountPoint, err)
		}
		if mountPoint == ablitRuntimeMountPath && !info.IsDir() {
			t.Fatalf("hook source %s is not a directory", source)
		}
		if mountPoint == ablitPTHMountPath && !info.Mode().IsRegular() {
			t.Fatalf("pth source %s is not a file", source)
		}
	}
	data, err := os.ReadFile(filepath.Join(mounts[ablitRuntimeMountPath], "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var copiedManifest struct {
		Version int                        `json:"version"`
		Layers  map[string]json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(data, &copiedManifest); err != nil {
		t.Fatal(err)
	}
	if copiedManifest.Version != 1 || len(copiedManifest.Layers) != 31 || copiedManifest.Layers["15"] == nil || copiedManifest.Layers["45"] == nil {
		t.Fatalf("manifest=%#v, want pinned L15 through L45", copiedManifest)
	}
	if err := h.verifyAblitRuntimeFiles(enabled); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mounts[ablitRuntimeMountPath], "basement_ablit.py"), []byte("modified"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := h.verifyAblitRuntimeFiles(enabled); err == nil {
		t.Fatal("modified embedded runtime file passed verification")
	}
}
