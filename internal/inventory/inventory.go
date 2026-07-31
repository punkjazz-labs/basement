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
	"runtime"
	"strconv"
	"strings"
	"time"
)

type System struct {
	Hostname               string   `json:"hostname"`
	Architecture           string   `json:"architecture"`
	OS                     string   `json:"os"`
	Kernel                 string   `json:"kernel"`
	ProductName            string   `json:"product_name"`
	DGXSpark               bool     `json:"dgx_spark"`
	DockerReady            bool     `json:"docker_ready"`
	DockerVersion          string   `json:"docker_version,omitempty"`
	NvidiaRuntimeReady     bool     `json:"nvidia_runtime_ready"`
	GPUVisible             bool     `json:"gpu_visible"`
	GPUDescription         string   `json:"gpu_description,omitempty"`
	DataDirectory          string   `json:"data_directory"`
	DataDirectoryWritable  bool     `json:"data_directory_writable"`
	StorageAvailable       int64    `json:"storage_available_bytes"`
	StorageTotal           int64    `json:"storage_total_bytes"`
	DockerDataDirectory    string   `json:"docker_data_directory,omitempty"`
	DockerStorageAvailable int64    `json:"docker_storage_available_bytes"`
	DockerStorageTotal     int64    `json:"docker_storage_total_bytes"`
	DockerSharesDataDisk   bool     `json:"docker_shares_data_disk"`
	MemoryAvailable        int64    `json:"memory_available_bytes"`
	MemoryTotal            int64    `json:"memory_total_bytes"`
	GPUMemoryFree          int64    `json:"gpu_memory_free_bytes"`
	GPUMemoryTotal         int64    `json:"gpu_memory_total_bytes"`
	GPUPowerDrawWatts      float64  `json:"gpu_power_draw_watts,omitempty"`
	GPUClockMHz            int64    `json:"gpu_clock_mhz,omitempty"`
	GPUTemperatureC        int64    `json:"gpu_temperature_c,omitempty"`
	Ready                  bool     `json:"ready"`
	Blocking               []string `json:"blocking_conditions"`
	ObservedAt             string   `json:"observed_at"`
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
	gpu := commandOutput(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	s.GPUVisible = gpu != ""
	s.GPUDescription = gpu
	// GB10-class identity: DGX Spark and its OEM equivalents (ASUS Ascent
	// GX10, MSI EdgeXpert, …) carry vendor DMI strings, so the GPU name and
	// the device tree are checked too — nvidia-smi naming the GB10 chip is
	// the authoritative signal.
	identity := strings.ToLower(strings.Join([]string{
		s.ProductName, gpu, readFirst("/proc/device-tree/model"),
	}, "\n"))
	s.DGXSpark = strings.Contains(identity, "dgx spark") || strings.Contains(identity, "gb10")
	s.DataDirectoryWritable = directoryWritable(h.DataDir)
	s.StorageAvailable, s.StorageTotal = diskSpace(h.DataDir)
	s.MemoryAvailable, s.MemoryTotal = memorySpace()
	docker, err := inspectDocker(ctx, h.socket())
	if err == nil {
		s.DockerReady, s.DockerVersion = true, docker.ServerVersion
		s.NvidiaRuntimeReady = docker.HasNvidiaRuntime
		s.DockerDataDirectory = docker.RootDirectory
		s.DockerStorageAvailable, s.DockerStorageTotal = diskSpace(docker.RootDirectory)
		s.DockerSharesDataDisk = sameFilesystem(h.DataDir, docker.RootDirectory)
	}
	s.GPUMemoryFree, s.GPUMemoryTotal, s.GPUPowerDrawWatts, s.GPUClockMHz, s.GPUTemperatureC = gpuTelemetry(ctx)
	// GB10 machines have unified memory: nvidia-smi reports memory.total as
	// [N/A], so system memory is the real GPU-visible capacity.
	if s.DGXSpark && s.GPUMemoryTotal <= 0 && s.MemoryTotal > 0 {
		s.GPUMemoryFree, s.GPUMemoryTotal = s.MemoryAvailable, s.MemoryTotal
	}
	if s.Architecture != "aarch64" {
		s.Blocking = append(s.Blocking, "Linux ARM64 architecture is required")
	}
	if !s.DGXSpark {
		s.Blocking = append(s.Blocking, "GB10 hardware identity was not detected (DGX Spark or an OEM GB10 machine)")
	}
	if !s.DockerReady {
		s.Blocking = append(s.Blocking, "Docker daemon is not reachable")
	}
	if !s.NvidiaRuntimeReady {
		s.Blocking = append(s.Blocking, "NVIDIA Container Runtime is not registered with Docker — run: sudo nvidia-ctk runtime configure --runtime=docker && sudo systemctl restart docker")
	}
	if !s.GPUVisible {
		s.Blocking = append(s.Blocking, "GPU is not visible through nvidia-smi")
	}
	if s.MemoryAvailable <= 0 || s.MemoryTotal <= 0 {
		s.Blocking = append(s.Blocking, "system memory capacity is unavailable")
	}
	if s.GPUMemoryFree <= 0 || s.GPUMemoryTotal <= 0 {
		s.Blocking = append(s.Blocking, "GPU memory capacity is unavailable")
	}
	if !s.DataDirectoryWritable {
		s.Blocking = append(s.Blocking, "manager data directory is not writable")
	}
	if s.DockerReady && (s.DockerStorageAvailable <= 0 || s.DockerStorageTotal <= 0) {
		s.Blocking = append(s.Blocking, "Docker storage capacity is unavailable")
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
	RootDirectory    string
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
		Runtimes      map[string]json.RawMessage `json:"Runtimes"`
		DockerRootDir string                     `json:"DockerRootDir"`
	}
	if err := dockerJSON(ctx, client, "/info", &info); err != nil {
		return dockerInfo{}, err
	}
	if strings.TrimSpace(info.DockerRootDir) == "" {
		return dockerInfo{}, errors.New("docker data root is unavailable")
	}
	_, hasNvidia := info.Runtimes["nvidia"]
	return dockerInfo{ServerVersion: version.Version, HasNvidiaRuntime: hasNvidia, RootDirectory: info.DockerRootDir}, nil
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

func sameFilesystem(first, second string) bool {
	firstDevice, firstOK := filesystemDevice(first)
	secondDevice, secondOK := filesystemDevice(second)
	return firstOK && secondOK && firstDevice == secondDevice
}

func memorySpace() (available int64, total int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemAvailable":
			available = value * 1024
		case "MemTotal":
			total = value * 1024
		}
	}
	return available, total
}

