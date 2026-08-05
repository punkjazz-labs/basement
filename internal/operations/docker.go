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
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// pullProgressInterval is how often an image pull reports its aggregate
// progress while a single phase runs. It matches the pace weight downloads
// report at, so both bars in the console move the same way.
var pullProgressInterval = time.Second

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
	// Command is the argument list the container was created with. Docker
	// fixes it at creation too, and a distributed container carries the
	// fabric address of the other rank inside it, so this is the only way to
	// tell that a container is still pointed at an address that has changed.
	Command []string
	// Tmpfs is the in-memory filesystems the container was created with, keyed
	// by mount point, with the mount options Docker recorded for each. Fixed
	// at creation like the binds, and never reported in Mounts (which carries
	// bind sources), so it is the only way to tell that a container predates
	// its recipe declaring a writable path.
	Tmpfs map[string]string
	// Image is the image reference the container was created from, as Docker
	// recorded it in the container's configuration. Every container this
	// manager creates is created from a repo@sha256 reference, so for ours
	// this is the exact pinned image, and it is fixed for the container's
	// life: pulling a newer digest changes nothing about a container that
	// already exists.
	Image string
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
	// Docker emits an event per layer per chunk, which for a multi-gigabyte
	// runtime image is hundreds a second. Each reported receipt costs a disk
	// check and a database write, so they are paced the same way weight
	// downloads are; the phase changing is always worth reporting at once.
	var reported time.Time
	reportedStatus := ""
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
		if progress == nil {
			continue
		}
		if status == reportedStatus && time.Since(reported) < pullProgressInterval {
			continue
		}
		if err := progress(last); err != nil {
			return nil, fmt.Errorf("persist image progress: %w", err)
		}
		reported, reportedStatus = time.Now(), status
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
		Config struct {
			Labels map[string]string
			Cmd    []string
			Image  string
		}
		HostConfig struct {
			Tmpfs map[string]string
		}
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
	return ContainerState{ID: body.ID, Running: body.State.Running, Status: body.State.Status, Labels: body.Config.Labels, Mounts: mounts, Command: body.Config.Cmd, Tmpfs: body.HostConfig.Tmpfs, Image: body.Config.Image}, nil
}

