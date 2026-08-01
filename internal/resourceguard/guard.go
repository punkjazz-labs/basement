package resourceguard

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Node describes resources that must be independently sufficient on every
// participating Spark. Capacities are never pooled across nodes.
type Node struct {
	Name                  string `json:"name"`
	SystemMemoryTotal     int64  `json:"system_memory_total_bytes"`
	SystemMemoryAvailable int64  `json:"system_memory_available_bytes"`
	GPUMemoryTotal        int64  `json:"gpu_memory_total_bytes"`
	GPUMemoryFree         int64  `json:"gpu_memory_free_bytes"`
	DataDiskAvailable     int64  `json:"data_disk_available_bytes"`
	RuntimeDiskAvailable  int64  `json:"runtime_disk_available_bytes"`
	SharedDataRuntimeDisk bool   `json:"shared_data_runtime_disk"`
}

type MemoryPolicy struct {
	MinimumTotalBytes   int64
	HostReserveBytes    int64
	GPUUtilization      float64
	RequireLiveCapacity bool
}

type MemoryResult struct {
	Node                     Node  `json:"node"`
	RuntimeBudgetBytes       int64 `json:"runtime_budget_bytes"`
	HostReserveBytes         int64 `json:"host_reserve_bytes"`
	RequiredAvailableBytes   int64 `json:"required_available_bytes"`
	NominalGPUHeadroomBytes  int64 `json:"nominal_gpu_headroom_bytes"`
	LiveCapacityWasEvaluated bool  `json:"live_capacity_evaluated"`
}

type DiskResult struct {
	Node                 Node  `json:"node"`
	ArtifactBytes        int64 `json:"artifact_bytes"`
	RuntimeBytes         int64 `json:"runtime_bytes"`
	SafetyMarginBytes    int64 `json:"safety_margin_bytes"`
	DataRequiredBytes    int64 `json:"data_required_bytes"`
	RuntimeRequiredBytes int64 `json:"runtime_required_bytes"`
}

type DiskPolicy struct {
	ArtifactBytes     int64
	RuntimeBytes      int64
	SafetyMarginBytes int64
}

func CheckMemory(nodes []Node, expectedNodes int, policy MemoryPolicy) ([]MemoryResult, error) {
	if expectedNodes < 1 || len(nodes) != expectedNodes {
		return nil, fmt.Errorf("resource inventory contains %d nodes, recipe requires %d", len(nodes), expectedNodes)
	}
	if policy.MinimumTotalBytes <= 0 || policy.HostReserveBytes <= 0 || policy.GPUUtilization <= 0 || policy.GPUUtilization >= 1 {
		return nil, errors.New("memory policy is incomplete")
	}
	results := make([]MemoryResult, 0, len(nodes))
	var problems []string
	for index, node := range nodes {
		name := nodeName(node, index)
		runtimeBudget := int64(math.Ceil(float64(node.GPUMemoryTotal) * policy.GPUUtilization))
		result := MemoryResult{
			Node: node, RuntimeBudgetBytes: runtimeBudget, HostReserveBytes: policy.HostReserveBytes,
			RequiredAvailableBytes:   runtimeBudget + policy.HostReserveBytes,
			NominalGPUHeadroomBytes:  node.GPUMemoryTotal - runtimeBudget,
			LiveCapacityWasEvaluated: policy.RequireLiveCapacity,
		}
		results = append(results, result)
		if node.SystemMemoryTotal < policy.MinimumTotalBytes {
			problems = append(problems, fmt.Sprintf("%s has %s of system memory; this model needs %s", name, humanBytes(node.SystemMemoryTotal), humanBytes(policy.MinimumTotalBytes)))
		}
		if node.GPUMemoryTotal < policy.MinimumTotalBytes {
			problems = append(problems, fmt.Sprintf("%s has %s of unified GPU memory; this model needs %s", name, humanBytes(node.GPUMemoryTotal), humanBytes(policy.MinimumTotalBytes)))
		}
		if result.NominalGPUHeadroomBytes < policy.HostReserveBytes {
			problems = append(problems, fmt.Sprintf("%s: the vLLM budget leaves %s outside the GPU allocation; %s must stay reserved for the system", name, humanBytes(result.NominalGPUHeadroomBytes), humanBytes(policy.HostReserveBytes)))
		}
		if !policy.RequireLiveCapacity {
			continue
		}
		if node.GPUMemoryFree < runtimeBudget {
			problems = append(problems, fmt.Sprintf("%s has %s of free GPU memory; %s is needed for weights, KV cache, and context", name, humanBytes(node.GPUMemoryFree), humanBytes(runtimeBudget)))
		}
		if node.SystemMemoryAvailable < result.RequiredAvailableBytes {
			problems = append(problems, fmt.Sprintf("%s has %s of unified memory available; %s is needed including the %s system reserve", name, humanBytes(node.SystemMemoryAvailable), humanBytes(result.RequiredAvailableBytes), humanBytes(policy.HostReserveBytes)))
		}
	}
	if len(problems) > 0 {
		return results, errors.New(strings.Join(problems, "; "))
	}
	return results, nil
}

