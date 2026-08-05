package operations

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/punkjazz-labs/basement/internal/inventory"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/resourceguard"
)

type HostExecutor struct {
	dataDir     string
	inventory   inventory.Provider
	docker      *DockerClient
	hf          *HFClient
	http        *http.Client
	retryDelays []time.Duration
}

// checkPort is the host socket seam. Production never reassigns it; tests
// replace it so a port-rule test never binds a loopback listener or depends
// on another process running on the test machine.
var checkPort = inventory.CheckPort

func NewHostExecutor(dataDir, dockerSocket string, provider inventory.Provider) *HostExecutor {
	return &HostExecutor{dataDir: dataDir, inventory: provider, docker: NewDockerClient(dockerSocket), hf: NewHFClient(), http: &http.Client{Timeout: 30 * time.Second}, retryDelays: networkRetryDelays}
}

// networkRetryDelays paces automatic retries of registry pulls and weight
// downloads: roughly two minutes of patience before a network problem is
// allowed to fail an install. Docker keeps completed layers and weight
// downloads resume from disk, so a retry only repeats the lost tail.
var networkRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

// transientNetworkError recognizes failures that a working machine recovers
// from on its own: resolver hiccups, dropped connections, registry blips, and
// a mid-stream HTTP/2 reset (observed on real hardware during multi-hour, tens
// of gigabytes weight downloads as "stream error: stream ID ...; CANCEL" from
// net/http's bundled HTTP/2 transport). That transport keeps the concrete
// error type unexported, so it cannot be matched with errors.As; Docker's
// daemon flattens its own errors into event strings the same way. The match
// is textual for both, by design. Anything else (auth, missing image, disk
// guard, a checksum mismatch) surfaces immediately.
func transientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"server misbehaving", // systemd-resolved SERVFAIL, e.g. "lookup ghcr.io on 127.0.0.53:53"
		"no such host",
		"temporary failure in name resolution",
		"connection refused",
		"connection reset",
		"broken pipe",
		"unexpected eof",
		"timeout",
		"network is unreachable",
		"service unavailable",
		"bad gateway",
		"stream error", // HTTP/2 stream-level reset, e.g. "stream error: stream ID 19; CANCEL"
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// retryNetwork reruns op while it fails on what looks like a temporary
// network problem. The user is never asked to redo a blip by hand; only a
// persistent outage surfaces, carrying the machine-side checks to run.
//
// Every retry is recorded, not just replayed: each attempt is announced
// through progress (visible on the job's live step receipt as it happens),
// and on eventual success the count and the per-attempt errors are folded
// into the returned receipt as "retries" and "retry_events" so a completed
// step still shows that a network blip happened, instead of reading as if
// the download had gone through cleanly the first time. op's resume
// mechanism (Range from bytes already on disk for a weight download, layers
// Docker already has for an image pull) is what makes each retry pick up
// where the last attempt left off rather than starting over.
func (h *HostExecutor) retryNetwork(ctx context.Context, what string, progress Progress, op func() (map[string]any, error)) (map[string]any, error) {
	var events []map[string]any
	for attempt := 0; ; attempt++ {
		value, err := op()
		if err == nil || ctx.Err() != nil || !transientNetworkError(err) {
			if err == nil && len(events) > 0 {
				if value == nil {
					value = map[string]any{}
				}
				value["retries"] = len(events)
				value["retry_events"] = events
			}
			return value, err
		}
		if attempt >= len(h.retryDelays) {
			if attempt > 0 {
				return nil, fmt.Errorf("%s failed %d times on network errors; check this machine's connection and DNS, then retry: %w", what, attempt+1, err)
			}
			return nil, err
		}
		events = append(events, map[string]any{"attempt": attempt + 1, "error": err.Error(), "retry_after_seconds": h.retryDelays[attempt].Seconds()})
		if progress != nil {
			if problem := progress(map[string]any{"status": "retrying after a network error", "attempt": attempt + 1, "error": err.Error()}); problem != nil {
				return nil, problem
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(h.retryDelays[attempt]):
		}
	}
}

// RuntimeImageBytes asks Docker for the pinned runtime image's on-disk size.
func (h *HostExecutor) RuntimeImageBytes(ctx context.Context, r recipe.Recipe) (int64, bool) {
	return h.docker.ImageSize(ctx, r.Runtime.Reference())
}

func (h *HostExecutor) ArtifactPath(r recipe.Recipe) string {
	index, _ := r.ArtifactIndex("primary")
	artifact := r.Artifacts[index]
	return filepath.Join(h.dataDir, "artifacts", strings.ReplaceAll(artifact.Repository, "/", "--"), artifact.Revision)
}

func (h *HostExecutor) Execute(ctx context.Context, execution Execution, operation recipe.Operation, r recipe.Recipe, progress Progress) (map[string]any, error) {
	switch operation.Type {
	case "verify_architecture":
		system, err := h.inventory.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		if system.Architecture != r.Requirements.Architecture {
			return nil, fmt.Errorf("architecture %s does not satisfy %s", system.Architecture, r.Requirements.Architecture)
		}
		return map[string]any{"architecture": system.Architecture}, nil
	case "verify_dgx_spark":
		system, err := h.inventory.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		if !system.DGXSpark {
			return nil, errors.New("GB10 hardware identity was not detected (DGX Spark or an OEM GB10 machine)")
		}
		return map[string]any{"product_name": system.ProductName, "dgx_spark": true}, nil
	case "verify_memory_capacity", "verify_memory":
		if operation.Type == "verify_memory" {
			if state, err := h.docker.Container(ctx, h.resolveContainerName(ctx, r)); err == nil && state.Running {
				return map[string]any{"already_running": true, "container_id": state.ID}, nil
			}
		}
		return h.verifyMemory(ctx, r, operation.Type == "verify_memory")
	case "verify_disk":
		return h.verifyDisk(ctx, r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes, execution.ReservedBytes)
	case "verify_port":
		if err := checkPort(r.Service.DefaultHostPort); err != nil {
			// Our own leftover container (a cancelled install, an old recipe
			// version) is not a blocker: the start phase stops it. A foreign
			// process is.
			if managed, listErr := h.docker.ManagedContainers(ctx); listErr == nil {
				for _, container := range managed {
					if !container.Running {
						continue
					}
					if container.RecipeID == r.ID && (container.HostPort == 0 || container.HostPort == r.Service.DefaultHostPort) {
						return map[string]any{
							"host_port": r.Service.DefaultHostPort, "occupied_by_previous_install": container.Name, "freed_during_start": true,
						}, nil
					}
					if container.HostPort != 0 && container.HostPort != r.Service.DefaultHostPort {
						continue
					}
					// An older managed container may predate the host-port
					// label. Unknown remains conservative because treating it
					// as a different port could hide the process that made this
					// exact bind fail.
					return nil, fmt.Errorf("port %d is held by the container of another model (%s, recipe %s); stop that model first", r.Service.DefaultHostPort, container.Name, container.RecipeID)
				}
			}
			return nil, err
		}
		return map[string]any{"host_port": r.Service.DefaultHostPort, "available": true}, nil
	case "verify_docker":
		if err := h.docker.Ping(ctx); err != nil {
			return nil, fmt.Errorf("docker daemon unavailable: %w", err)
		}
		return map[string]any{"docker_ready": true}, nil
	case "verify_nvidia_runtime":
		system, err := h.inventory.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		if !system.NvidiaRuntimeReady || !system.GPUVisible {
			return nil, errors.New("NVIDIA runtime or GPU visibility is unavailable")
		}
		return map[string]any{"nvidia_runtime": true, "gpu_visible": true, "gpu": system.GPUDescription}, nil
	case "verify_artifact_access":
		checks := make([]map[string]any, 0, len(r.Artifacts))
		for _, artifact := range r.Artifacts {
			check, err := h.hf.CheckAccess(ctx, artifact)
			if err != nil {
				return nil, err
			}
			checks = append(checks, check)
		}
		return map[string]any{"artifacts": checks}, nil
	case "pull_image":
		if h.docker.ImageExists(ctx, r.Runtime.Reference()) {
			return map[string]any{"image": r.Runtime.Reference(), "reused": true}, nil
		}
		if _, err := h.verifyDisk(ctx, r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes, execution.ReservedBytes); err != nil {
			return nil, fmt.Errorf("refusing image pull: %w", err)
		}
		guarded := func(value any) error {
			if _, err := h.verifyDisk(ctx, r, r.TotalArtifactBytes(), 0, execution.ReservedBytes); err != nil {
				return fmt.Errorf("the image pull paused to protect free disk space: %w", err)
			}
			if progress != nil {
				return progress(value)
			}
			return nil
		}
		return h.retryNetwork(ctx, "pulling the runtime image", guarded, func() (map[string]any, error) {
			return h.docker.Pull(ctx, r.Runtime.Reference(), guarded)
		})
	case "download_artifact":
		var receipts []map[string]any
		for i, artifact := range r.Artifacts {
			target := h.artifactPath(r, i)
			var laterBytes int64
			for _, later := range r.Artifacts[i+1:] {
				laterBytes += later.ExpectedBytes
			}
			guarded := func(value any) error {
				completed := receiptInt64(value, "bytes_complete")
				remaining := artifact.ExpectedBytes - completed
				if remaining < 0 {
					remaining = 0
				}
				remainingArtifacts := remaining + laterBytes
				if _, err := h.verifyDisk(ctx, r, remainingArtifacts, 0, execution.ReservedBytes); err != nil {
					return fmt.Errorf("the download paused to protect free disk space: %w", err)
				}
				if progress != nil {
					return progress(value)
				}
				return nil
			}
			receipt, err := h.retryNetwork(ctx, "downloading model weights", guarded, func() (map[string]any, error) {
				return h.hf.Download(ctx, artifact, target, guarded)
			})
			if err != nil {
				return nil, err
			}
			receipts = append(receipts, receipt)
		}
		return map[string]any{"artifacts": receipts}, nil
	case "write_generated_config":
		return h.writeGeneratedConfig(execution, r)
	case "create_container":
		if err := h.verifyRuntimeInputs(r); err != nil {
			return nil, err
		}
		drift, err := h.replaceStaleContainer(ctx, r, execution.Placement)
		if err != nil {
			return nil, err
		}
		id, err := h.createContainer(ctx, execution, r)
		if err != nil {
			return nil, err
		}
		receipt := map[string]any{"container_id": id, "container_name": containerName(r)}
		if len(drift.Mounts) > 0 {
			receipt["recreated_after_move"] = mountMismatchReceipt(drift.Mounts)
		}
		if drift.Image != nil {
			receipt["recreated_for_image_change"] = imageMismatchReceipt(*drift.Image)
		}
		if len(drift.Writable) > 0 {
			receipt["recreated_for_writable_paths"] = mountMismatchReceipt(drift.Writable)
		}
		if len(drift.Command) > 0 {
			receipt["recreated_for_command_change"] = commandMismatchReceipt(drift.Command)
		}
		if len(drift.Environment) > 0 {
			receipt["recreated_for_environment_change"] = environmentMismatchReceipt(drift.Environment)
		}
		if len(drift.Launch) > 0 {
			receipt["recreated_after_address_change"] = launchMismatchReceipt(drift.Launch)
		}
		if execution.Placement.Distributed() {
			receipt["node_rank"] = execution.Placement.Rank()
			receipt["master_address"] = execution.Placement.MasterAddress
			receipt["master_port"] = execution.Placement.MasterPort
		}
		return receipt, nil
	case "start_container":
		if err := h.verifyRuntimeInputs(r); err != nil {
			return nil, err
		}
		// A start job carries no create step, so a container that is gone —
		// removed by hand outside this manager, while the stored install still
		// names it — or one left pointing at a directory that has since moved
		// is rebuilt here or not at all. Both are asked before anything is
		// touched: replaceStaleContainer removes what it repairs, which would
		// otherwise read as a container that was never there.
		_, inspectErr := h.docker.Container(ctx, h.resolveContainerName(ctx, r))
		missing := errors.Is(inspectErr, ErrContainerNotFound)
		drift, err := h.replaceStaleContainer(ctx, r, execution.Placement)
		if err != nil {
			return nil, err
		}
		if missing || drift.found() {
			// A container is never created without the record of how it was
			// launched; on a machine where remove_container deleted that record
			// this writes it again first.
			if err := h.ensureGeneratedConfig(execution, r); err != nil {
				return nil, err
			}
			if _, err := h.createContainer(ctx, execution, r); err != nil {
				return nil, err
			}
		}
		name := h.resolveContainerName(ctx, r)
		if err := h.docker.Start(ctx, name); err != nil {
			return nil, err
		}
		state, err := h.docker.Container(ctx, name)
		if err != nil {
			return nil, err
		}
		receipt := map[string]any{"container_id": state.ID, "running": state.Running}
		// The two repairs are different stories and the log tells them apart:
		// one container was gone, the other was reading from the wrong place.
		if missing {
			receipt["recreated_missing"] = containerName(r)
		}
		if len(drift.Mounts) > 0 {
			receipt["recreated_after_move"] = mountMismatchReceipt(drift.Mounts)
		}
		// A recipe can pin a new image digest without changing its version, so
		// the container built by an earlier deployment would otherwise keep
		// serving the old image while every receipt reported the new one.
		if drift.Image != nil {
			receipt["recreated_for_image_change"] = imageMismatchReceipt(*drift.Image)
		}
		// A recipe that has since declared a writable path describes a
		// container this one is not: its root filesystem is read-only and the
		// path the runtime compiles into does not exist, which the runtime
		// discovers only after loading the weights.
		if len(drift.Writable) > 0 {
			receipt["recreated_for_writable_paths"] = mountMismatchReceipt(drift.Writable)
		}
		// Serve arguments are fixed at container creation just like the image
		// and mounts. The receipt names the first changed flag so a recipe edit
		// is visible rather than reading as an unexplained rebuild.
		if len(drift.Command) > 0 {
			receipt["recreated_for_command_change"] = commandMismatchReceipt(drift.Command)
		}
		// The environment is fixed at creation too, and a manager that learns
		// to set a variable changes nothing about a container that already
		// exists. The comfyui cache redirects are exactly that, so without
		// this a failed install would be retried against the same broken
		// container forever.
		if len(drift.Environment) > 0 {
			receipt["recreated_for_environment_change"] = environmentMismatchReceipt(drift.Environment)
		}
		// A two-Spark model meets at an address the kernel assigns to the
		// cabled port, and that address can differ after a reboot. The
		// container built by the last deployment still holds the old one, so
		// it is rebuilt here against the address this job just resolved.
		if len(drift.Launch) > 0 {
			receipt["recreated_after_address_change"] = launchMismatchReceipt(drift.Launch)
		}
		return receipt, nil
	case "stop_container":
		name := h.resolveContainerName(ctx, r)
		if err := h.docker.Stop(ctx, name); err != nil {
			return nil, err
		}
		// Containers from earlier recipe versions (cancelled installs,
		// upgrades — including ones still under the pre-rename name) hold
		// the same host port; stop them too.
		stopped := []string{name}
		if managed, err := h.docker.ManagedContainers(ctx); err == nil {
			for _, container := range managed {
				if container.RecipeID == r.ID && container.Running && container.Name != name {
					if err := h.docker.Stop(ctx, container.Name); err == nil {
						stopped = append(stopped, container.Name)
					}
				}
			}
		}
		return map[string]any{"container_name": name, "stopped": true, "stopped_containers": stopped}, nil
	case "remove_container":
		name := h.resolveContainerName(ctx, r)
		if err := h.docker.Remove(ctx, name); err != nil {
			return nil, err
		}
		if err := h.removeOwnedConfig(r); err != nil {
			return nil, err
		}
		if err := h.removeOwnedCache(r); err != nil {
			return nil, err
		}
		return map[string]any{"container_name": name, "removed": true, "generated_config_removed": true, "compilation_cache_removed": true}, nil
	case "wait_http":
		return h.waitHTTP(ctx, r, progress)
	case "verify_openai_inference":
		return h.verifyInference(ctx, r)
	case "verify_media_generation":
		return h.verifyMediaGeneration(ctx, r, progress)
	case "measure_throughput":
		return h.measureThroughput(ctx, r)
	case "remove_artifact_if_unshared":
		if !execution.RemoveArtifacts {
			return map[string]any{"retained": true, "bytes_retained": r.TotalArtifactBytes()}, nil
		}
		var reclaimed, sharedRetained int64
		sharedRepositories := make([]string, 0)
		for i, artifact := range r.Artifacts {
			target := h.artifactPath(r, i)
			if execution.SharedArtifacts[artifactKey(artifact)] || execution.SharedArtifacts[target] {
				sharedRetained += artifact.ExpectedBytes
				sharedRepositories = append(sharedRepositories, artifact.Repository)
				continue
			}
			if !h.hf.Complete(artifact, target) {
				return nil, fmt.Errorf("refusing to remove unowned or incomplete artifact path %s", target)
			}
			if err := h.removeOwnedArtifact(target); err != nil {
				return nil, err
			}
			reclaimed += artifact.ExpectedBytes
		}
		return map[string]any{"removed": reclaimed > 0, "reclaimed_bytes": reclaimed, "shared_retained_bytes": sharedRetained, "shared_retained_repositories": sharedRepositories}, nil
	default:
		return nil, fmt.Errorf("operation %s is not implemented", operation.Type)
	}
}

func (h *HostExecutor) verifyMemory(ctx context.Context, r recipe.Recipe, requireLive bool) (map[string]any, error) {
	system, err := h.inventory.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := memoryPlan(r)
	if err != nil {
		return nil, err
	}
	node := resourceguard.Node{
		Name: system.Hostname, SystemMemoryTotal: system.MemoryTotal, SystemMemoryAvailable: system.MemoryAvailable,
		GPUMemoryTotal: system.GPUMemoryTotal, GPUMemoryFree: system.GPUMemoryFree,
	}
	// Every node evaluates itself and only itself; a two-Spark recipe is
	// checked by this step running on each node in turn, never by pooling.
	results, err := resourceguard.CheckMemory([]resourceguard.Node{node}, 1, resourceguard.MemoryPolicy{
		MinimumTotalBytes: r.Requirements.MinimumMemoryBytes, HostReserveBytes: r.Requirements.MemoryReserveBytes,
		GPUUtilization: plan.utilization, RuntimeBudgetBytes: plan.budgetBytes, RequireLiveCapacity: requireLive,
	})
	receipt := map[string]any{
		"per_node": results, "live_capacity": requireLive, "runtime_kind": r.Runtime.Kind,
		"kv_cache_dtype": plan.kvCacheDType, "max_model_len": plan.maxModelLen, "max_num_seqs": plan.maxConcurrentRequests,
	}
	if err != nil {
		return receipt, err
	}
	return receipt, nil
}

// memoryPlan is the runtime-neutral view of a recipe's planned device memory:
// how much of the device the engine claims up front, the context length it
// plans for, and how many requests it will run at once. Each kind spells
// these differently (vLLM gpu_memory_utilization / max_model_len /
// max_num_seqs, SGLang mem_fraction_static / context_length /
// max_running_requests, llama.cpp memory_model / context_size / parallel)
// but the guardrail and its receipt read the same fields for every kind.
//
// The claim itself comes in two shapes, and they are not interchangeable.
// vLLM and SGLang are handed a share of the device and grow into it, so a
// bigger machine means a bigger claim. llama.cpp is handed nothing of the
// sort: it maps the weights and sizes the KV cache from the context, and
// holds the same bytes on any machine. Expressing that as a share would
// inflate the claim on a larger node and understate it on a smaller one, so
// it stays a byte count all the way through to the guardrail.
type runtimeMemoryPlan struct {
	utilization           float64
	budgetBytes           int64
	kvCacheDType          string
	maxModelLen           int
	maxConcurrentRequests int
}

func memoryPlan(r recipe.Recipe) (runtimeMemoryPlan, error) {
	if c, media := r.MediaGeneration(); media {
		// ComfyUI has no device memory fraction to read: it loads what a
		// workflow asks for and holds it, so the claim is the recipe's own
		// measured total. There is no context length and no KV cache, and
		// both stay absent rather than reading zero, which would look like a
		// model that plans for no context at all.
		planned, ok := r.PlannedMemoryBytes()
		if !ok {
			return runtimeMemoryPlan{}, fmt.Errorf("recipe declares runtime kind %q without a memory model", r.Runtime.Kind)
		}
		return runtimeMemoryPlan{budgetBytes: planned, maxConcurrentRequests: c.ConcurrentGenerations}, nil
	}
	if r.Runtime.Kind == "llamacpp" {
		planned, ok := r.PlannedMemoryBytes()
		if !ok {
			return runtimeMemoryPlan{}, fmt.Errorf("recipe declares runtime kind %q without its service block and memory model", r.Runtime.Kind)
		}
		l := r.Service.LlamaCpp
		// llama.cpp does not quantize the KV cache unless it is told to, and
		// this schema does not let a recipe tell it to, so the dtype the
		// receipt reports is the one llama.cpp uses.
		return runtimeMemoryPlan{budgetBytes: planned, kvCacheDType: "f16", maxModelLen: l.ContextSize, maxConcurrentRequests: l.Parallel}, nil
	}
	fraction, ok := r.Service.MemoryFraction(r.Runtime.Kind)
	if !ok {
		return runtimeMemoryPlan{}, fmt.Errorf("recipe declares runtime kind %q without its service block", r.Runtime.Kind)
	}
	utilization, err := strconv.ParseFloat(fraction, 64)
	if err != nil {
		return runtimeMemoryPlan{}, fmt.Errorf("parse %s device memory fraction: %w", r.Runtime.Kind, err)
	}
	plan := runtimeMemoryPlan{utilization: utilization}
	switch r.Runtime.Kind {
	case "vllm":
		v := r.Service.VLLM
		plan.kvCacheDType, plan.maxModelLen, plan.maxConcurrentRequests = v.KVCacheDType, v.MaxModelLen, v.MaxNumSeqs
	case "sglang":
		s := r.Service.SGLang
		plan.kvCacheDType, plan.maxModelLen, plan.maxConcurrentRequests = s.KVCacheDType, s.ContextLength, s.MaxRunningRequests
	}
	return plan, nil
}

// verifyDisk checks free disk against this job's own requirement, minus
// whatever reservedBytes other running installs have already claimed. That
// claim can land on either disk pool depending on where downloads happen to
// write, so it is charged against both rather than split or guessed at.
func (h *HostExecutor) verifyDisk(ctx context.Context, r recipe.Recipe, artifactBytes, runtimeBytes, reservedBytes int64) (map[string]any, error) {
	system, err := h.inventory.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	node := resourceguard.Node{
		Name: system.Hostname, DataDiskAvailable: system.StorageAvailable, RuntimeDiskAvailable: system.DockerStorageAvailable,
		SharedDataRuntimeDisk: system.DockerSharesDataDisk,
	}
	// A transient Docker-inspect failure reads as zero bytes, which is not
	// the same as a full disk; failing a running download on it would be
	// wrong. GB10 machines have a single disk, so the data-disk reading is
	// the honest fallback.
	dockerDiskUnknown := system.DockerStorageAvailable <= 0
	if dockerDiskUnknown {
		node.RuntimeDiskAvailable = system.StorageAvailable
		node.SharedDataRuntimeDisk = true
	}
	if reservedBytes > 0 {
		node.DataDiskAvailable -= reservedBytes
		node.RuntimeDiskAvailable -= reservedBytes
	}
	results, err := resourceguard.CheckDisk([]resourceguard.Node{node}, 1, resourceguard.DiskPolicy{
		ArtifactBytes: artifactBytes, RuntimeBytes: runtimeBytes, SafetyMarginBytes: r.Requirements.SafetyMarginBytes,
	})
	receipt := map[string]any{
		"per_node": results, "artifact_bytes": artifactBytes, "runtime_disk_bytes": runtimeBytes,
		"safety_margin_bytes": r.Requirements.SafetyMarginBytes, "docker_disk_unknown": dockerDiskUnknown,
		"reserved_by_other_installs_bytes": reservedBytes,
	}
	if err != nil {
		if reservedBytes > 0 {
			return receipt, errors.New("not enough free space while another install is running, so wait for it to finish or free up space")
		}
		return receipt, err
	}
	return receipt, nil
}

func receiptInt64(value any, key string) int64 {
	receipt, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	switch number := receipt[key].(type) {
	case int64:
		return number
	case int:
		return int64(number)
	case float64:
		return int64(number)
	default:
		return 0
	}
}

func (h *HostExecutor) Completed(ctx context.Context, execution Execution, operation recipe.Operation, r recipe.Recipe, receipt json.RawMessage) bool {
	switch operation.Type {
	case "pull_image":
		return h.docker.ImageExists(ctx, r.Runtime.Reference())
	case "download_artifact":
		for i, artifact := range r.Artifacts {
			if !h.hf.Complete(artifact, h.artifactPath(r, i)) {
				return false
			}
		}
		return true
	case "write_generated_config":
		data, err := os.ReadFile(h.configPath(r))
		if err != nil || !bytes.Contains(data, []byte(r.Runtime.Digest)) {
			return false
		}
		for _, artifact := range r.Artifacts {
			if !bytes.Contains(data, []byte(artifact.Revision)) {
				return false
			}
		}
		return true
	case "create_container":
		state, err := h.docker.Container(ctx, h.resolveContainerName(ctx, r))
		// Labels alone say the container is ours and for this recipe version;
		// they say nothing about where it reads the model from, what it is
		// allowed to write, which image it runs, or which serve arguments it
		// carries. A recipe version is not a container's contents: the digest,
		// writable paths, serve command and environment can all change under a
		// version that stays put. One that disagrees on any of them is
		// not "already created" — it has to be built again (see staleMounts,
		// staleTmpfs, staleImage, staleCommand, staleEnvironment and
		// staleLaunch). This list has to stay in step with the one
		// replaceStaleContainer builds, because a container this call accepts
		// is never offered to that one.
		return err == nil && containerLabelsMatch(state.Labels, r) &&
			len(staleMounts(state, h.expectedMounts(r))) == 0 &&
			len(staleTmpfs(state, containerTmpfs(r))) == 0 &&
			staleImage(state, r) == nil &&
			len(staleCommand(state, r, execution.Placement)) == 0 &&
			len(staleEnvironment(state, containerEnvironment(r, execution.Placement))) == 0 &&
			len(staleLaunch(state, r, execution.Placement)) == 0
	case "start_container":
		state, err := h.docker.Container(ctx, h.resolveContainerName(ctx, r))
		return err == nil && state.Running
	case "wait_http":
		return h.health(ctx, r) == nil
	case "verify_openai_inference":
		_, err := h.verifyInference(ctx, r)
		return err == nil
	case "verify_media_generation":
		// The text kinds re-run their proof here, because asking a running
		// model for one short answer costs a second. A media proof is a real
		// generation and costs minutes at best, so a resumed job checks the
		// proof instead of repeating it: the receipt names the files the
		// verification produced, and they are still on disk and still
		// non-empty, or this returns false and the step runs again.
		return h.mediaGenerationProven(receipt)
	case "stop_container":
		state, err := h.docker.Container(ctx, h.resolveContainerName(ctx, r))
		return errors.Is(err, ErrContainerNotFound) || (err == nil && !state.Running)
	case "remove_container":
		_, err := h.docker.Container(ctx, h.resolveContainerName(ctx, r))
		_, configErr := os.Stat(filepath.Dir(h.configPath(r)))
		_, cacheErr := os.Stat(h.cachePath(r))
		return errors.Is(err, ErrContainerNotFound) && errors.Is(configErr, os.ErrNotExist) && errors.Is(cacheErr, os.ErrNotExist)
	case "remove_artifact_if_unshared":
		if !execution.RemoveArtifacts {
			return true
		}
		for i, artifact := range r.Artifacts {
			target := h.artifactPath(r, i)
			if execution.SharedArtifacts[artifactKey(artifact)] || execution.SharedArtifacts[target] {
				continue
			}
			if h.hf.Complete(artifact, target) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// startTimeout resolves the recipe's health-wait deadline. 0 or a negative
// value falls back to the default of 20 minutes.
func startTimeout(r recipe.Recipe) time.Duration {
	minutes := r.Runtime.StartTimeoutMinutes
	if minutes <= 0 {
		minutes = 20
	}
	return time.Duration(minutes) * time.Minute
}

func (h *HostExecutor) waitHTTP(ctx context.Context, r recipe.Recipe, progress Progress) (map[string]any, error) {
	timeout := startTimeout(r)
	deadline := time.Now().Add(timeout)
	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		if err := h.health(ctx, r); err == nil {
			return map[string]any{"url": h.modelURL(r) + healthPath(r), "attempts": attempt, "status": "ready"}, nil
		}
		// A dead container never becomes healthy — fail immediately with its
		// exit state and last output instead of burning the whole deadline.
		if attempt%5 == 0 {
			name := h.resolveContainerName(ctx, r)
			if state, err := h.docker.Container(ctx, name); err == nil && !state.Running {
				logs := h.docker.Logs(ctx, name, 40)
				detail := ""
				if logs != "" {
					detail = "; last container output:\n" + logs
				}
				return nil, fmt.Errorf("the model container exited during startup (%s)%s", state.Status, detail)
			}
		}
		if progress != nil {
			if err := progress(map[string]any{"url": h.modelURL(r) + healthPath(r), "attempt": attempt, "status": "waiting"}); err != nil {
				return nil, fmt.Errorf("persist health progress: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	logs := h.docker.Logs(ctx, h.resolveContainerName(ctx, r), 40)
	minutes := int(timeout / time.Minute)
	if logs != "" {
		return nil, fmt.Errorf("the model server did not become HTTP-ready within %d minutes; last container output:\n%s", minutes, logs)
	}
	return nil, fmt.Errorf("the model server did not become HTTP-ready within %d minutes", minutes)
}

// healthPath is the runtime's liveness route. vLLM, SGLang and llama-server
// all serve /health; ComfyUI does not, and its documented /queue answers with
// the queue state as JSON. The route is deliberately not "/": that serves the
// ComfyUI web app, which basement never exposes to anyone, so treating it as
// a health signal would be checking a page nobody may open.
func healthPath(r recipe.Recipe) string {
	if r.Runtime.Kind == "comfyui" {
		return "/queue"
	}
	return "/health"
}

func (h *HostExecutor) health(ctx context.Context, r recipe.Recipe) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.modelURL(r)+healthPath(r), nil)
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health returned %s", resp.Status)
	}
	return nil
}

func (h *HostExecutor) verifyInference(ctx context.Context, r recipe.Recipe) (map[string]any, error) {
	// Reasoning models spend their first tokens inside a think block before
	// any visible answer, so the budget must be large enough to finish
	// thinking; a small cap makes a healthy model look like it said nothing.
	body, _ := json.Marshal(map[string]any{"model": r.Service.ServedModelID, "messages": []map[string]string{{"role": "user", "content": "Reply with the single word ready."}}, "max_tokens": 512, "temperature": 0})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.modelURL(r)+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := *h.http
	client.Timeout = 5 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("inference request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read inference response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inference returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var result struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content    string `json:"content"`
				Reasoning  string `json:"reasoning_content"`
				Reasoning2 string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode inference response: %w; body: %s", err, truncateForError(raw, 2048))
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("inference returned no choices; body: %s", truncateForError(raw, 2048))
	}
	msg := result.Choices[0].Message
	answer := strings.TrimSpace(msg.Content)
	reasoning := strings.TrimSpace(msg.Reasoning + msg.Reasoning2)
	if answer == "" && reasoning == "" {
		return nil, fmt.Errorf("inference returned an empty model response (finish_reason %q); body: %s", result.Choices[0].FinishReason, truncateForError(raw, 2048))
	}
	return map[string]any{"endpoint": h.modelURL(r) + "/v1", "served_model_id": r.Service.ServedModelID, "response_non_empty": true, "answered": answer != "", "reasoning_only": answer == "" && reasoning != "", "finish_reason": result.Choices[0].FinishReason, "reported_model": result.Model}, nil
}

// mediaVerificationSeed is the seed the install proof always generates with.
// It is fixed so two installs of the same recipe on the same machine produce
// the same clip, which is what makes the receipt comparable; it is not a
// quality choice and no user ever sees it.
const mediaVerificationSeed = 1

// verifyMediaGeneration is the media counterpart of verify_openai_inference:
// the cheapest honest proof that the thing actually works. It runs the
// recipe's own graph at its minimum duration, its default short edge and its
// verification step count, and requires a non-empty file on disk afterwards.
// Nothing weaker would do: ComfyUI answers /queue as soon as its server
// process is up and before any weights are loaded, so the health check ahead
// of this one would report a working model that failed the moment anybody
// asked it for anything.
//
// What it deliberately does not do is generate a good clip. The proof has to
// show that 42 GB of weights loaded and that the graph reached its save node;
// quality is not evidence of either, and paying for it meant twenty sampler
// steps at roughly forty-six seconds each on every install and every start.
// The step count is the recipe's VerificationSamplerSteps instead, so what is
// left is the weight load, which is real and unavoidable, plus one step.
//
// The wait is the recipe's own start timeout, so a recipe that knows how long
// its smallest generation takes says so in one place and both the health wait
// and this step honour it.
func (h *HostExecutor) verifyMediaGeneration(ctx context.Context, r recipe.Recipe, progress Progress) (map[string]any, error) {
	c, ok := r.MediaGeneration()
	if !ok {
		return nil, fmt.Errorf("recipe %s is not a media model, so it has no generation to verify", r.ID)
	}
	name, ok := c.Graphs[recipe.ModeTextToVideo]
	if !ok {
		return nil, fmt.Errorf("recipe %s declares no %s workflow to verify with", r.ID, recipe.ModeTextToVideo)
	}
	raw, err := recipe.Graph(name)
	if err != nil {
		return nil, err
	}
	frames := c.Frames(c.MinBlocks)
	graph, err := recipe.RenderGraph(raw, recipe.GraphInputs{
		Prompt: "A slow pan across a quiet room.",
		Seed:   mediaVerificationSeed,
		Frames: frames,
		Width:  c.DefaultShortEdge,
		Height: c.DefaultShortEdge,
		Steps:  c.VerificationSamplerSteps,
	})
	if err != nil {
		return nil, err
	}
	destination := filepath.Join(h.GenerationRoot(r), "install-verification")
	// A previous attempt's files must not be able to pass for this one's.
	if err := os.RemoveAll(destination); err != nil {
		return nil, err
	}
	report := func(update GenerationProgressUpdate) error {
		if progress == nil {
			return nil
		}
		receipt := map[string]any{
			"status": "generating", "elapsed_seconds": int64(update.Elapsed.Seconds()),
		}
		if update.Queue != nil {
			receipt["queue_running"] = update.Queue.Running
			receipt["queue_pending"] = update.Queue.Pending
		}
		if update.Step != nil {
			receipt["progress_value"] = update.Step.Value
			receipt["progress_max"] = update.Step.Max
			if update.Step.Node != "" {
				receipt["progress_phase"] = update.Step.Node
			}
		}
		return progress(receipt)
	}
	outcome, err := RunGeneration(ctx, NewComfyUIClient(h.modelURL(r)), graph, h.GenerationRoot(r), destination, report)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"mode": recipe.ModeTextToVideo, "prompt_id": outcome.PromptID,
		"frames": frames, "blocks": c.MinBlocks, "width": c.DefaultShortEdge, "height": c.DefaultShortEdge,
		"steps": c.VerificationSamplerSteps,
		"seed":  mediaVerificationSeed, "output_files": outcome.Files, "bytes": outcome.Bytes,
		"duration_seconds": math.Round(outcome.Duration.Seconds()*10) / 10,
	}, nil
}

// mediaGenerationProven reads a recorded verification receipt and confirms
// what it claims is still true on disk. It is deliberately strict about the
// receipt's shape: an unreadable one proves nothing, and proving nothing
// means running the generation again.
func (h *HostExecutor) mediaGenerationProven(receipt json.RawMessage) bool {
	var recorded struct {
		OutputFiles []string `json:"output_files"`
		Bytes       int64    `json:"bytes"`
	}
	if json.Unmarshal(receipt, &recorded) != nil || recorded.Bytes <= 0 || len(recorded.OutputFiles) == 0 {
		return false
	}
	for _, name := range recorded.OutputFiles {
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	return true
}

// truncateForError keeps error payloads readable while preserving enough of
// the raw body to diagnose an unexpected response shape.
func truncateForError(raw []byte, limit int) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		return s[:limit] + "… (truncated)"
	}
	return s
}

// measureMinDecodeDuration is how long measureThroughput lets a model decode
// before it cuts the sample off. A fixed token count finishes in only a few
// seconds on a fast model — the previous 256-token budget measured DeepSeek
// V4 Flash at 36.5 tok/s against 34.7-44.9 tok/s across the qualification's
// own multi-thousand-token runs, because a few seconds of decode is not
// enough to average out normal run-to-run jitter. Targeting a duration
// instead keeps the sample window comparable across models of very
// different speeds: a slow model just produces fewer tokens in the same
// thirty seconds, rather than the measurement ending before it has settled.
// Servers with continuous usage stats report the cumulative token count on
// the last chunk seen before cancellation; other servers use the existing
// streamed-chunk approximation.
const measureMinDecodeDuration = 30 * time.Second

// measureMaxTokens is a safety ceiling only, not the thing being targeted.
// It exists so a model fast enough to decode this many tokens inside
// measureMinDecodeDuration (well over 100 tok/s) still has a defined stopping
// point, and so the request prompt below (written to keep a model talking
// for a while) cannot run away indefinitely on its own.
const measureMaxTokens = 4096

// measureThroughput streams a generation and reports device-measured decode
// speed and time to first token, so the catalog shows numbers observed on
// this Spark rather than editorial estimates. It requests cumulative usage
// on each chunk so a duration-limited stream retains an accurate token count
// where the server supports it. See measureMinDecodeDuration for why the
// sample runs to a duration rather than a fixed token count.
func (h *HostExecutor) measureThroughput(ctx context.Context, r recipe.Recipe) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"model": r.Service.ServedModelID,
		// Asked to write at length so the model keeps decoding for the whole
		// measurement window instead of finishing early on a short answer;
		// measureMaxTokens is the real stopping point, this is just the
		// instruction that gets it there.
		"messages":    []map[string]string{{"role": "user", "content": "Write a long, detailed essay of at least 2000 words on why local inference on personal hardware matters. Cover privacy, cost at scale, latency, offline reliability, and customization, with concrete examples throughout, and keep expanding on each point rather than summarizing early."}},
		"max_tokens":  measureMaxTokens,
		"temperature": 0,
		"stream":      true,
		"stream_options": map[string]any{
			"include_usage":          true,
			"continuous_usage_stats": true,
		},
	})
	// Cancelling once the duration target is hit (below) is what actually
	// stops the request; without a request-scoped context there would be no
	// way to end the stream early short of waiting for the model to finish
	// on its own.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, h.modelURL(r)+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := *h.http
	client.Timeout = 5 * time.Minute
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("benchmark request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("benchmark returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var firstToken, firstChunk time.Time
	var completionTokens int64
	var chunkCount int64
	var samples []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		if len(samples) < 3 {
			samples = append(samples, truncateForError([]byte(payload), 512))
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content    string `json:"content"`
					Reasoning  string `json:"reasoning_content"`
					Reasoning2 string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
			completionTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) > 0 {
			if firstChunk.IsZero() {
				firstChunk = time.Now()
			}
			delta := chunk.Choices[0].Delta
			if delta.Content != "" || delta.Reasoning != "" || delta.Reasoning2 != "" {
				if firstToken.IsZero() {
					firstToken = time.Now()
				}
				chunkCount++
			}
		}
		// The duration target, not measureMaxTokens, is what normally ends
		// the sample: once thirty seconds of decode have been observed,
		// cancelling the request stops the model generating further tokens
		// no one is going to count. The usage update above deliberately
		// happens before this check so continuous usage retains the newest
		// cumulative count. completionTokens falls back to chunkCount below
		// for a server that does not send continuous usage.
		if !firstToken.IsZero() && time.Since(firstToken) >= measureMinDecodeDuration {
			cancel()
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read benchmark stream: %w", err)
	}
	finished := time.Now()
	// Some servers deliver text under delta fields this parser does not know;
	// the usage frame still proves tokens were generated, so measure from the
	// first delta rather than failing a working model.
	if chunkCount == 0 && completionTokens > 0 && !firstChunk.IsZero() {
		firstToken = firstChunk
		chunkCount = completionTokens
	}
	if firstToken.IsZero() || chunkCount == 0 {
		return nil, fmt.Errorf("benchmark stream produced no tokens; first stream chunks: %s", strings.Join(samples, " | "))
	}
	if completionTokens == 0 {
		completionTokens = chunkCount
	}
	// Servers that support continuous usage provide the cumulative token count
	// from the last streamed chunk. For servers that do not support the
	// extension, completionTokens is the chunk count fallback above. The decode
	// window starts at firstToken, not at started: time to first token is prefill
	// and queueing, not decode, so it is already excluded from the tok/s window
	// below rather than counted as free generation time. The started-based
	// fallback only covers the pathological case where firstToken and finished
	// land on the same clock tick.
	generation := finished.Sub(firstToken).Seconds()
	if generation <= 0 {
		generation = finished.Sub(started).Seconds()
	}
	tokensPerSecond := float64(completionTokens) / generation
	return map[string]any{
		"tokens_per_second":      math.Round(tokensPerSecond*10) / 10,
		"time_to_first_token_ms": firstToken.Sub(started).Milliseconds(),
		"completion_tokens":      completionTokens,
		"measured":               true,
	}, nil
}

