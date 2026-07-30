package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/punkjazz-labs/runonspark-manager/internal/inventory"
	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
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

func (h *HostExecutor) ArtifactPath(r recipe.Recipe) string {
	artifact := r.Artifacts[0]
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
			return nil, errors.New("DGX Spark hardware identity was not detected")
		}
		return map[string]any{"product_name": system.ProductName, "dgx_spark": true}, nil
	case "verify_disk":
		system, err := h.inventory.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		if system.StorageAvailable < r.RequiredBytes() {
			return nil, fmt.Errorf("insufficient storage: %d available, %d required including safety margin", system.StorageAvailable, r.RequiredBytes())
		}
		return map[string]any{"available_bytes": system.StorageAvailable, "required_bytes": r.RequiredBytes(), "safety_margin_bytes": r.Requirements.SafetyMarginBytes}, nil
	case "verify_port":
		if err := inventory.CheckPort(r.Service.DefaultHostPort); err != nil {
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
		return h.docker.Pull(ctx, r.Runtime.Reference(), progress)
	case "download_artifact":
		var receipts []map[string]any
		for i, artifact := range r.Artifacts {
			target := h.artifactPath(r, i)
			receipt, err := h.hf.Download(ctx, artifact, target, progress)
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
		config := map[string]any{"recipe_id": r.ID, "recipe_version": r.Version, "image": r.Runtime.Reference(), "model_revision": r.Artifacts[0].Revision, "container_name": containerName(r), "arguments": vllmArgs(r)}
		if err := atomicJSON(path, config, 0o640); err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "contains_secrets": false}, nil
	case "create_container":
		id, err := h.docker.Create(ctx, containerName(r), r.Runtime.Reference(), h.ArtifactPath(r), r)
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
		return map[string]any{"container_name": containerName(r), "stopped": true}, nil
	case "remove_container":
		if err := h.docker.Remove(ctx, containerName(r)); err != nil {
			return nil, err
		}
		if err := h.removeOwnedConfig(r); err != nil {
			return nil, err
		}
		return map[string]any{"container_name": containerName(r), "removed": true, "generated_config_removed": true}, nil
	case "wait_http":
		return h.waitHTTP(ctx, r, progress)
	case "verify_openai_inference":
		return h.verifyInference(ctx, r)
	case "remove_artifact_if_unshared":
		if !execution.RemoveArtifacts {
			return map[string]any{"retained": true, "bytes_retained": r.TotalArtifactBytes()}, nil
		}
		var reclaimed int64
		for i, artifact := range r.Artifacts {
			target := h.artifactPath(r, i)
			if !h.hf.Complete(artifact, target) {
				return nil, fmt.Errorf("refusing to remove unowned or incomplete artifact path %s", target)
			}
			if err := h.removeOwnedArtifact(target); err != nil {
				return nil, err
			}
			reclaimed += artifact.ExpectedBytes
		}
		return map[string]any{"removed": true, "reclaimed_bytes": reclaimed}, nil
	default:
		return nil, fmt.Errorf("operation %s is not implemented", operation.Type)
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
		return err == nil && bytes.Contains(data, []byte(r.Runtime.Digest)) && bytes.Contains(data, []byte(r.Artifacts[0].Revision))
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
		return errors.Is(err, ErrContainerNotFound) && errors.Is(configErr, os.ErrNotExist)
	case "remove_artifact_if_unshared":
		if !execution.RemoveArtifacts {
			return true
		}
		for i, artifact := range r.Artifacts {
			if h.hf.Complete(artifact, h.artifactPath(r, i)) {
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
	body, _ := json.Marshal(map[string]any{"model": r.Service.ServedModelID, "messages": []map[string]string{{"role": "user", "content": "Reply with the single word ready."}}, "max_tokens": 16, "temperature": 0})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.modelURL(r)+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := *h.http
	client.Timeout = 5 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("inference request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("inference returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var result struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content+result.Choices[0].Message.Reasoning) == "" {
		return nil, errors.New("inference returned an empty model response")
	}
	return map[string]any{"endpoint": h.modelURL(r) + "/v1", "served_model_id": r.Service.ServedModelID, "response_non_empty": true, "reported_model": result.Model}, nil
}

func (h *HostExecutor) artifactPath(r recipe.Recipe, index int) string {
	artifact := r.Artifacts[index]
	return filepath.Join(h.dataDir, "artifacts", strings.ReplaceAll(artifact.Repository, "/", "--"), artifact.Revision)
}

func (h *HostExecutor) configPath(r recipe.Recipe) string {
	return filepath.Join(h.dataDir, "configs", r.ID, fmt.Sprint(r.Version), "launch.json")
}
func (h *HostExecutor) modelURL(r recipe.Recipe) string {
	return fmt.Sprintf("http://127.0.0.1:%d", r.Service.DefaultHostPort)
}
func containerName(r recipe.Recipe) string { return fmt.Sprintf("runonspark-%s-v%d", r.ID, r.Version) }

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
