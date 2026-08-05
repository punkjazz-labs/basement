package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const DefaultListen = "127.0.0.1:7070"

type Config struct {
	Listen      string
	FleetListen string
	DataDir     string
	Version     string
	// Command is an optional subcommand ("pairing-url") instead of serving.
	Command string
}

func Parse(version string) (Config, error) {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", DefaultListen, "HTTP listen address; choose a LAN/Tailscale address deliberately")
	flag.StringVar(&cfg.FleetListen, "fleet-listen", "", "dedicated mutual TLS listen address; defaults to the console address on the next port")
	flag.StringVar(&cfg.DataDir, "data-dir", defaultDataDir(), "persistent manager data directory")
	flag.Parse()
	cfg.Version = version
	cfg.Command = flag.Arg(0)
	if strings.TrimSpace(cfg.Listen) == "" {
		return Config{}, errors.New("listen address cannot be empty")
	}
	if strings.TrimSpace(cfg.FleetListen) == "" {
		fleetListen, err := adjacentListenAddress(cfg.Listen)
		if err != nil {
			return Config{}, fmt.Errorf("derive fleet listen address: %w", err)
		}
		cfg.FleetListen = fleetListen
	} else if _, _, err := net.SplitHostPort(cfg.FleetListen); err != nil {
		return Config{}, fmt.Errorf("invalid fleet listen address: %w", err)
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data directory: %w", err)
	}
	cfg.DataDir = filepath.Clean(abs)
	return cfg, nil
}

func adjacentListenAddress(address string) (string, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return "", err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port >= 65535 {
		return "", errors.New("console listen port must leave room for the fleet listener")
	}
	return net.JoinHostPort(host, strconv.Itoa(port+1)), nil
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
