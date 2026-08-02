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
	"sort"
	"strings"
	"sync"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
)

type DockerClient struct {
	client        *http.Client
	negotiateOnce sync.Once
	negotiated    string
}

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

// ImageSize reports the on-disk size Docker records for an image, when the
// image is present locally.
func (d *DockerClient) ImageSize(ctx context.Context, ref string) (int64, bool) {
	resp, err := d.request(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var payload struct {
		Size int64 `json:"Size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.Size <= 0 {
		return 0, false
	}
	return payload.Size, true
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
	// Docker reports one layer per event; a bar drawn from a single layer
	// shrinks and jumps as the reported layer changes. Aggregate every layer
	// into one monotonic total instead.
	type layerState struct {
		current, total int64
		done           bool
	}
	layers := map[string]*layerState{}
	last := map[string]any{"image": ref, "status": "pulling"}
	for scanner.Scan() {
		var event struct {
			Status, ID, Error string
			ProgressDetail    struct{ Current, Total int64 } `json:"progressDetail"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Error != "" {
			return nil, errors.New(event.Error)
		}
		if event.ID != "" {
			layer, ok := layers[event.ID]
			if !ok {
				layer = &layerState{}
				layers[event.ID] = layer
			}
			switch event.Status {
			case "Downloading":
				if event.ProgressDetail.Current > layer.current {
					layer.current = event.ProgressDetail.Current
				}
				if event.ProgressDetail.Total > layer.total {
					layer.total = event.ProgressDetail.Total
				}
			case "Download complete", "Already exists", "Pull complete":
				if layer.total > 0 {
					layer.current = layer.total
				}
				if event.Status != "Download complete" {
					layer.done = true
				}
			}
		}
		var bytesComplete, bytesTotal int64
		var layersDone int
		downloaded := true
		for _, layer := range layers {
			bytesComplete += layer.current
			bytesTotal += layer.total
			if layer.done {
				layersDone++
			}
			if layer.total > 0 && layer.current < layer.total {
				downloaded = false
			}
		}
		status := "Downloading"
		if len(layers) > 0 && downloaded {
			status = "Extracting"
		}
		last = map[string]any{
			"image": ref, "status": status,
			"bytes_complete": bytesComplete, "bytes_total": bytesTotal,
			"layers_done": layersDone, "layers_total": len(layers),
		}
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

func (d *DockerClient) Create(ctx context.Context, name, image string, artifactPaths []string, cachePath string, r recipe.Recipe) (string, error) {
	if len(artifactPaths) != len(r.Artifacts) || cachePath == "" {
		return "", errors.New("container artifact and cache paths are incomplete")
	}
	port := fmt.Sprintf("%d/tcp", r.Service.InternalPort)
	binds := make([]string, 0, len(artifactPaths)+1)
	for index, artifactPath := range artifactPaths {
		binds = append(binds, artifactPath+":"+artifactMountPath(r.Artifacts[index].Role)+":ro")
	}
	binds = append(binds, cachePath+":/root/.cache:rw")
	environment := make([]string, 0, len(r.Runtime.Environment)+1)
	for name, value := range r.Runtime.Environment {
		environment = append(environment, name+"="+value)
	}
	// The root filesystem is read-only and /root/.cache is the one writable
	// mount; Triton defaults to /root/.triton and crashes the engine on a
	// read-only rootfs, so steer it into the persistent cache.
	if _, overridden := r.Runtime.Environment["TRITON_CACHE_DIR"]; !overridden {
		environment = append(environment, "TRITON_CACHE_DIR=/root/.cache/triton")
	}
	sort.Strings(environment)
	// The model endpoint is loopback-only; the manager's authenticated /v1
	// proxy is the sole network path to it (ADR 0007).
	hostConfig := map[string]any{
		"Binds":          binds,
		"PortBindings":   map[string]any{port: []map[string]string{{"HostIp": "127.0.0.1", "HostPort": fmt.Sprint(r.Service.DefaultHostPort)}}},
		"DeviceRequests": []map[string]any{{"Driver": "nvidia", "Count": -1, "Capabilities": [][]string{{"gpu"}}}},
		"ReadonlyRootfs": true,
		"Tmpfs":          map[string]string{"/tmp": "rw,noexec,nosuid,size=8g"},
	}
	if r.Runtime.ShmBytes > 0 {
		hostConfig["ShmSize"] = r.Runtime.ShmBytes
	}
	if r.Runtime.MemoryLock {
		hostConfig["Ulimits"] = []map[string]any{{"Name": "memlock", "Soft": -1, "Hard": -1}}
	}
	if r.Runtime.IPCLock {
		hostConfig["CapAdd"] = []string{"IPC_LOCK"}
	}
	body := map[string]any{
		"Image":        image,
		"Entrypoint":   []string{"vllm"},
		"Cmd":          vllmArgs(r),
		"Env":          environment,
		"Labels":       map[string]string{"ai.runonspark.managed": "true", "ai.runonspark.recipe-id": r.ID, "ai.runonspark.recipe-version": fmt.Sprint(r.Version)},
		"ExposedPorts": map[string]any{port: map[string]any{}},
		"HostConfig":   hostConfig,
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

// ManagedContainer is a container this manager created, in any lifecycle
// state, identified by its labels.
type ManagedContainer struct {
	Name     string
	Running  bool
	RecipeID string
	Version  string
}

// ManagedContainers lists every container carrying the managed label,
// including stopped ones — leftovers from cancelled or superseded jobs.
func (d *DockerClient) ManagedContainers(ctx context.Context) ([]ManagedContainer, error) {
	filters := url.QueryEscape(`{"label":["ai.runonspark.managed=true"]}`)
	resp, err := d.request(ctx, http.MethodGet, "/containers/json?all=1&filters="+filters, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, dockerError(resp)
	}
	var body []struct {
		Names  []string
		State  string
		Labels map[string]string
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	containers := make([]ManagedContainer, 0, len(body))
	for _, entry := range body {
		name := ""
		if len(entry.Names) > 0 {
			name = strings.TrimPrefix(entry.Names[0], "/")
		}
		containers = append(containers, ManagedContainer{
			Name:     name,
			Running:  entry.State == "running",
			RecipeID: entry.Labels["ai.runonspark.recipe-id"],
			Version:  entry.Labels["ai.runonspark.recipe-version"],
		})
	}
	return containers, nil
}

// Logs returns the last lines of a container's output, for diagnostics when
// a model fails to come up. Docker multiplexes streams with an 8-byte frame
// header; strip those and keep printable text.
func (d *DockerClient) Logs(ctx context.Context, name string, tail int) string {
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/logs?stdout=1&stderr=1&tail="+fmt.Sprint(tail), nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return ""
	}
	var text strings.Builder
	for offset := 0; offset+8 <= len(raw); {
		size := int(raw[offset+4])<<24 | int(raw[offset+5])<<16 | int(raw[offset+6])<<8 | int(raw[offset+7])
		start := offset + 8
		end := start + size
		if size < 0 || end > len(raw) {
			// Not stream-multiplexed (tty container): return as-is.
			return strings.TrimSpace(string(raw))
		}
		text.Write(raw[start:end])
		offset = end
	}
	return strings.TrimSpace(text.String())
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
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+d.apiPrefix(ctx)+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return d.client.Do(req)
}

// apiPrefix negotiates the API version once per client: the daemon's own
// ApiVersion from the unversioned /version endpoint (accepted by every
// daemon) becomes the prefix for all calls. Never pin a literal version —
// old pins are rejected by new daemons ("client version too old") and new
// pins are rejected by old daemons.
func (d *DockerClient) apiPrefix(ctx context.Context) string {
	d.negotiateOnce.Do(func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
		if err != nil {
			return
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var version struct {
			APIVersion string `json:"ApiVersion"`
		}
		if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&version) == nil && version.APIVersion != "" {
			d.negotiated = "/v" + version.APIVersion
		}
		// On any failure the prefix stays empty: unversioned paths mean
		// "latest supported" and work on every daemon.
	})
	return d.negotiated
}

func dockerError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Errorf("docker returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
}

func vllmArgs(r recipe.Recipe) []string {
	v := r.Service.VLLM
	specConfig := map[string]any{"method": v.SpeculativeMethod, "num_speculative_tokens": v.SpeculativeTokens}
	if v.SpeculativeMoE != "" {
		specConfig["moe_backend"] = v.SpeculativeMoE
	}
	if v.SpeculativeModelRole != "" {
		specConfig["model"] = artifactMountPath(v.SpeculativeModelRole)
	}
	spec, _ := json.Marshal(specConfig)
	args := []string{"serve", "/model", "--host", "0.0.0.0", "--port", fmt.Sprint(r.Service.InternalPort), "--served-model-name", r.Service.ServedModelID,
		"--tensor-parallel-size", fmt.Sprint(v.TensorParallelSize), "--gpu-memory-utilization", v.GPUMemoryUtil, "--max-model-len", fmt.Sprint(v.MaxModelLen),
		"--max-num-seqs", fmt.Sprint(v.MaxNumSeqs), "--speculative-config", string(spec),
		"--reasoning-parser", v.ReasoningParser, "--tool-call-parser", v.ToolCallParser}
	args = appendOptional(args, "--kv-cache-dtype", v.KVCacheDType)
	args = appendOptional(args, "--attention-backend", v.AttentionBackend)
	args = appendOptional(args, "--moe-backend", v.MoEBackend)
	args = appendOptional(args, "--linear-backend", v.LinearBackend)
	args = appendOptional(args, "--load-format", v.LoadFormat)
	if v.MaxBatchedTokens > 0 {
		args = append(args, "--max-num-batched-tokens", fmt.Sprint(v.MaxBatchedTokens))
	}
	if v.MultimodalImageLimit > 0 {
		limit, _ := json.Marshal(map[string]int{"image": v.MultimodalImageLimit})
		args = append(args, "--limit-mm-per-prompt", string(limit))
	}
	if v.ChatTemplateFile != "" {
		args = append(args, "--chat-template", artifactMountPath("primary")+"/"+v.ChatTemplateFile)
	}
	// Always sent, explicit values included when both are false: a recipe's
	// "false" is a decision, and the model's own template default must not
	// win over it. Laguna ships enable_thinking defaulting to true, so
	// omitting the flag turned thinking on against the recipe's intent.
	options, _ := json.Marshal(v.ChatTemplate)
	args = append(args, "--default-chat-template-kwargs", string(options))
	generation, _ := json.Marshal(v.Generation)
	args = append(args, "--override-generation-config", string(generation))
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

func appendOptional(args []string, flag, value string) []string {
	if value == "" {
		return args
	}
	return append(args, flag, value)
}

func artifactMountPath(role string) string {
	if role == "primary" {
		return "/model"
	}
	return "/" + role
}
