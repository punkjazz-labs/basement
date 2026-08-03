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
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

type DockerClient struct {
	client        *http.Client
	negotiateOnce sync.Once
	negotiated    string
}

// Container labels this manager sets on everything it creates, and reads
// back to recognize its own containers.
const (
	labelManaged       = "ai.basement.managed"
	labelRecipeID      = "ai.basement.recipe-id"
	labelRecipeVersion = "ai.basement.recipe-version"
	// labelNodeRole is set only on distributed containers, so a two-Spark
	// worker container is distinguishable from a single-node one on sight.
	labelNodeRole = "ai.basement.node-role"

	// Pre-rename (spec 10) label namespace. Containers created before the
	// rename still carry it — live containers are never relabeled — so
	// ManagedContainers keeps reading both namespaces; new containers only
	// ever get the current one.
	legacyLabelManaged       = "ai.runonspark.managed"
	legacyLabelRecipeID      = "ai.runonspark.recipe-id"
	legacyLabelRecipeVersion = "ai.runonspark.recipe-version"
)

var ErrContainerNotFound = errors.New("container not found")

type ContainerState struct {
	ID      string
	Running bool
	Status  string
	Labels  map[string]string
	// Mounts is where the container reads each of its mount points from on
	// this host, keyed by mount point. Docker fixes a container's binds when
	// it is created and never revisits them, so this is the only way to tell
	// that a container is still pointed at a directory that has since moved.
	Mounts map[string]string
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
		Mounts []struct {
			Source      string
			Destination string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ContainerState{}, err
	}
	mounts := make(map[string]string, len(body.Mounts))
	for _, mount := range body.Mounts {
		if mount.Destination != "" && mount.Source != "" {
			mounts[mount.Destination] = mount.Source
		}
	}
	return ContainerState{ID: body.ID, Running: body.State.Running, Status: body.State.Status, Labels: body.Config.Labels, Mounts: mounts}, nil
}