func CheckDisk(nodes []Node, expectedNodes int, policy DiskPolicy) ([]DiskResult, error) {
	if expectedNodes < 1 || len(nodes) != expectedNodes {
		return nil, fmt.Errorf("resource inventory contains %d nodes, recipe requires %d", len(nodes), expectedNodes)
	}
	if policy.ArtifactBytes < 0 || policy.RuntimeBytes < 0 || policy.SafetyMarginBytes <= 0 {
		return nil, errors.New("disk policy is incomplete")
	}
	results := make([]DiskResult, 0, len(nodes))
	var problems []string
	for index, node := range nodes {
		result := DiskResult{Node: node, ArtifactBytes: policy.ArtifactBytes, RuntimeBytes: policy.RuntimeBytes, SafetyMarginBytes: policy.SafetyMarginBytes}
		if node.SharedDataRuntimeDisk {
			result.DataRequiredBytes = policy.ArtifactBytes + policy.RuntimeBytes + policy.SafetyMarginBytes
			result.RuntimeRequiredBytes = result.DataRequiredBytes
		} else {
			result.DataRequiredBytes = policy.ArtifactBytes + policy.SafetyMarginBytes
			result.RuntimeRequiredBytes = policy.RuntimeBytes + policy.SafetyMarginBytes
		}
		results = append(results, result)
		name := nodeName(node, index)
		if node.DataDiskAvailable < result.DataRequiredBytes {
			problems = append(problems, fmt.Sprintf("%s has %s free for model data, but %s must stay free (including the safety margin)", name, humanBytes(node.DataDiskAvailable), humanBytes(result.DataRequiredBytes)))
		}
		if !node.SharedDataRuntimeDisk && node.RuntimeDiskAvailable < result.RuntimeRequiredBytes {
			problems = append(problems, fmt.Sprintf("%s has %s free on the Docker disk, but %s must stay free (including the safety margin)", name, humanBytes(node.RuntimeDiskAvailable), humanBytes(result.RuntimeRequiredBytes)))
		}
	}
	if len(problems) > 0 {
		return results, errors.New(strings.Join(problems, "; "))
	}
	return results, nil
}

func nodeName(node Node, index int) string {
	if strings.TrimSpace(node.Name) != "" {
		return node.Name
	}
	return fmt.Sprintf("node-%d", index+1)
}

// humanBytes renders sizes the way the console does, so guard errors read
// as product copy instead of raw byte counts.
func humanBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	amount := float64(value)
	unit := 0
	for amount >= 1000 && unit < len(units)-1 {
		amount /= 1000
		unit++
	}
	if amount >= 100 {
		return fmt.Sprintf("%.0f %s", amount, units[unit])
	}
	return fmt.Sprintf("%.1f %s", amount, units[unit])
}
