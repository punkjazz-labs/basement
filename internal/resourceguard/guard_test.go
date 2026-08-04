package resourceguard

import (
	"strings"
	"testing"
)

func TestMemoryGuardPreservesRuntimeAndHostHeadroom(t *testing.T) {
	node := Node{Name: "spark-a", SystemMemoryTotal: 128_000_000_000, SystemMemoryAvailable: 120_000_000_000, GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: 120_000_000_000}
	results, err := CheckMemory([]Node{node}, 1, MemoryPolicy{MinimumTotalBytes: 120_000_000_000, HostReserveBytes: 16_000_000_000, GPUUtilization: 0.8, RequireLiveCapacity: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RuntimeBudgetBytes != 102_400_000_000 || results[0].RequiredAvailableBytes != 118_400_000_000 {
		t.Fatalf("unexpected memory result: %#v", results)
	}
}

// An engine that allocates what the model needs rather than a share of the
// device states its claim in bytes. That claim must not change with the size
// of the machine it lands on: scaling it would over-reserve on a big node and
// under-reserve on a small one, and neither is what the engine will do.
func TestMemoryGuardHonoursAnAbsoluteRuntimeBudget(t *testing.T) {
	policy := MemoryPolicy{MinimumTotalBytes: 120_000_000_000, HostReserveBytes: 10_000_000_000, RuntimeBudgetBytes: 109_000_000_000, RequireLiveCapacity: true}
	node := Node{Name: "spark-a", SystemMemoryTotal: 128_000_000_000, SystemMemoryAvailable: 126_000_000_000, GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: 126_000_000_000}
	results, err := CheckMemory([]Node{node}, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].RuntimeBudgetBytes != 109_000_000_000 || results[0].RequiredAvailableBytes != 119_000_000_000 {
		t.Fatalf("unexpected memory result: %#v", results)
	}
	// A larger machine claims the same bytes, not a larger share of itself.
	bigger := node
	bigger.GPUMemoryTotal, bigger.SystemMemoryTotal = 256_000_000_000, 256_000_000_000
	results, err = CheckMemory([]Node{bigger}, 1, policy)
	if err != nil || results[0].RuntimeBudgetBytes != 109_000_000_000 {
		t.Fatalf("budget scaled with the machine: %#v (%v)", results, err)
	}
	// A policy that states neither form of claim is refused, not read as
	// zero. So is one that states both: the absolute budget would win and
	// the share the caller also wrote would be silently discarded.
	neither := MemoryPolicy{MinimumTotalBytes: 120_000_000_000, HostReserveBytes: 10_000_000_000}
	both := neither
	both.GPUUtilization, both.RuntimeBudgetBytes = 0.85, 109_000_000_000
	for name, broken := range map[string]MemoryPolicy{"neither": neither, "both": both} {
		if _, err := CheckMemory([]Node{node}, 1, broken); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("CheckMemory() with %s stated=%v, want a refusal naming the invariant", name, err)
		}
	}
	// A share outside the open interval is still incomplete rather than
	// clamped, exactly as before.
	unusable := neither
	unusable.GPUUtilization = 1.5
	if _, err := CheckMemory([]Node{node}, 1, unusable); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("CheckMemory()=%v, want an incomplete-policy error", err)
	}
}

func TestMemoryGuardRejectsLiveOOMRisk(t *testing.T) {
	node := Node{Name: "spark-low", SystemMemoryTotal: 128_000_000_000, SystemMemoryAvailable: 110_000_000_000, GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: 90_000_000_000}
	_, err := CheckMemory([]Node{node}, 1, MemoryPolicy{MinimumTotalBytes: 120_000_000_000, HostReserveBytes: 16_000_000_000, GPUUtilization: 0.8, RequireLiveCapacity: true})
	if err == nil || !strings.Contains(err.Error(), "weights, KV cache, and context") || !strings.Contains(err.Error(), "system reserve") {
		t.Fatalf("CheckMemory()=%v", err)
	}
}

func TestMultiNodeGuardsDoNotPoolCapacity(t *testing.T) {
	nodes := []Node{
		{Name: "spark-1", SystemMemoryTotal: 140, SystemMemoryAvailable: 130, GPUMemoryTotal: 140, GPUMemoryFree: 130, DataDiskAvailable: 160, RuntimeDiskAvailable: 160, SharedDataRuntimeDisk: true},
		{Name: "spark-2", SystemMemoryTotal: 100, SystemMemoryAvailable: 90, GPUMemoryTotal: 100, GPUMemoryFree: 90, DataDiskAvailable: 80, RuntimeDiskAvailable: 80, SharedDataRuntimeDisk: true},
	}
	_, memoryErr := CheckMemory(nodes, 2, MemoryPolicy{MinimumTotalBytes: 110, HostReserveBytes: 10, GPUUtilization: 0.5})
	_, diskErr := CheckDisk(nodes, 2, DiskPolicy{ArtifactBytes: 80, RuntimeBytes: 10, SafetyMarginBytes: 10})
	if memoryErr == nil || !strings.Contains(memoryErr.Error(), "spark-2") {
		t.Fatalf("under-provisioned node passed memory check: %v", memoryErr)
	}
	if diskErr == nil || !strings.Contains(diskErr.Error(), "spark-2") {
		t.Fatalf("under-provisioned node passed disk check: %v", diskErr)
	}
}

func TestDiskGuardChecksSeparateDockerFilesystem(t *testing.T) {
	node := Node{Name: "spark-docker-full", DataDiskAvailable: 500, RuntimeDiskAvailable: 40, SharedDataRuntimeDisk: false}
	_, err := CheckDisk([]Node{node}, 1, DiskPolicy{ArtifactBytes: 100, RuntimeBytes: 35, SafetyMarginBytes: 10})
	if err == nil || !strings.Contains(err.Error(), "Docker disk") {
		t.Fatalf("separate Docker exhaustion passed: %v", err)
	}
}
