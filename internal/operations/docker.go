package operations

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

type DockerClient struct{ client *http.Client }

var ErrContainerNotFound = errors.New("container not found")

type ContainerState struct {
	ID      string
	Running bool
	Status  string
	Labels  map[string]string
}

func NewDockerClient(socket string) *DockerClient {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &DockerClient{client: &http.Client{Transport: transport, Timeout: 0}}
}

func (d *DockerClient) Ping(ctx context.Context) error {
	resp, err := d.request(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return dockerError(resp)
	}
	return nil
}

func (d *DockerClient) ImageExists(ctx context.Context, ref string) bool {
	resp, err := d.request(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (d *DockerClient) Pull(ctx context.Context, ref string, progress Progress) (map[string]any, error) {
	path := "/images/create?fromImage=" + url.QueryEscape(ref)
	resp, err := d.request(ctx, http.MethodPost, path, nil)
	if err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, dockerError(resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	last := map[string]any{"image": ref, "status": "pulling"}
	for scanner.Scan() {
		var event struct {
			Status, ID, Error string
			ProgressDetail    map[string]any `json:"progressDetail"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Error != "" {
			return nil, errors.New(event.Error)
		}
		last = map[string]any{"image": ref, "status": event.Status, "layer": event.ID, "progress": event.ProgressDetail}
		if progress != nil {
			if err := progress(last); err != nil {
				return nil, fmt.Errorf("persist image progress: %w", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read image pull: %w", err)
	}
	last["digest_pinned"] = true
	return last, nil
}

func (d *DockerClient) Container(ctx context.Context, name string) (ContainerState, error) {
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return ContainerState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ContainerState{}, ErrContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ContainerState{}, dockerError(resp)
	}
	var body struct {
		ID    string
		State struct {
			Running bool
			Status  string
		}
		Config struct{ Labels map[string]string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ContainerState{}, err
	}
	return ContainerState{ID: body.ID, Running: body.State.Running, Status: body.State.Status, Labels: body.Config.Labels}, nil
}

func (d *DockerClient) Create(ctx context.Context, name, image, modelPath string, r recipe.Recipe) (string, error) {
	port := fmt.Sprintf("%d/tcp", r.Service.InternalPort)
	body := map[string]any{
		"Image":        image,
		"Entrypoint":   []string{"vllm"},
		"Cmd":          vllmArgs(r),
		"Labels":       map[string]string{"ai.runonspark.managed": "true", "ai.runonspark.recipe-id": r.ID, "ai.runonspark.recipe-version": fmt.Sprint(r.Version)},
		"ExposedPorts": map[string]any{port: map[string]any{}},
		"HostConfig": map[string]any{
			"Binds":          []string{modelPath + ":/model:ro"},
			"PortBindings":   map[string]any{port: []map[string]string{{"HostIp": "0.0.0.0", "HostPort": fmt.Sprint(r.Service.DefaultHostPort)}}},
			"DeviceRequests": []map[string]any{{"Driver": "nvidia", "Count": -1, "Capabilities": [][]string{{"gpu"}}}},
			"IpcMode":        "host", "ReadonlyRootfs": true,
			"Tmpfs": map[string]string{"/tmp": "rw,noexec,nosuid,size=4g", "/root/.cache": "rw,nosuid,size=8g"},
		},
	}
	encoded, _ := json.Marshal(body)
	resp, err := d.request(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		existing, inspectErr := d.Container(ctx, name)
		if inspectErr != nil {
			return "", dockerError(resp)
		}
		if existing.Labels["ai.runonspark.recipe-id"] != r.ID || existing.Labels["ai.runonspark.recipe-version"] != fmt.Sprint(r.Version) {
			return "", errors.New("container name is owned by another installation")
		}
		return existing.ID, nil
	}
	if resp.StatusCode != http.StatusCreated {
		return "", dockerError(resp)
	}
	var result struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (d *DockerClient) Start(ctx context.Context, name string) error {
	return d.action(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", http.StatusNoContent, http.StatusNotModified)
}
func (d *DockerClient) Stop(ctx context.Context, name string) error {
	return d.action(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop?t=30", http.StatusNoContent, http.StatusNotModified, http.StatusNotFound)
}
func (d *DockerClient) Remove(ctx context.Context, name string) error {
	return d.action(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?v=0&force=0", http.StatusNoContent, http.StatusNotFound)
}

func (d *DockerClient) action(ctx context.Context, method, path string, accepted ...int) error {
	resp, err := d.request(ctx, method, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, status := range accepted {
		if resp.StatusCode == status {
			return nil
		}
	}
	return dockerError(resp)
}

func (d *DockerClient) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.41"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return d.client.Do(req)
}

func dockerError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Errorf("docker returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
}

func vllmArgs(r recipe.Recipe) []string {
	v := r.Service.VLLM
	spec, _ := json.Marshal(map[string]any{"method": v.SpeculativeMethod, "num_speculative_tokens": v.SpeculativeTokens, "moe_backend": v.SpeculativeMoE})
	args := []string{"serve", "/model", "--host", "0.0.0.0", "--port", fmt.Sprint(r.Service.InternalPort), "--served-model-name", r.Service.ServedModelID,
		"--tensor-parallel-size", fmt.Sprint(v.TensorParallelSize), "--kv-cache-dtype", v.KVCacheDType, "--attention-backend", v.AttentionBackend,
		"--moe-backend", v.MoEBackend, "--gpu-memory-utilization", v.GPUMemoryUtil, "--max-model-len", fmt.Sprint(v.MaxModelLen),
		"--max-num-seqs", fmt.Sprint(v.MaxNumSeqs), "--max-num-batched-tokens", fmt.Sprint(v.MaxBatchedTokens), "--speculative-config", string(spec),
		"--load-format", v.LoadFormat, "--reasoning-parser", v.ReasoningParser, "--tool-call-parser", v.ToolCallParser}
	if v.TrustRemoteCode {
		args = append(args, "--trust-remote-code")
	}
	if v.ChunkedPrefill {
		args = append(args, "--enable-chunked-prefill")
	}
	if v.AsyncScheduling {
		args = append(args, "--async-scheduling")
	}
	if v.PrefixCaching {
		args = append(args, "--enable-prefix-caching")
	}
	if v.AutoToolChoice {
		args = append(args, "--enable-auto-tool-choice")
	}
	return args
}
