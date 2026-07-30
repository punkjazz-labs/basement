package inventory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type System struct {
	Hostname              string   `json:"hostname"`
	Architecture          string   `json:"architecture"`
	OS                    string   `json:"os"`
	Kernel                string   `json:"kernel"`
	ProductName           string   `json:"product_name"`
	DGXSpark              bool     `json:"dgx_spark"`
	DockerReady           bool     `json:"docker_ready"`
	DockerVersion         string   `json:"docker_version,omitempty"`
	NvidiaRuntimeReady    bool     `json:"nvidia_runtime_ready"`
	GPUVisible            bool     `json:"gpu_visible"`
	GPUDescription        string   `json:"gpu_description,omitempty"`
	DataDirectory         string   `json:"data_directory"`
	DataDirectoryWritable bool     `json:"data_directory_writable"`
	StorageAvailable      int64    `json:"storage_available_bytes"`
	StorageTotal          int64    `json:"storage_total_bytes"`
	Ready                 bool     `json:"ready"`
	Blocking              []string `json:"blocking_conditions"`
	ObservedAt            string   `json:"observed_at"`
}

type Provider interface {
	Inspect(context.Context) (System, error)
}

type Host struct {
	DataDir      string
	DockerSocket string
}

func (h Host) Inspect(ctx context.Context) (System, error) {
	hostname, _ := os.Hostname()
	s := System{
		Hostname: hostname, Architecture: normalizeArch(runtime.GOARCH), OS: readOSRelease(),
		Kernel: commandOutput(ctx, "uname", "-r"), ProductName: readFirst("/sys/devices/virtual/dmi/id/product_name", "/sys/class/dmi/id/product_name"),
		DataDirectory: h.DataDir, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	product := strings.ToLower(s.ProductName)
	s.DGXSpark = strings.Contains(product, "dgx spark") || strings.Contains(product, "gb10")
	s.DataDirectoryWritable = directoryWritable(h.DataDir)
	s.StorageAvailable, s.StorageTotal = diskSpace(h.DataDir)
	docker, err := inspectDocker(ctx, h.socket())
	if err == nil {
		s.DockerReady, s.DockerVersion = true, docker.ServerVersion
		s.NvidiaRuntimeReady = docker.HasNvidiaRuntime
	}
	gpu := commandOutput(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	s.GPUVisible = gpu != ""
	s.GPUDescription = gpu
	if s.Architecture != "aarch64" {
		s.Blocking = append(s.Blocking, "Linux ARM64 architecture is required")
	}
	if !s.DGXSpark {
		s.Blocking = append(s.Blocking, "DGX Spark hardware identity was not detected")
	}
	if !s.DockerReady {
		s.Blocking = append(s.Blocking, "Docker daemon is not reachable")
	}
	if !s.NvidiaRuntimeReady {
		s.Blocking = append(s.Blocking, "NVIDIA Container Runtime is not registered with Docker")
	}
	if !s.GPUVisible {
		s.Blocking = append(s.Blocking, "GPU is not visible through nvidia-smi")
	}
	if !s.DataDirectoryWritable {
		s.Blocking = append(s.Blocking, "manager data directory is not writable")
	}
	s.Ready = len(s.Blocking) == 0
	return s, nil
}

func (h Host) socket() string {
	if h.DockerSocket != "" {
		return h.DockerSocket
	}
	return "/var/run/docker.sock"
}

type dockerInfo struct {
	ServerVersion    string
	HasNvidiaRuntime bool
}

func inspectDocker(ctx context.Context, socket string) (dockerInfo, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	var version struct {
		Version string `json:"Version"`
	}
	if err := dockerJSON(ctx, client, "/version", &version); err != nil {
		return dockerInfo{}, err
	}
	var info struct {
		Runtimes map[string]json.RawMessage `json:"Runtimes"`
	}
	if err := dockerJSON(ctx, client, "/info", &info); err != nil {
		return dockerInfo{}, err
	}
	_, hasNvidia := info.Runtimes["nvidia"]
	return dockerInfo{ServerVersion: version.Version, HasNvidiaRuntime: hasNvidia}, nil
}

func dockerJSON(ctx context.Context, client *http.Client, path string, target any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func normalizeArch(arch string) string {
	if arch == "arm64" {
		return "aarch64"
	}
	return arch
}

func readOSRelease() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.Trim(value, `"`)
		}
	}
	if values["PRETTY_NAME"] != "" {
		return values["PRETTY_NAME"]
	}
	return runtime.GOOS
}

func readFirst(paths ...string) string {
	for _, path := range paths {
		if data, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func commandOutput(ctx context.Context, name string, args ...string) string {
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := exec.CommandContext(commandCtx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func directoryWritable(path string) bool {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return false
	}
	f, err := os.CreateTemp(path, ".write-check-")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func diskSpace(path string) (int64, int64) {
	var stat syscall.Statfs_t
	probe := path
	for {
		if err := syscall.Statfs(probe, &stat); err == nil {
			return int64(stat.Bavail) * int64(stat.Bsize), int64(stat.Blocks) * int64(stat.Bsize)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return 0, 0
		}
		probe = parent
	}
}

func CheckPort(port int) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("port %d is occupied: %w", port, err)
	}
	return errors.Join(listener.Close())
}