// Create builds the container for r. writablePaths are extra read-write bind
// mounts keyed by container path — a media runtime's output and input
// directories, and nothing else today; every other kind passes none and gets
// exactly the filesystem it always had.
func (d *DockerClient) Create(ctx context.Context, name, image string, artifactPaths []string, cachePath string, writablePaths map[string]string, r recipe.Recipe, placement Placement) (string, error) {
	if len(artifactPaths) != len(r.Artifacts) || cachePath == "" {
		return "", errors.New("container artifact and cache paths are incomplete")
	}
	port := fmt.Sprintf("%d/tcp", r.Service.InternalPort)
	binds := make([]string, 0, len(artifactPaths)+1+len(writablePaths))
	for index, artifactPath := range artifactPaths {
		binds = append(binds, artifactPath+":"+artifactMountPath(r.Artifacts[index].Role)+":ro")
	}
	binds = append(binds, cachePath+":"+cacheMountPath+":rw")
	// Sorted so the same recipe on the same machine always produces the same
	// container definition; Docker records binds in the order they are given.
	for _, mountPoint := range sortedKeys(writablePaths) {
		binds = append(binds, writablePaths[mountPoint]+":"+mountPoint+":rw")
	}
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
		"Tmpfs":          containerTmpfs(r),
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
		// itself (see serveEndpointArgs) rather than relying on a port mapping.
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
		return []string{"python3", "-m", "sglang.launch_server"}, sglangArgs(r, placement), nil
	case "llamacpp":
		if r.Service.LlamaCpp == nil {
			return nil, nil, errors.New("recipe declares runtime kind llamacpp without a service.llamacpp block")
		}
		if placement.Distributed() {
			return nil, nil, errors.New("runtime kind llamacpp serves a model from a single Spark")
		}
		return []string{"/app/llama-server"}, llamaCppArgs(r, placement), nil
	case "comfyui":
		if r.Service.ComfyUI == nil {
			return nil, nil, errors.New("recipe declares runtime kind comfyui without a service.comfyui block")
		}
		if placement.Distributed() {
			return nil, nil, errors.New("runtime kind comfyui serves a model from a single Spark")
		}
		// The image's working directory is the ComfyUI checkout, so the
		// entrypoint is the server as its own project runs it.
		return []string{"python3", "main.py"}, comfyUIArgs(r, placement), nil
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
		if v.SpeculativeDraftSampleMethod != "" {
			specConfig["draft_sample_method"] = v.SpeculativeDraftSampleMethod
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
	args = appendOptional(args, "--tokenizer-mode", v.TokenizerMode)
	if v.MaxBatchedTokens > 0 {
		args = append(args, "--max-num-batched-tokens", fmt.Sprint(v.MaxBatchedTokens))
	}
	if v.BlockSize > 0 {
		args = append(args, "--block-size", fmt.Sprint(v.BlockSize))
	}
	if v.MaxCUDAGraphCaptureSize > 0 {
		args = append(args, "--max-cudagraph-capture-size", fmt.Sprint(v.MaxCUDAGraphCaptureSize))
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
	if v.FlashInferAutotune {
		args = append(args, "--enable-flashinfer-autotune")
	}
	if v.DisableQuantFusions {
		args = append(args, "--compilation-config", disabledQuantFusionsConfig)
	}
	return append(args, vllmDistributedArgs(placement)...)
}

// disabledQuantFusionsConfig is the whole compilation configuration a recipe
// can ask for, written out once as the literal document vLLM's own workaround
// for issue 50773 uses. It is a constant rather than a marshalled value
// because there is nothing here for a recipe to vary: the boolean either
// turns these two passes off or the flag is not sent at all.
const disabledQuantFusionsConfig = `{"pass_config":{"fuse_norm_quant":false,"fuse_act_quant":false}}`

// vllmDistributedArgs are the two-node launch flags the community DGX Spark
// recipe uses: plain vllm serve with --nnodes/--node-rank/--master-addr/
// --master-port, the multiprocessing executor, and --headless on the rank
// that serves no HTTP. There is no ray and no torchrun.
func vllmDistributedArgs(placement Placement) []string {
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

// RankBindsHostPort reports the host port this rank binds on the machine it
// runs on, and whether it binds one at all. Being handed a port is not the
// same as binding it: vLLM gives every rank --host and --port and then adds
// --headless to the worker, which serves nothing, so the host port on the
// worker Spark is none of that rank's business. SGLang has no headless mode,
// so every rank binds the port it is given, each on its own machine.
//
// This is the port-side companion of the argument builders above and has to
// stay in step with them. It is exported because the node endpoint that runs a
// worker's own preflight has to ask the same question the launch does: a rank
// that binds a port must find it free before the head stages a model for a
// process that cannot start, and a rank that binds none must not be failed on
// a port that never mattered. An unknown kind is treated as binding, so a
// runtime added without its headless story gets a check it may not need rather
// than silently skipping one it does.
func RankBindsHostPort(r recipe.Recipe, placement Placement) (int, bool) {
	if placement.Role == RoleWorker && r.Runtime.Kind == "vllm" {
		return 0, false
	}
	return r.Service.DefaultHostPort, true
}

// sglangArgs builds the sglang.launch_server command line. Every flag beyond
// the four positional essentials is omitted when the recipe leaves it unset,
// so the runtime's own default applies and the manager never invents a value.
func sglangArgs(r recipe.Recipe, placement Placement) []string {
	if r.Service.SGLang == nil {
		return nil
	}
	s := *r.Service.SGLang
	serveHost, servePort := serveEndpointArgs(r, placement)
	// --enable-metrics is not a recipe choice: SGLang mounts /metrics only
	// behind this flag, and the console's telemetry tiles read that endpoint.
	args := []string{
		"--model-path", artifactMountPath("primary"),
		"--host", serveHost,
		"--port", fmt.Sprint(servePort),
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
	return append(args, sglangDistributedArgs(placement)...)
}

// sglangDistributedArgs are SGLang's own multi-node launch flags. The shape is
// the same idea as vLLM's and the values come from the same resolved
// placement, but the flags are not: SGLang counts nodes with --nnodes, takes
// this rank with --node-rank, and takes the rendezvous as a single
// --dist-init-addr host:port rather than as a separate address and port. Every
// rank runs sglang.launch_server with the same arguments except its rank;
// there is no headless flag, because rank 0 is the only one that serves model
// traffic and the job only ever health-checks and inference-tests the head.
// That also means every rank is handed the same host and port, and under host
// networking each one binds it on its own machine's loopback rather than on a
// port they would have to share.
//
// --tp-size is not repeated here: tensor parallelism spans the whole topology,
// so the recipe's tensor_parallel_size (validated to equal spark_count) is
// already the global size every rank is launched with.
func sglangDistributedArgs(placement Placement) []string {
	if !placement.Distributed() {
		return nil
	}
	return []string{
		"--nnodes", fmt.Sprint(placement.NodeCount),
		"--node-rank", fmt.Sprint(placement.Rank()),
		"--dist-init-addr", net.JoinHostPort(placement.MasterAddress, fmt.Sprint(placement.MasterPort)),
	}
}

// llamaCppArgs builds the llama-server command line. It differs from the
// other two builders in one structural way: llama-server is pointed at a
// single GGUF file rather than at a repository snapshot directory, so the
// model argument joins the recipe's pinned file name to the primary mount.
// A split quantization names its first shard and llama-server opens the rest
// beside it, which is why only one name is ever passed.
//
// There are no distributed arguments here and no headless mode. llama.cpp can
// spread a model across machines through its RPC backend, but that is a
// separate server process on every node and a different launch shape
// entirely; until that is built and qualified, the validator refuses a
// multi-Spark llama.cpp recipe and this builder never sees one.
func llamaCppArgs(r recipe.Recipe, placement Placement) []string {
	if r.Service.LlamaCpp == nil {
		return nil
	}
	l := *r.Service.LlamaCpp
	serveHost, servePort := serveEndpointArgs(r, placement)
	// --metrics is not a recipe choice, for the same reason SGLang's
	// --enable-metrics is not: llama-server mounts /metrics only behind this
	// flag, and the console's telemetry tiles read that endpoint. --alias is
	// the name llama-server reports to /v1/models, so it is what the proxy
	// and the inference test address the model by.
	args := []string{
		"--model", artifactMountPath("primary") + "/" + l.ModelFile,
		"--host", serveHost,
		"--port", fmt.Sprint(servePort),
		"--alias", r.Service.ServedModelID,
		"--metrics",
	}
	if l.ContextSize > 0 {
		args = append(args, "--ctx-size", fmt.Sprint(l.ContextSize))
	}
	if l.GPULayers > 0 {
		args = append(args, "--n-gpu-layers", fmt.Sprint(l.GPULayers))
	}
	if l.Parallel > 0 {
		args = append(args, "--parallel", fmt.Sprint(l.Parallel))
	}
	args = appendOptional(args, "--flash-attn", l.FlashAttention)
	if l.Jinja {
		args = append(args, "--jinja")
	}
	if l.ChatTemplateFile != "" {
		args = append(args, "--chat-template-file", artifactMountPath("primary")+"/"+l.ChatTemplateFile)
	}
	return args
}

func appendOptional(args []string, flag, value string) []string {
	if value == "" {
		return args
	}
	return append(args, flag, value)
}

func artifactMountPath(role string) string { return recipe.ArtifactMountPath(role) }

// cacheMountPath is the one writable bind mount a container gets; the rest of
// its root filesystem is read-only. Declared with the schema, because the
// validator has to keep recipes off it.
const cacheMountPath = recipe.CacheMountPath

// Mount options for the two kinds of in-memory filesystem a container gets.
// They differ in one deliberate way: a JIT compiler writes a shared object
// into its cache directory and then loads it, so noexec on a writable path
// would replace one import-time failure with another. exec must be stated,
// not merely implied: Docker mounts a tmpfs noexec unless told otherwise,
// which real hardware proved by refusing to map the compiled kernel from a
// mount that only omitted the flag. nosuid stays on both.
const (
	tempTmpfsOptions     = "rw,noexec,nosuid,size=8g"
	writableTmpfsOptions = "rw,exec,nosuid,size=4g"
)

// containerTmpfs is the complete set of in-memory filesystems a container for
// r is created with, keyed the way Docker's HostConfig.Tmpfs is: the scratch
// mount every container has always had, plus one private mount per writable
// path the recipe declares. A recipe cannot reach the scratch mount or shadow
// a bind through this map — the validator refuses those paths — so a recipe
// declaring nothing produces exactly the filesystem that shipped before.
func containerTmpfs(r recipe.Recipe) map[string]string {
	mounts := map[string]string{recipe.TempMountPath: tempTmpfsOptions}
	for _, writable := range r.Runtime.WritablePaths {
		mounts[writable] = writableTmpfsOptions
	}
	return mounts
}

// containerMounts is where a container for r must read each of its mount
// points from on this host right now: one per artifact role, the writable
// compilation cache, and any extra read-write directory the runtime needs
// (a media runtime's output and input directories). Keyed by mount point,
// which is what Docker reports back on an existing container.
func containerMounts(r recipe.Recipe, artifactPaths []string, cachePath string, writablePaths map[string]string) map[string]string {
	mounts := make(map[string]string, len(artifactPaths)+1+len(writablePaths))
	for index, path := range artifactPaths {
		if index < len(r.Artifacts) {
			mounts[artifactMountPath(r.Artifacts[index].Role)] = path
		}
	}
	if cachePath != "" {
		mounts[cacheMountPath] = cachePath
	}
	for mountPoint, host := range writablePaths {
		mounts[mountPoint] = host
	}
	return mounts
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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

// staleTmpfs reports the in-memory mounts an existing container was created
// with that no longer describe what this recipe asks for. Docker fixes a
// container's tmpfs mounts at creation exactly as it fixes its binds, so a
// container built before its recipe declared a writable path has no such mount
// and never will: the runtime imports, finds the path read-only, and the
// worker dies after the weights have loaded. A path the recipe has since
// dropped counts too, so a container's writable surface never outlives the
// reason it was granted.
//
// A container reporting no tmpfs at all is not evidence of anything — a daemon
// that does not report host configuration is silence, not disagreement — and
// no container is rebuilt on silence.
func staleTmpfs(state ContainerState, expected map[string]string) []mountMismatch {
	if len(state.Tmpfs) == 0 {
		return nil
	}
	drift := make([]mountMismatch, 0, len(expected))
	for mountPoint, want := range expected {
		if actual, mounted := state.Tmpfs[mountPoint]; !mounted || actual != want {
			drift = append(drift, mountMismatch{MountPoint: mountPoint, Actual: actual, Expected: want})
		}
	}
	for mountPoint, actual := range state.Tmpfs {
		if _, wanted := expected[mountPoint]; !wanted {
			drift = append(drift, mountMismatch{MountPoint: mountPoint, Actual: actual})
		}
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].MountPoint < drift[j].MountPoint })
	return drift
}

// imageMismatch is the pinned image an existing container was built from,
// when the recipe now pins a different one.
type imageMismatch struct {
	Actual   string
	Expected string
}

// staleImage reports that an existing container is still running an image the
// recipe has moved off. A recipe can change its digest without changing its
// version — the two-Spark Flash recipe moved from vLLM v0.25.1 to v0.26.0 that
// way, because bumping the version would rename the container and orphan the
// running one — and Docker fixes a container's image at creation exactly as it
// fixes its binds. pull_image fetches the new digest, create_container finds a
// container of the right name and version and reuses it, and the machine goes
// on serving the old image with nothing in the receipt to say so. Only the
// digest the container was actually built from can tell.
//
// An image the container does not report is silence, not disagreement, and no
// container is rebuilt on silence.
func staleImage(state ContainerState, r recipe.Recipe) *imageMismatch {
	expected := r.Runtime.Reference()
	if state.Image == "" || expected == "" || state.Image == expected {
		return nil
	}
	return &imageMismatch{Actual: state.Image, Expected: expected}
}

// imageMismatchReceipt renders an image change for a job receipt, so the log
// names both digests rather than saying only that something was rebuilt.
func imageMismatchReceipt(drift imageMismatch) map[string]any {
	return map[string]any{"was": drift.Actual, "now": drift.Expected}
}

// commandMismatch is the first argument at which an existing container's
// command differs from the command this recipe and placement produce now.
// Flag names are retained separately so a receipt can name the recipe knob
// that changed instead of reducing the repair to a bare stale-container event.
type commandMismatch struct {
	Position int
	Flag     string
	Actual   string
	Expected string
}

// staleCommand reports an existing container whose serve command no longer
// matches the recipe. Docker fixes Config.Cmd when the container is created,
// so changing a serve setting under the same recipe version does not affect a
// container that is merely started again. Comparison is deliberately exact:
// the command builders produce one deterministic order and this package has
// never promised that reordered flags or differently formatted values are
// equivalent.
//
// The distributed rendezvous arguments are removed from this generic pass and
// remain the responsibility of staleLaunch. For vLLM those are --node-rank,
// --master-addr and --master-port; for SGLang they are --node-rank and
// --dist-init-addr. Their values come from placement rather than the recipe,
// and staleLaunch knows which values were resolved strongly enough to compare.
// Keeping that one authority means an unresolved live address is silence while
// a resolved address change still rebuilds the container.
func staleCommand(state ContainerState, r recipe.Recipe, placement Placement) []commandMismatch {
	if len(state.Command) == 0 {
		return nil
	}
	_, expected, err := runtimeCommand(r, placement)
	if err != nil || len(expected) == 0 {
		return nil
	}
	actual := commandWithoutLaunchFlags(state.Command, r)
	expected = commandWithoutLaunchFlags(expected, r)
	limit := len(actual)
	if len(expected) < limit {
		limit = len(expected)
	}
	for position := 0; position < limit; position++ {
		if actual[position] != expected[position] {
			return []commandMismatch{describeCommandMismatch(actual, expected, position)}
		}
	}
	if len(actual) == len(expected) {
		return nil
	}
	return []commandMismatch{describeCommandMismatch(actual, expected, limit)}
}

func commandWithoutLaunchFlags(command []string, r recipe.Recipe) []string {
	excluded := launchFlagNames(r)
	if len(excluded) == 0 {
		return command
	}
	comparable := make([]string, 0, len(command))
	for position := 0; position < len(command); position++ {
		argument := command[position]
		if !excluded[argument] {
			comparable = append(comparable, argument)
			continue
		}
		// Every rendezvous argument has one value. A truncated Config.Cmd is
		// still diagnosed by staleLaunch when the resolved value is present;
		// when it is not, there is no live value safe to compare against.
		if position+1 < len(command) {
			position++
		}
	}
	return comparable
}

func describeCommandMismatch(actual, expected []string, position int) commandMismatch {
	drift := commandMismatch{Position: position, Flag: fmt.Sprintf("argv[%d]", position)}
	if position < len(actual) {
		drift.Actual = actual[position]
	}
	if position < len(expected) {
		drift.Expected = expected[position]
	}
	if position > 0 && position < len(actual) && position < len(expected) &&
		actual[position-1] == expected[position-1] && strings.HasPrefix(expected[position-1], "--") {
		drift.Flag = expected[position-1]
		return drift
	}
	if position < len(expected) && strings.HasPrefix(expected[position], "--") {
		drift.Flag = expected[position]
	} else if position < len(actual) && strings.HasPrefix(actual[position], "--") {
		drift.Flag = actual[position]
	}
	return drift
}

// commandMismatchReceipt renders command drift in the same flag, was and now
// shape as rendezvous drift, with the argv position retained for positional or
// ordering changes.
func commandMismatchReceipt(drift []commandMismatch) []map[string]any {
	entries := make([]map[string]any, 0, len(drift))
	for _, mismatch := range drift {
		entries = append(entries, map[string]any{
			"flag": mismatch.Flag, "position": mismatch.Position,
			"was": mismatch.Actual, "now": mismatch.Expected,
		})
	}
	return entries
}

// launchMismatch is a launch flag whose value was fixed into an existing
// container and no longer matches what this job resolved.
type launchMismatch struct {
	Flag     string
	Actual   string
	Expected string
}

// staleLaunch reports the distributed launch flags an existing container
// still carries from an earlier deployment. The fabric address is the reason
// this exists: both ranks meet at the head's address on the cabled port, that
// address is auto-assigned by the kernel, and nothing guarantees the kernel
// hands out the same one after a reboot. The address is re-resolved live for
// every job, but a container created by an earlier job keeps the old one
// baked into its command line, and a start job creates no container - so
// without this the ranks would be told to meet at an address that no longer
// exists and would simply hang. A flag the container does not carry at all is
// not evidence of staleness and is skipped: only a value that positively
// disagrees counts.
func staleLaunch(state ContainerState, r recipe.Recipe, placement Placement) []launchMismatch {
	expected := launchFlags(r, placement)
	if len(expected) == 0 {
		return nil
	}
	drift := make([]launchMismatch, 0, len(expected))
	for flag, want := range expected {
		actual, carried := commandFlag(state.Command, flag)
		if !carried || actual == want {
			continue
		}
		drift = append(drift, launchMismatch{Flag: flag, Actual: actual, Expected: want})
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Flag < drift[j].Flag })
	return drift
}

// launchFlags are the distributed launch flags this job resolved, named the
// way the recipe's runtime names them, so drift is measured against what the
// container was actually given rather than against a shape borrowed from
// another runtime. It is the one place the two multi-node vocabularies are
// written down: a new runtime kind gets its case here at the same time as its
// argument builder above, or its containers survive a reboot holding an
// address that no longer exists.
//
// An unresolved address or port is not a disagreement to act on: it means this
// call knows nothing about where the ranks meet, and a container is never
// rebuilt on the strength of what the caller failed to say. vLLM carries the
// two halves as separate flags and each is compared on its own; SGLang carries
// them as one value, which is only comparable when both halves are known.
func launchFlags(r recipe.Recipe, placement Placement) map[string]string {
	if !placement.Distributed() {
		return nil
	}
	flags := map[string]string{"--node-rank": fmt.Sprint(placement.Rank())}
	switch r.Runtime.Kind {
	case "vllm":
		if placement.MasterAddress != "" {
			flags["--master-addr"] = placement.MasterAddress
		}
		if placement.MasterPort > 0 {
			flags["--master-port"] = fmt.Sprint(placement.MasterPort)
		}
	case "sglang":
		if placement.MasterAddress != "" && placement.MasterPort > 0 {
			flags["--dist-init-addr"] = net.JoinHostPort(placement.MasterAddress, fmt.Sprint(placement.MasterPort))
		}
	}
	return flags
}

// launchFlagNames is the complete set of placement-resolved arguments for a
// runtime, including values launchFlags may deliberately omit when placement
// is unresolved. staleCommand needs the complete names so an absent live
// address cannot leak back into generic argv comparison and cause a rebuild
// that staleLaunch correctly refused to infer.
func launchFlagNames(r recipe.Recipe) map[string]bool {
	if !r.Distributed() {
		return nil
	}
	flags := map[string]bool{"--node-rank": true}
	switch r.Runtime.Kind {
	case "vllm":
		flags["--master-addr"] = true
		flags["--master-port"] = true
	case "sglang":
		flags["--dist-init-addr"] = true
	}
	return flags
}

func commandFlag(command []string, flag string) (string, bool) {
	for index, argument := range command {
		if argument == flag && index+1 < len(command) {
			return command[index+1], true
		}
	}
	return "", false
}

// launchMismatchReceipt renders launch drift for a job receipt, so the log
// says which address the container was holding and which one it now uses.
func launchMismatchReceipt(drift []launchMismatch) []map[string]any {
	entries := make([]map[string]any, 0, len(drift))
	for _, mismatch := range drift {
		entries = append(entries, map[string]any{"flag": mismatch.Flag, "was": mismatch.Actual, "now": mismatch.Expected})
	}
	return entries
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
	case "llamacpp":
		if r.Service.LlamaCpp == nil {
			break
		}
		// The weights themselves are a named file here, not a directory the
		// runtime scans, so a missing or misnamed shard is exactly the kind
		// of mistake this check exists to catch before the container runs.
		refs = append(refs, runtimeFileReference{Role: "primary", Name: r.Service.LlamaCpp.ModelFile})
		if r.Service.LlamaCpp.ChatTemplateFile != "" {
			refs = append(refs, runtimeFileReference{Role: "primary", Name: r.Service.LlamaCpp.ChatTemplateFile})
		}
	}
	return refs
}