func (h *HostExecutor) artifactPath(r recipe.Recipe, index int) string {
	artifact := r.Artifacts[index]
	return filepath.Join(h.dataDir, "artifacts", strings.ReplaceAll(artifact.Repository, "/", "--"), artifact.Revision)
}

// expectedMounts is what a container for r would be created with right now.
func (h *HostExecutor) expectedMounts(r recipe.Recipe) map[string]string {
	paths := make([]string, len(r.Artifacts))
	for index := range r.Artifacts {
		paths[index] = h.artifactPath(r, index)
	}
	return containerMounts(r, paths, h.cachePath(r), h.mediaMounts(r))
}

// GenerationRoot is where a media model's generations live under a data
// directory. It is the host side of the container's output directory, so
// results land straight into a directory basement owns and nothing is ever
// read back out of the container. It is a plain function because two layers
// need the same answer: the step executor mounts it, and the generation API
// serves files out of it.
func GenerationRoot(dataDir, recipeID string) string {
	return filepath.Join(dataDir, "generations", recipeID)
}

func (h *HostExecutor) GenerationRoot(r recipe.Recipe) string {
	return GenerationRoot(h.dataDir, r.ID)
}

// generationInputRoot is where source images are staged for the modes that
// take one. It is a dot-directory inside the generation root so a listing of
// results never has to skip it by name.
func (h *HostExecutor) generationInputRoot(r recipe.Recipe) string {
	return filepath.Join(h.GenerationRoot(r), ".input")
}

