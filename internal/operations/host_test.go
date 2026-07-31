package operations

import (
	"context"
	"strings"
	"testing"

	"github.com/punkjazz-labs/runonspark-manager/internal/inventory"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

type resourceInventory struct{ system inventory.System }

func (r resourceInventory) Inspect(context.Context) (inventory.System, error) { return r.system, nil }

func TestHostMemoryGuardChecksCapacityAndLiveHeadroom(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	provider := resourceInventory{system: inventory.System{
		Hostname: "spark-low", MemoryTotal: 128_000_000_000, MemoryAvailable: 100_000_000_000,
		GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: 90_000_000_000,
	}}
	executor := &HostExecutor{inventory: provider}
	if _, err := executor.verifyMemory(context.Background(), r, false); err != nil {
		t.Fatalf("static capacity should pass: %v", err)
	}
	if _, err := executor.verifyMemory(context.Background(), r, true); err == nil || !strings.Contains(err.Error(), "KV cache") {
		t.Fatalf("live OOM risk passed: %v", err)
	}
}

func TestHostDiskGuardRejectsBeforeRequiredHeadroom(t *testing.T) {
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r := recipes[0]
	executor := &HostExecutor{inventory: resourceInventory{system: inventory.System{Hostname: "spark-full", StorageAvailable: r.RequiredBytes() - 1, DockerStorageAvailable: r.RequiredBytes() - 1, DockerSharesDataDisk: true}}}
	if _, err := executor.verifyDisk(context.Background(), r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes); err == nil || !strings.Contains(err.Error(), "spark-full") {
		t.Fatalf("insufficient disk passed: %v", err)
	}
}
