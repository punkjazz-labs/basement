package setup

import (
	"context"
	"strings"
)

// Identity is what the target machine reports about itself over SSH (or
// locally). GB10 detection is authoritative here — hostname patterns from
// discovery are only hints, because OEM machines (ASUS Ascent GX10, MSI
// EdgeXpert, Gigabyte AI TOP Atom, Acer Veriton GN100, Dell, Lenovo…) ship
// the same GB10 superchip under vendor default hostnames.
type Identity struct {
	Hostname    string
	GPUName     string // nvidia-smi product name, e.g. "NVIDIA GB10"
	DeviceModel string // /proc/device-tree/model, e.g. "NVIDIA DGX Spark"
	OSName      string // /etc/os-release PRETTY_NAME
	MemoryBytes int64
}

// IsGB10 reports whether the machine carries a GB10 superchip.
func (id Identity) IsGB10() bool {
	return strings.Contains(strings.ToUpper(id.GPUName), "GB10") ||
		strings.Contains(strings.ToUpper(id.DeviceModel), "GB10") ||
		strings.Contains(strings.ToUpper(id.DeviceModel), "DGX SPARK")
}

// Product renders the friendliest product name available.
func (id Identity) Product() string {
	if model := strings.TrimSpace(id.DeviceModel); model != "" {
		return model
	}
	if gpu := strings.TrimSpace(id.GPUName); gpu != "" {
		return gpu
	}
	return "unknown hardware"
}

// Probe collects the identity of the machine behind runner. Each field is
// best-effort: a missing nvidia-smi or device tree yields an empty value, and
// classification copes.
func Probe(ctx context.Context, runner Runner) Identity {
	read := func(command string) string {
		out, err := runner.Run(ctx, command, nil)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	identity := Identity{
		Hostname:    read("hostname -s 2>/dev/null || hostname"),
		GPUName:     firstLine(read("nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null")),
		DeviceModel: strings.Trim(read("cat /proc/device-tree/model 2>/dev/null"), "\x00"),
		OSName:      read(". /etc/os-release 2>/dev/null && printf %s \"$PRETTY_NAME\""),
	}
	if memory := read("awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo 2>/dev/null"); memory != "" {
		identity.MemoryBytes = parseInt64(memory)
	}
	return identity
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return strings.TrimSpace(line)
}

func parseInt64(value string) int64 {
	var parsed int64
	for _, char := range strings.TrimSpace(value) {
		if char < '0' || char > '9' {
			return 0
		}
		parsed = parsed*10 + int64(char-'0')
	}
	return parsed
}