// mediaMounts are the read-write bind mounts a media runtime needs, keyed by
// container path. Every other kind gets none, so nothing that shipped before
// this kind changes shape.
func (h *HostExecutor) mediaMounts(r recipe.Recipe) map[string]string {
	c, ok := r.MediaGeneration()
	if !ok {
		return nil
	}
	return map[string]string{
		c.OutputDirectory: h.GenerationRoot(r),
		c.InputDirectory:  h.generationInputRoot(r),
	}
}

// writeGeneratedConfig records how this machine launches r: the pinned image
// and revisions, the container name, and the exact command. It is the body of
// the write_generated_config step, and the one place that record is produced.
func (h *HostExecutor) writeGeneratedConfig(execution Execution, r recipe.Recipe) (map[string]any, error) {
	path := h.configPath(r)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	modelRevisions := make(map[string]string, len(r.Artifacts))
	for _, artifact := range r.Artifacts {
		modelRevisions[artifact.Role] = artifact.Revision
	}
	entrypoint, arguments, err := runtimeCommand(r, execution.Placement)
	if err != nil {
		return nil, err
	}
	config := map[string]any{"recipe_id": r.ID, "recipe_version": r.Version, "image": r.Runtime.Reference(), "model_revisions": modelRevisions, "container_name": containerName(r), "runtime_kind": r.Runtime.Kind, "entrypoint": entrypoint, "arguments": arguments}
	if execution.Placement.Distributed() {
		config["node_role"] = execution.Placement.Role
		config["node_count"] = execution.Placement.NodeCount
	}
	if err := atomicJSON(path, config, 0o640); err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "contains_secrets": false}, nil
}

