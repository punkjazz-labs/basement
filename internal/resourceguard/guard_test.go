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

func TestMemoryGuardRejectsLiveOOMRisk(t *testing.T) {
	node := Node{Name: "spark-low", SystemMemoryTotal: 128_000_000_000, SystemMemoryAvailable: 110_000_000_000, GPUMemoryTotal: 128_000_000_000, GPUMemoryFree: 90_000_000_000}
	_, err := CheckMemory([]Node{node}, 1, MemoryPolicy{MinimumTotalBytes: 120_000_000_000, HostReserveBytes: 16_000_000_000, GPUUtilization: 0.8, RequireLiveCapacity: true})
	if err == nil || !strings.Contains(err.Error(), "weights, KV cache, and context") || !strings.Contains(err.Error(), "host reserve") {
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
