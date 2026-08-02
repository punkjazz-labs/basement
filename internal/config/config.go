package config

import (
	"errors"
	"flag"
	"fmt"
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
	// Command is an optional subcommand ("pairing-url") instead of serving.
	Command string
}

func Parse(version string) (Config, error) {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", DefaultListen, "HTTP listen address; choose a LAN/Tailscale address deliberately")
	flag.StringVar(&cfg.DataDir, "data-dir", defaultDataDir(), "persistent manager data directory")
	flag.Parse()
	cfg.Version = version
	cfg.Command = flag.Arg(0)
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

func defaultDataDir() string {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		return "/var/lib/basement"
	}
	if state := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
		return filepath.Join(state, "basement")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./var"
	}
	return filepath.Join(home, ".local", "state", "basement")
}