// ensureGeneratedConfig writes that record when it is not on disk. Docker
// takes the container's command from the recipe, not from this file, so a
// missing record cannot stop a container from being built; it would leave a
// running container whose launch is written down nowhere, which is what a
// remove_container followed by a start would produce.
func (h *HostExecutor) ensureGeneratedConfig(execution Execution, r recipe.Recipe) error {
	if _, err := os.Stat(h.configPath(r)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := h.writeGeneratedConfig(execution, r)
	return err
}

// createContainer builds the container for r against this machine's current
// layout. Callers about to create must go through it rather than reaching
// for the Docker client, so the mount sources always come from one place.
func (h *HostExecutor) createContainer(ctx context.Context, execution Execution, r recipe.Recipe) (string, error) {
	artifactPaths := make([]string, len(r.Artifacts))
	for index := range r.Artifacts {
		artifactPaths[index] = h.artifactPath(r, index)
	}
	cachePath := h.cachePath(r)
	if err := os.MkdirAll(cachePath, 0o750); err != nil {
		return "", err
	}
	writable := h.mediaMounts(r)
	for _, host := range writable {
		if err := os.MkdirAll(host, 0o750); err != nil {
			return "", err
		}
	}
	if err := h.writeComfyUIRuntimeState(r, cachePath); err != nil {
		return "", err
	}
	return h.docker.Create(ctx, containerName(r), r.Runtime.Reference(), artifactPaths, cachePath, writable, r, execution.Placement)
}

// writeComfyUIRuntimeState puts the two things a media runtime needs before
// it can start into the container's one writable persistent bind: the model
// folder map, because ComfyUI has no command-line flag per model folder and
// its documented alternative is a file, and an empty directory for its own
// state, because the container root is read-only. Both are rewritten every
// time a container is created, so neither can describe a layout the recipe
// has since moved off. Every other runtime kind gets nothing here.
func (h *HostExecutor) writeComfyUIRuntimeState(r recipe.Recipe, cachePath string) error {
	if _, ok := r.MediaGeneration(); !ok {
		return nil
	}
	document := comfyUIModelPaths(r)
	if strings.TrimSpace(document) == "" {
		return fmt.Errorf("the recipe pins no model folders, so the media runtime would start with no weights to load")
	}
	name := filepath.Join(cachePath, filepath.Base(comfyUIModelPathsFile))
	if err := os.WriteFile(name, []byte(document), 0o640); err != nil {
		return fmt.Errorf("write the media runtime's model paths: %w", err)
	}
	return os.MkdirAll(filepath.Join(cachePath, filepath.Base(comfyUIUserDirectory)), 0o750)
}

// containerDrift is everything about an existing container of ours that no
// longer matches what creating it today would produce. Each kind is a
// different story and the receipt tells them apart.
type containerDrift struct {
	Mounts      []mountMismatch
	Writable    []mountMismatch
	Command     []commandMismatch
	Launch      []launchMismatch
	Environment []environmentMismatch
	Image       *imageMismatch
}

func (d containerDrift) found() bool {
	return len(d.Mounts) > 0 || len(d.Writable) > 0 || len(d.Command) > 0 || len(d.Launch) > 0 || len(d.Environment) > 0 || d.Image != nil
}

// replaceStaleContainer removes a container of ours that no longer matches
// this machine or this recipe: one still pointed at a directory this manager
// has moved away from (an install adopted across the rename (spec 10) is the
// case that produced this, its container holding binds under the pre-rename
// data directory long after the files moved), one built before its recipe
// declared a writable path, one still running the image digest the recipe
// pinned before this one, one carrying serve arguments the recipe no longer
// declares, or, on a two-Spark model, one still holding the fabric address the
// other rank had during an earlier deployment. It returns what disagreed, so
// the caller can create the container again and the receipt can say why.
// Nothing is touched unless something positively disagrees.
func (h *HostExecutor) replaceStaleContainer(ctx context.Context, r recipe.Recipe, placement Placement) (containerDrift, error) {
	name := h.resolveContainerName(ctx, r)
	state, err := h.docker.Container(ctx, name)
	if err != nil || !containerLabelsMatch(state.Labels, r) {
		return containerDrift{}, nil
	}
	drift := containerDrift{
		Mounts:      staleMounts(state, h.expectedMounts(r)),
		Writable:    staleTmpfs(state, containerTmpfs(r)),
		Command:     staleCommand(state, r, placement),
		Launch:      staleLaunch(state, r, placement),
		Environment: staleEnvironment(state, containerEnvironment(r, placement)),
		Image:       staleImage(state, r),
	}
	if !drift.found() {
		return containerDrift{}, nil
	}
	reason := "the addresses this job resolved"
	switch {
	case len(drift.Mounts) > 0:
		reason = drift.Mounts[0].Expected
	case drift.Image != nil:
		reason = drift.Image.Expected
	case len(drift.Writable) > 0:
		reason = "the writable paths this recipe declares"
	case len(drift.Command) > 0:
		reason = "the serve arguments this recipe declares"
	case len(drift.Environment) > 0:
		reason = "the environment this runtime needs"
	}
	if err := h.docker.Stop(ctx, name); err != nil {
		return containerDrift{}, fmt.Errorf("stop the model container so it can be rebuilt against %s: %w", reason, err)
	}
	if err := h.docker.Remove(ctx, name); err != nil {
		return containerDrift{}, fmt.Errorf("remove the model container so it can be rebuilt against %s: %w", reason, err)
	}
	return drift, nil
}

// verifyRuntimeInputs checks that every host path the generated command
// names is really on this machine before a container is created or started.
// The runtime validates its own arguments and dies with a language-level
// traceback when one names a file it cannot find; whether a file this
// manager downloaded is on disk is a manager-side fact, and it must be
// reported as a plain sentence naming the file.
func (h *HostExecutor) verifyRuntimeInputs(r recipe.Recipe) error {
	for index, artifact := range r.Artifacts {
		path := h.artifactPath(r, index)
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return fmt.Errorf("the downloaded files for %s are not on this machine at %s, so the model cannot start; install it again to download them", artifact.Repository, path)
		}
	}
	for _, reference := range runtimeFileReferences(r) {
		index, ok := r.ArtifactIndex(reference.Role)
		if !ok {
			return fmt.Errorf("the recipe tells the runtime to read %s from its %s files, but it declares no %s artifact", reference.Name, reference.Role, reference.Role)
		}
		path := filepath.Join(h.artifactPath(r, index), reference.Name)
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%s is missing from the downloaded model files at %s, and the recipe tells the runtime to read it, so the model cannot start; install it again to download the missing file", reference.Name, path)
		}
	}
	return nil
}