func (d *DockerClient) Create(ctx context.Context, name, image string, artifactPaths []string, cachePath string, r recipe.Recipe, placement Placement) (string, error) {
	if len(artifactPaths) != len(r.Artifacts) || cachePath == "" {
		return "", errors.New("container artifact and cache paths are incomplete")
	}
	port := fmt.Sprintf("%d/tcp", r.Service.InternalPort)
	binds := make([]string, 0, len(artifactPaths)+1)
	for index, artifactPath := range artifactPaths {
		binds = append(binds, artifactPath+":"+artifactMountPath(r.Artifacts[index].Role)+":ro")
	}
	binds = append(binds, cachePath+":"+cacheMountPath+":rw")
	environment := make([]string, 0, len(r.Runtime.Environment)+1)
	for name, value := range r.Runtime.Environment {
		environment = append(environment, name+"="+value)
	}
	for name, value := range interconnectEnvironment(r, placement) {
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
	if placement.Distributed() {
		// Ranks reach each other over the cabled fabric, which means the host
		// network namespace and the RDMA devices. Published ports mean nothing
		// under host networking, so the API server is told to bind loopback
		// itself (see vllmArgs) rather than relying on a port mapping.
		delete(hostConfig, "PortBindings")
		hostConfig["NetworkMode"] = "host"
		hostConfig["IpcMode"] = "host"
		hostConfig["Devices"] = []map[string]any{{"PathOnHost": "/dev/infiniband", "PathInContainer": "/dev/infiniband", "CgroupPermissions": "rwm"}}
		hostConfig["CapAdd"] = []string{"IPC_LOCK"}
		hostConfig["Ulimits"] = []map[string]any{{"Name": "memlock", "Soft": -1, "Hard": -1}}
	}
	labels := map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: fmt.Sprint(r.Version)}
	if placement.Distributed() {
		labels[labelNodeRole] = placement.Role
	}
	entrypoint, command, err := runtimeCommand(r, placement)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"Image":        image,
		"Entrypoint":   entrypoint,
		"Cmd":          command,
		"Env":          environment,
		"Labels":       labels,
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
		if existing.Labels[labelRecipeID] != r.ID || existing.Labels[labelRecipeVersion] != fmt.Sprint(r.Version) {
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
// including stopped ones — leftovers from cancelled or superseded jobs. Both
// the current and the pre-rename (spec 10) label namespace are matched
// (repeated "label" filter values are ORed by the Docker API), so a
// container created before the rename is still recognized as ours; it is
// never relabeled in place.
func (d *DockerClient) ManagedContainers(ctx context.Context) ([]ManagedContainer, error) {
	filters := url.QueryEscape(`{"label":["` + labelManaged + `=true","` + legacyLabelManaged + `=true"]}`)
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
		recipeID := entry.Labels[labelRecipeID]
		if recipeID == "" {
			recipeID = entry.Labels[legacyLabelRecipeID]
		}
		version := entry.Labels[labelRecipeVersion]
		if version == "" {
			version = entry.Labels[legacyLabelRecipeVersion]
		}
		containers = append(containers, ManagedContainer{
			Name:     name,
			Running:  entry.State == "running",
			RecipeID: recipeID,
			Version:  version,
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

// runtimeCommand returns the container entrypoint and arguments for the
// recipe's runtime kind and placement. A recipe whose kind and service block
// disagree never reaches here (the validator rejects it), so an error means
// the caller built a Recipe by hand.
func runtimeCommand(r recipe.Recipe, placement Placement) ([]string, []string, error) {
	switch r.Runtime.Kind {
	case "vllm":
		if r.Service.VLLM == nil {
			return nil, nil, errors.New("recipe declares runtime kind vllm without a service.vllm block")
		}
		return []string{"vllm"}, vllmArgs(r, placement), nil
	case "sglang":
		if r.Service.SGLang == nil {
			return nil, nil, errors.New("recipe declares runtime kind sglang without a service.sglang block")
		}
		if placement.Distributed() {
			return nil, nil, errors.New("distributed sglang serving is not yet supported")
		}
		return []string{"python3", "-m", "sglang.launch_server"}, sglangArgs(r), nil
	default:
		return nil, nil, fmt.Errorf("runtime kind %q is not supported", r.Runtime.Kind)
	}
}

// interconnectEnvironment merges the recipe's fabric environment for this
// node: what every rank needs, then this rank's own additions. A single-node
// placement gets nothing, so existing recipes are untouched.
func interconnectEnvironment(r recipe.Recipe, placement Placement) map[string]string {
	if !placement.Distributed() || r.Topology.Interconnect == nil {
		return nil
	}
	link := r.Topology.Interconnect
	merged := map[string]string{}
	for name, value := range link.SharedEnvironment {
		merged[name] = value
	}
	// This node's own cabled port beats the recipe's pinned names: the two
	// machines need not even have the cable in the same port as each other,
	// and each rank pins its transports to the port it actually holds.
	if detected, err := resolveFabric(r); err == nil {
		merged["NCCL_SOCKET_IFNAME"] = detected.NetDev
		merged["GLOO_SOCKET_IFNAME"] = detected.NetDev
		merged["TP_SOCKET_IFNAME"] = detected.NetDev
		if detected.HCA != "" {
			merged["NCCL_IB_HCA"] = detected.HCA
		}
	}
	perRole := link.HeadEnvironment
	if placement.Role == RoleWorker {
		perRole = link.WorkerEnvironment
	}
	for name, value := range perRole {
		merged[name] = value
	}
	return merged
}

func vllmArgs(r recipe.Recipe, placement Placement) []string {
	if r.Service.VLLM == nil {
		return nil
	}
	v := *r.Service.VLLM
	serveHost, servePort := serveEndpointArgs(r, placement)
	args := []string{"serve", "/model", "--host", serveHost, "--port", fmt.Sprint(servePort), "--served-model-name", r.Service.ServedModelID,
		"--tensor-parallel-size", fmt.Sprint(v.TensorParallelSize), "--gpu-memory-utilization", v.GPUMemoryUtil, "--max-model-len", fmt.Sprint(v.MaxModelLen),
		"--max-num-seqs", fmt.Sprint(v.MaxNumSeqs),
		"--reasoning-parser", v.ReasoningParser, "--tool-call-parser", v.ToolCallParser}
	if v.SpeculativeMethod != "" {
		specConfig := map[string]any{"method": v.SpeculativeMethod, "num_speculative_tokens": v.SpeculativeTokens}
		if v.SpeculativeMoE != "" {
			specConfig["moe_backend"] = v.SpeculativeMoE
		}
		if v.SpeculativeModelRole != "" {
			specConfig["model"] = artifactMountPath(v.SpeculativeModelRole)
		}
		spec, _ := json.Marshal(specConfig)
		args = append(args, "--speculative-config", string(spec))
	}
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
	return append(args, distributedArgs(r, placement)...)
}

// distributedArgs are the two-node launch flags the community DGX Spark
// recipe uses: plain vllm serve with --nnodes/--node-rank/--master-addr/
// --master-port, the multiprocessing executor, and --headless on the rank
// that serves no HTTP. There is no ray and no torchrun.
func distributedArgs(r recipe.Recipe, placement Placement) []string {
	if !placement.Distributed() {
		return nil
	}
	args := []string{
		"--distributed-executor-backend", "mp",
		"--nnodes", fmt.Sprint(placement.NodeCount),
		"--node-rank", fmt.Sprint(placement.Rank()),
		"--master-addr", placement.MasterAddress,
		"--master-port", fmt.Sprint(placement.MasterPort),
	}
	if placement.Role == RoleWorker {
		return append(args, "--headless")
	}
	return args
}

// serveEndpointArgs are the host and port the API server binds. Under host
// networking there is no port publishing, so the head binds the recipe's
// host port directly, and it binds loopback so the manager's authenticated
// /v1 proxy stays the only network path to the model (ADR 0007).
func serveEndpointArgs(r recipe.Recipe, placement Placement) (string, int) {
	if placement.Distributed() {
		return "127.0.0.1", r.Service.DefaultHostPort
	}
	return "0.0.0.0", r.Service.InternalPort
}

// sglangArgs builds the sglang.launch_server command line. Every flag beyond
// the four positional essentials is omitted when the recipe leaves it unset,
// so the runtime's own default applies and the manager never invents a value.
func sglangArgs(r recipe.Recipe) []string {
	if r.Service.SGLang == nil {
		return nil
	}
	s := *r.Service.SGLang
	// --enable-metrics is not a recipe choice: SGLang mounts /metrics only
	// behind this flag, and the console's telemetry tiles read that endpoint.
	args := []string{
		"--model-path", artifactMountPath("primary"),
		"--host", "0.0.0.0",
		"--port", fmt.Sprint(r.Service.InternalPort),
		"--served-model-name", r.Service.ServedModelID,
		"--enable-metrics",
	}
	if s.TensorParallelSize > 0 {
		args = append(args, "--tp-size", fmt.Sprint(s.TensorParallelSize))
	}
	args = appendOptional(args, "--mem-fraction-static", s.MemFractionStatic)
	if s.ContextLength > 0 {
		args = append(args, "--context-length", fmt.Sprint(s.ContextLength))
	}
	if s.MaxRunningRequests > 0 {
		args = append(args, "--max-running-requests", fmt.Sprint(s.MaxRunningRequests))
	}
	args = appendOptional(args, "--quantization", s.Quantization)
	args = appendOptional(args, "--kv-cache-dtype", s.KVCacheDType)
	args = appendOptional(args, "--attention-backend", s.AttentionBackend)
	args = appendOptional(args, "--speculative-algorithm", s.SpeculativeAlgorithm)
	if s.SpeculativeNumDraftTokens > 0 {
		args = append(args, "--speculative-num-draft-tokens", fmt.Sprint(s.SpeculativeNumDraftTokens))
	}
	if s.SpeculativeModelRole != "" {
		args = append(args, "--speculative-draft-model-path", artifactMountPath(s.SpeculativeModelRole))
	}
	if s.ChatTemplateFile != "" {
		args = append(args, "--chat-template", artifactMountPath("primary")+"/"+s.ChatTemplateFile)
	}
	args = appendOptional(args, "--tool-call-parser", s.ToolCallParser)
	args = appendOptional(args, "--reasoning-parser", s.ReasoningParser)
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

// cacheMountPath is the one writable mount a container gets; the rest of its
// root filesystem is read-only.
const cacheMountPath = "/root/.cache"

// containerMounts is where a container for r must read each of its mount
// points from on this host right now: one per artifact role, plus the
// writable compilation cache. Keyed by mount point, which is what Docker
// reports back on an existing container.
func containerMounts(r recipe.Recipe, artifactPaths []string, cachePath string) map[string]string {
	mounts := make(map[string]string, len(artifactPaths)+1)
	for index, path := range artifactPaths {
		if index < len(r.Artifacts) {
			mounts[artifactMountPath(r.Artifacts[index].Role)] = path
		}
	}
	if cachePath != "" {
		mounts[cacheMountPath] = cachePath
	}
	return mounts
}

// mountMismatch is one mount point an existing container fills from the
// wrong place on this host.
type mountMismatch struct {
	MountPoint string
	Actual     string
	Expected   string
}

// staleMounts reports the mount points an existing container fills from a
// host directory this manager no longer uses. Docker records a container's
// binds once, at creation, and silently creates an empty directory for a
// bind source that has since disappeared instead of refusing to start — so
// a container created before the data directory moved comes back up with an
// empty /model, and the runtime dies inside its own argument validation
// complaining about a file it was never shown. A mount point the container
// does not report at all is not evidence of staleness and is skipped: only a
// source that positively disagrees counts.
func staleMounts(state ContainerState, expected map[string]string) []mountMismatch {
	stale := make([]mountMismatch, 0, len(expected))
	for mountPoint, want := range expected {
		actual, reported := state.Mounts[mountPoint]
		if !reported || filepath.Clean(actual) == filepath.Clean(want) {
			continue
		}
		stale = append(stale, mountMismatch{MountPoint: mountPoint, Actual: actual, Expected: want})
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].MountPoint < stale[j].MountPoint })
	return stale
}

// mountMismatchReceipt renders mismatches for a job receipt, so an operator
// reading the log can see exactly which directory moved.
func mountMismatchReceipt(stale []mountMismatch) []map[string]any {
	entries := make([]map[string]any, 0, len(stale))
	for _, mismatch := range stale {
		entries = append(entries, map[string]any{"mount_point": mismatch.MountPoint, "was": mismatch.Actual, "now": mismatch.Expected})
	}
	return entries
}

// runtimeFileReference is a file inside a mounted artifact that the generated
// command names by path. The runtime validates such a path itself and fails
// with a language-level traceback when it is absent, so every entry here has
// to be checked on disk before the container runs. This list and the argument
// builders above must stay in step.
type runtimeFileReference struct {
	Role string
	Name string
}

func runtimeFileReferences(r recipe.Recipe) []runtimeFileReference {
	var refs []runtimeFileReference
	switch r.Runtime.Kind {
	case "vllm":
		if r.Service.VLLM != nil && r.Service.VLLM.ChatTemplateFile != "" {
			refs = append(refs, runtimeFileReference{Role: "primary", Name: r.Service.VLLM.ChatTemplateFile})
		}
	case "sglang":
		if r.Service.SGLang != nil && r.Service.SGLang.ChatTemplateFile != "" {
			refs = append(refs, runtimeFileReference{Role: "primary", Name: r.Service.SGLang.ChatTemplateFile})
		}
	}
	return refs
}
