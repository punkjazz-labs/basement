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

	"github.com/punkjazz-labs/runonspark-manager/internal/inventory"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
	"github.com/punkjazz-labs/runonspark-manager/internal/resourceguard"
)

type HostExecutor struct {
	dataDir   string
	inventory inventory.Provider
	docker    *DockerClient
	hf        *HFClient
	http      *http.Client
}

func NewHostExecutor(dataDir, dockerSocket string, provider inventory.Provider) *HostExecutor {
	return &HostExecutor{dataDir: dataDir, inventory: provider, docker: NewDockerClient(dockerSocket), hf: NewHFClient(), http: &http.Client{Timeout: 30 * time.Second}}
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
			if state, err := h.docker.Container(ctx, containerName(r)); err == nil && state.Running {
				return map[string]any{"already_running": true, "container_id": state.ID}, nil
			}
		}
		return h.verifyMemory(ctx, r, operation.Type == "verify_memory")
	case "verify_disk":
		return h.verifyDisk(ctx, r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes)
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
		if _, err := h.verifyDisk(ctx, r, r.TotalArtifactBytes(), r.Runtime.ImageDiskBytes); err != nil {
			return nil, fmt.Errorf("refusing image pull: %w", err)
		}
		guarded := func(value any) error {
			if _, err := h.verifyDisk(ctx, r, r.TotalArtifactBytes(), 0); err != nil {
				return fmt.Errorf("disk reserve reached during image pull: %w", err)
			}
			if progress != nil {
				return progress(value)
			}
			return nil
		}
		return h.docker.Pull(ctx, r.Runtime.Reference(), guarded)
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
				if _, err := h.verifyDisk(ctx, r, remainingArtifacts, 0); err != nil {
					return fmt.Errorf("disk reserve reached during model download: %w", err)
				}
				if progress != nil {
					return progress(value)
				}
				return nil
			}
			receipt, err := h.hf.Download(ctx, artifact, target, guarded)
			if err != nil {
				return nil, err
			}
			receipts = append(receipts, receipt)
		}
		return map[string]any{"artifacts": receipts}, nil
	case "write_generated_config":
		path := h.configPath(r)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
		modelRevisions := make(map[string]string, len(r.Artifacts))
		for _, artifact := range r.Artifacts {
			modelRevisions[artifact.Role] = artifact.Revision
		}
		config := map[string]any{"recipe_id": r.ID, "recipe_version": r.Version, "image": r.Runtime.Reference(), "model_revisions": modelRevisions, "container_name": containerName(r), "arguments": vllmArgs(r)}
		if err := atomicJSON(path, config, 0o640); err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "contains_secrets": false}, nil
	case "create_container":
		artifactPaths := make([]string, len(r.Artifacts))
		for index := range r.Artifacts {
			artifactPaths[index] = h.artifactPath(r, index)
		}
		cachePath := h.cachePath(r)
		if err := os.MkdirAll(cachePath, 0o750); err != nil {
			return nil, err
		}
		id, err := h.docker.Create(ctx, containerName(r), r.Runtime.Reference(), artifactPaths, cachePath, r)
		if err != nil {
			return nil, err
		}
		return map[string]any{"container_id": id, "container_name": containerName(r)}, nil
	case "start_container":
		if err := h.docker.Start(ctx, containerName(r)); err != nil {
			return nil, err
		}
		state, err := h.docker.Container(ctx, containerName(r))
		if err != nil {
			return nil, err
		}
		return map[string]any{"container_id": state.ID, "running": state.Running}, nil
	case "stop_container":
		if err := h.docker.Stop(ctx, containerName(r)); err != nil {
			return nil, err
		}
		// Containers from earlier recipe versions (cancelled installs,
		// upgrades) hold the same host port; stop them too.
		stopped := []string{containerName(r)}
		if managed, err := h.docker.ManagedContainers(ctx); err == nil {
			for _, container := range managed {
				if container.RecipeID == r.ID && container.Running && container.Name != containerName(r) {
					if err := h.docker.Stop(ctx, container.Name); err == nil {
						stopped = append(stopped, container.Name)
					}
				}
			}
		}
		return map[string]any{"container_name": containerName(r), "stopped": true, "stopped_containers": stopped}, nil
	case "remove_container":
		if err := h.docker.Remove(ctx, containerName(r)); err != nil {
			return nil, err
		}
		if err := h.removeOwnedConfig(r); err != nil {
			return nil, err
		}
		if err := h.removeOwnedCache(r); err != nil {
			return nil, err
		}
		return map[string]any{"container_name": containerName(r), "removed": true, "generated_config_removed": true, "compilation_cache_removed": true}, nil
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
	utilization, err := strconv.ParseFloat(r.Service.VLLM.GPUMemoryUtil, 64)
	if err != nil {
		return nil, fmt.Errorf("parse GPU memory utilization: %w", err)
	}
	node := resourceguard.Node{
		Name: system.Hostname, SystemMemoryTotal: system.MemoryTotal, SystemMemoryAvailable: system.MemoryAvailable,
		GPUMemoryTotal: system.GPUMemoryTotal, GPUMemoryFree: system.GPUMemoryFree,
	}
	results, err := resourceguard.CheckMemory([]resourceguard.Node{node}, r.Topology.SparkCount, resourceguard.MemoryPolicy{
		MinimumTotalBytes: r.Requirements.MinimumMemoryBytes, HostReserveBytes: r.Requirements.MemoryReserveBytes,
		GPUUtilization: utilization, RequireLiveCapacity: requireLive,
	})
	receipt := map[string]any{"per_node": results, "live_capacity": requireLive, "kv_cache_dtype": r.Service.VLLM.KVCacheDType, "max_model_len": r.Service.VLLM.MaxModelLen, "max_num_seqs": r.Service.VLLM.MaxNumSeqs}
	if err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (h *HostExecutor) verifyDisk(ctx context.Context, r recipe.Recipe, artifactBytes, runtimeBytes int64) (map[string]any, error) {
	system, err := h.inventory.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	node := resourceguard.Node{
		Name: system.Hostname, DataDiskAvailable: system.StorageAvailable, RuntimeDiskAvailable: system.DockerStorageAvailable,
		SharedDataRuntimeDisk: system.DockerSharesDataDisk,
	}
	results, err := resourceguard.CheckDisk([]resourceguard.Node{node}, r.Topology.SparkCount, resourceguard.DiskPolicy{
		ArtifactBytes: artifactBytes, RuntimeBytes: runtimeBytes, SafetyMarginBytes: r.Requirements.SafetyMarginBytes,
	})
	receipt := map[string]any{"per_node": results, "artifact_bytes": artifactBytes, "runtime_disk_bytes": runtimeBytes, "safety_margin_bytes": r.Requirements.SafetyMarginBytes}
	if err != nil {
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
		state, err := h.docker.Container(ctx, containerName(r))
		return err == nil && state.Labels["ai.runonspark.recipe-id"] == r.ID && state.Labels["ai.runonspark.recipe-version"] == fmt.Sprint(r.Version)
	case "start_container":
		state, err := h.docker.Container(ctx, containerName(r))
		return err == nil && state.Running
	case "wait_http":
		return h.health(ctx, r) == nil
	case "verify_openai_inference":
		_, err := h.verifyInference(ctx, r)
		return err == nil
	case "stop_container":
		state, err := h.docker.Container(ctx, containerName(r))
		return errors.Is(err, ErrContainerNotFound) || (err == nil && !state.Running)
	case "remove_container":
		_, err := h.docker.Container(ctx, containerName(r))
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

func (h *HostExecutor) waitHTTP(ctx context.Context, r recipe.Recipe, progress Progress) (map[string]any, error) {
	deadline := time.Now().Add(20 * time.Minute)
	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		if err := h.health(ctx, r); err == nil {
			return map[string]any{"url": h.modelURL(r) + "/health", "attempts": attempt, "status": "ready"}, nil
		}
		// A dead container never becomes healthy — fail immediately with its
		// exit state and last output instead of burning the whole deadline.
		if attempt%5 == 0 {
			if state, err := h.docker.Container(ctx, containerName(r)); err == nil && !state.Running {
				logs := h.docker.Logs(ctx, containerName(r), 40)
				detail := ""
				if logs != "" {
					detail = "; last container output:\n" + logs
				}
				return nil, fmt.Errorf("the model container exited during startup (%s)%s", state.Status, detail)
			}
		}
		if progress != nil {
			if err := progress(map[string]any{"url": h.modelURL(r) + "/health", "attempt": attempt, "status": "waiting"}); err != nil {
				return nil, fmt.Errorf("persist health progress: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	logs := h.docker.Logs(ctx, containerName(r), 40)
	if logs != "" {
		return nil, fmt.Errorf("vLLM did not become HTTP-ready within 20 minutes; last container output:\n%s", logs)
	}
	return nil, errors.New("vLLM did not become HTTP-ready within 20 minutes")
}

func (h *HostExecutor) health(ctx context.Context, r recipe.Recipe) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.modelURL(r)+"/health", nil)
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
	var firstToken time.Time
	var completionTokens int64
	var chunkCount int64
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
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
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
		if len(chunk.Choices) > 0 && (chunk.Choices[0].Delta.Content != "" || chunk.Choices[0].Delta.Reasoning != "") {
			if firstToken.IsZero() {
				firstToken = time.Now()
			}
			chunkCount++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read benchmark stream: %w", err)
	}
	finished := time.Now()
	if firstToken.IsZero() || chunkCount == 0 {
		return nil, errors.New("benchmark stream produced no tokens")
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

func (h *HostExecutor) configPath(r recipe.Recipe) string {
	return filepath.Join(h.dataDir, "configs", r.ID, fmt.Sprint(r.Version), "launch.json")
}
func (h *HostExecutor) cachePath(r recipe.Recipe) string {
	return filepath.Join(h.dataDir, "caches", r.ID, fmt.Sprint(r.Version))
}
func (h *HostExecutor) modelURL(r recipe.Recipe) string {
	return fmt.Sprintf("http://127.0.0.1:%d", r.Service.DefaultHostPort)
}
func containerName(r recipe.Recipe) string { return fmt.Sprintf("runonspark-%s-v%d", r.ID, r.Version) }

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