func (h *HostExecutor) configPath(r recipe.Recipe) string {
	return filepath.Join(h.dataDir, "configs", r.ID, fmt.Sprint(r.Version), "launch.json")
}
func (h *HostExecutor) cachePath(r recipe.Recipe) string {
	return filepath.Join(h.dataDir, "caches", r.ID, fmt.Sprint(r.Version))
}
func (h *HostExecutor) modelURL(r recipe.Recipe) string {
	return fmt.Sprintf("http://127.0.0.1:%d", r.Service.DefaultHostPort)
}

// containerName is the name new containers are created under. Callers about
// to create a container must always use this directly; callers looking up
// or operating on a container that may already exist should go through
// resolveContainerName instead (see its doc comment).
func containerName(r recipe.Recipe) string { return fmt.Sprintf("basement-%s-v%d", r.ID, r.Version) }

// legacyContainerName is the pre-rename (spec 10) naming scheme. Containers
// created before the rename still carry it — live containers are never
// renamed — so lookups fall back to it via resolveContainerName.
func legacyContainerName(r recipe.Recipe) string {
	return fmt.Sprintf("runonspark-%s-v%d", r.ID, r.Version)
}

// resolveContainerName returns the name r's container actually has on the
// host right now: the current scheme if a container exists under it,
// otherwise the pre-rename scheme if one exists there instead, otherwise the
// current scheme (nothing exists yet, so that is what creating one will
// use). This is for callers that look up or operate on a container that may
// already exist; a step that is about to CREATE a container must call
// containerName directly so new containers always get the current name.
func (h *HostExecutor) resolveContainerName(ctx context.Context, r recipe.Recipe) string {
	name := containerName(r)
	if _, err := h.docker.Container(ctx, name); err == nil {
		return name
	}
	legacy := legacyContainerName(r)
	if _, err := h.docker.Container(ctx, legacy); err == nil {
		return legacy
	}
	return name
}

