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

func NewHostExecutor(dataDir, dockerSocket string, provider inventory.Provider) *HostExecutor {
	return &HostExecutor{dataDir: dataDir, inventory: provider, docker: NewDockerClient(dockerSocket), hf: NewHFClient(), http: &http.Client{Timeout: 30 * time.Second}, retryDelays: networkRetryDelays}
}

// networkRetryDelays paces automatic retries of registry pulls and weight
// downloads: roughly two minutes of patience before a network problem is
// allowed to fail an install. Docker keeps completed layers and weight
// downloads resume from disk, so a retry only repeats the lost tail.
var networkRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

// transientNetworkError recognizes failures that a working machine recovers
// from on its own: resolver hiccups, dropped connections, registry blips.
// Docker flattens typed errors into event strings, so the match is textual
// by design. Anything else (auth, missing image, disk guard) surfaces
// immediately.
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
func (h *HostExecutor) retryNetwork(ctx context.Context, what string, progress Progress, op func() (map[string]any, error)) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		value, err := op()
		if err == nil || ctx.Err() != nil || !transientNetworkError(err) {
			return value, err
		}
		if attempt >= len(h.retryDelays) {
			if attempt > 0 {
				return nil, fmt.Errorf("%s failed %d times on network errors; check this machine's connection and DNS, then retry: %w", what, attempt+1, err)
			}
			return nil, err
		}
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
		if err := inventory.CheckPort(r.Service.DefaultHostPort); err != nil {
			// Our own leftover container (a cancelled install, an old recipe
			// version) is not a blocker: the start phase stops it. A foreign
			// process is.
			if managed, listErr := h.docker.ManagedContainers(ctx); listErr == nil {
				for _, container := range managed {
					if !container.Running {
						continue
					}
					if container.RecipeID == r.ID {
						return map[string]any{
							"host_port": r.Service.DefaultHostPort, "occupied_by_previous_install": container.Name, "freed_during_start": true,
						}, nil
					}
					// Every current recipe publishes the same host port; when
					// multi-model serving lands this needs the holder's real
					// port instead of assuming a conflict.
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
		stale, drift, err := h.replaceStaleContainer(ctx, r, execution.Placement)
		if err != nil {
			return nil, err
		}
		id, err := h.createContainer(ctx, execution, r)
		if err != nil {
			return nil, err
		}
		receipt := map[string]any{"container_id": id, "container_name": containerName(r)}
		if len(stale) > 0 {
			receipt["recreated_after_move"] = mountMismatchReceipt(stale)
		}
		if len(drift) > 0 {
			receipt["recreated_after_address_change"] = launchMismatchReceipt(drift)
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
		stale, drift, err := h.replaceStaleContainer(ctx, r, execution.Placement)
		if err != nil {
			return nil, err
		}
		if missing || len(stale) > 0 || len(drift) > 0 {
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
		if len(stale) > 0 {
			receipt["recreated_after_move"] = mountMismatchReceipt(stale)
		}
		// A two-Spark model meets at an address the kernel assigns to the
		// cabled port, and that address can differ after a reboot. The
		// container built by the last deployment still holds the old one, so
		// it is rebuilt here against the address this job just resolved.
		if len(drift) > 0 {
			receipt["recreated_after_address_change"] = launchMismatchReceipt(drift)
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
		// they say nothing about where it reads the model from. One whose
		// binds no longer match this machine's layout is not "already
		// created" — it has to be built again (see staleMounts).
		return err == nil && containerLabelsMatch(state.Labels, r) && len(staleMounts(state, h.expectedMounts(r))) == 0
	case "start_container":
		state, err := h.docker.Container(ctx, h.resolveContainerName(ctx, r))
		return err == nil && state.Running
	case "wait_http":
		return h.health(ctx, r) == nil
	case "verify_openai_inference":
		_, err := h.verifyInference(ctx, r)
		return err == nil
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

// healthPath is the runtime's liveness route. vLLM and SGLang both serve
// /health; a kind whose route differs adds a case here rather than changing
// the wait loop.
func healthPath(_ recipe.Recipe) string { return "/health" }

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

// truncateForError keeps error payloads readable while preserving enough of
// the raw body to diagnose an unexpected response shape.
func truncateForError(raw []byte, limit int) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		return s[:limit] + "… (truncated)"
	}
	return s
}

// measureThroughput streams a fixed generation and reports device-measured
// decode speed and time to first token, so the catalog shows numbers observed
// on this Spark rather than editorial estimates.
func (h *HostExecutor) measureThroughput(ctx context.Context, r recipe.Recipe) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"model":          r.Service.ServedModelID,
		"messages":       []map[string]string{{"role": "user", "content": "Explain, in about 200 plain-language words, why local inference on personal hardware matters."}},
		"max_tokens":     256,
		"temperature":    0,
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.modelURL(r)+"/v1/chat/completions", bytes.NewReader(body))
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
	return containerMounts(r, paths, h.cachePath(r))
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
	return h.docker.Create(ctx, containerName(r), r.Runtime.Reference(), artifactPaths, cachePath, r, execution.Placement)
}

// replaceStaleContainer removes a container of ours that is still pointed at
// a directory this manager has moved away from — an install adopted across
// the rename (spec 10) is the case that produced this, its container holding
// binds under the pre-rename data directory long after the files moved — or,
// on a two-Spark model, one still holding the fabric address the other rank
// had during an earlier deployment. It returns what disagreed, so the caller
// can create the container again and the receipt can say why. Nothing is
// touched unless a mount source or a launch flag positively disagrees.
func (h *HostExecutor) replaceStaleContainer(ctx context.Context, r recipe.Recipe, placement Placement) ([]mountMismatch, []launchMismatch, error) {
	name := h.resolveContainerName(ctx, r)
	state, err := h.docker.Container(ctx, name)
	if err != nil || !containerLabelsMatch(state.Labels, r) {
		return nil, nil, nil
	}
	stale := staleMounts(state, h.expectedMounts(r))
	drift := staleLaunch(state, r, placement)
	if len(stale) == 0 && len(drift) == 0 {
		return nil, nil, nil
	}
	reason := "the addresses this job resolved"
	if len(stale) > 0 {
		reason = stale[0].Expected
	}
	if err := h.docker.Stop(ctx, name); err != nil {
		return nil, nil, fmt.Errorf("stop the model container so it can be rebuilt against %s: %w", reason, err)
	}
	if err := h.docker.Remove(ctx, name); err != nil {
		return nil, nil, fmt.Errorf("remove the model container so it can be rebuilt against %s: %w", reason, err)
	}
	return stale, drift, nil
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