// gpuTelemetry samples memory plus device health in one nvidia-smi call.
// Memory sums across GPUs; power sums; clock and temperature report the
// hottest/fastest device. Fields a driver does not expose parse as zero.
func gpuTelemetry(ctx context.Context) (free, total int64, powerWatts float64, clockMHz, temperatureC int64) {
	output := commandOutput(ctx, "nvidia-smi", "--query-gpu=memory.free,memory.total,power.draw,clocks.sm,temperature.gpu", "--format=csv,noheader,nounits")
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		freeMiB, freeErr := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
		totalMiB, totalErr := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if freeErr == nil && totalErr == nil {
			free += freeMiB * 1024 * 1024
			total += totalMiB * 1024 * 1024
		}
		if len(fields) >= 3 {
			if watts, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64); err == nil {
				powerWatts += watts
			}
		}
		if len(fields) >= 4 {
			if clock, err := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64); err == nil && clock > clockMHz {
				clockMHz = clock
			}
		}
		if len(fields) >= 5 {
			if temperature, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64); err == nil && temperature > temperatureC {
				temperatureC = temperature
			}
		}
	}
	return free, total, powerWatts, clockMHz, temperatureC
}

func CheckPort(port int) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("port %d is occupied: %w", port, err)
	}
	return errors.Join(listener.Close())
}
