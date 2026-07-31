package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const DefaultListen = "127.0.0.1:7070"

type Config struct {
	Listen  string
	DataDir string
	Version string
}

func Parse(version string) (Config, error) {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", DefaultListen, "HTTP listen address; choose a LAN/Tailscale address deliberately")
	flag.StringVar(&cfg.DataDir, "data-dir", defaultDataDir(), "persistent manager data directory")
	flag.Parse()
	cfg.Version = version
	if strings.TrimSpace(cfg.Listen) == "" {
		return Config{}, errors.New("listen address cannot be empty")
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data directory: %w", err)
	}
	cfg.DataDir = filepath.Clean(abs)
	return cfg, nil
}

// ModelPublishHost is the interface model endpoints are published on. It
// follows the manager's own listen interface so LAN exposure of the
// unauthenticated model port stays the same deliberate choice as exposing the
// manager itself; anything unparseable falls back to loopback.
func (c Config) ModelPublishHost() string {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil {
		return "127.0.0.1"
	}
	return host
}

func defaultDataDir() string {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		return "/var/lib/runonspark-manager"
	}
	if state := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
		return filepath.Join(state, "runonspark-manager")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./var"
	}
	return filepath.Join(home, ".local", "state", "runonspark-manager")
}