// containerLabelsMatch reports whether labels identify r, reading the
// current label namespace and falling back to the pre-rename (spec 10) one
// so a container created before the rename is still recognized.
func containerLabelsMatch(labels map[string]string, r recipe.Recipe) bool {
	recipeID := labels[labelRecipeID]
	if recipeID == "" {
		recipeID = labels[legacyLabelRecipeID]
	}
	version := labels[labelRecipeVersion]
	if version == "" {
		version = labels[legacyLabelRecipeVersion]
	}
	return recipeID == r.ID && version == fmt.Sprint(r.Version)
}

// ArtifactKey identifies a pinned artifact independently of the recipe that
// references it, so removal can recognize artifacts shared across recipes.
func ArtifactKey(a recipe.Artifact) string { return artifactKey(a) }

func artifactKey(a recipe.Artifact) string { return a.Repository + "@" + a.Revision }

func (h *HostExecutor) removeOwnedArtifact(target string) error {
	root := filepath.Join(h.dataDir, "artifacts")
	absRoot, _ := filepath.Abs(root)
	absTarget, _ := filepath.Abs(target)
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("artifact removal escaped managed root")
	}
	return os.RemoveAll(absTarget)
}

func (h *HostExecutor) removeOwnedConfig(r recipe.Recipe) error {
	target := filepath.Dir(h.configPath(r))
	root := filepath.Join(h.dataDir, "configs")
	absRoot, _ := filepath.Abs(root)
	absTarget, _ := filepath.Abs(target)
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("configuration removal escaped managed root")
	}
	return os.RemoveAll(absTarget)
}

func (h *HostExecutor) removeOwnedCache(r recipe.Recipe) error {
	target := h.cachePath(r)
	root := filepath.Join(h.dataDir, "caches")
	absRoot, _ := filepath.Abs(root)
	absTarget, _ := filepath.Abs(target)
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("cache removal escaped managed root")
	}
	return os.RemoveAll(absTarget)
}
