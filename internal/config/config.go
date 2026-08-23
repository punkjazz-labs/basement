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
	// Listen holds every console address the manager binds, in the order
	// -listen named them. One address is the ordinary case. The first
	// address is the primary: the fleet listener and every URL the manager
	// reports for itself derive from it, so a machine keeps one identity
	// even when it answers on several addresses.
	Listen      []string
	FleetListen string
	DataDir     string
	Version     string
	// Command is an optional subcommand ("pairing-url") instead of serving.
	Command string
}

// PrimaryListen is the first console address, or "" when there is none.
func (c Config) PrimaryListen() string {
	if len(c.Listen) == 0 {
		return ""
	}
	return c.Listen[0]
}

func Parse(version string) (Config, error) {
	var cfg Config
	var listen string
	flag.StringVar(&listen, "listen", DefaultListen, "HTTP listen address, or several separated by commas; choose LAN/Tailscale addresses deliberately")
	flag.StringVar(&cfg.FleetListen, "fleet-listen", "", "dedicated mutual TLS listen address; defaults to the console address on the next port")
	flag.StringVar(&cfg.DataDir, "data-dir", defaultDataDir(), "persistent manager data directory")
	flag.Parse()
	cfg.Version = version
	cfg.Command = flag.Arg(0)
	addresses, err := parseListenAddresses(listen)
	if err != nil {
		return Config{}, err
	}
	cfg.Listen = addresses
	if strings.TrimSpace(cfg.FleetListen) == "" {
		fleetListen, err := adjacentListenAddress(cfg.PrimaryListen())
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

// parseListenAddresses turns the -listen value into the console addresses to
// bind. A single address behaves exactly as it always did, so a unit file in
// the field keeps working. A comma separated list binds every address in it,
// which is how one console answers on the local network and on Tailscale at
// the same time.
//
// Every entry must be a complete host:port. An empty entry is a typo (a
// trailing or doubled comma), and a manager that silently dropped it would
// bind fewer addresses than the owner asked for, so it is refused. An exact
// repeat of an address is not a typo with consequences: it names the same
// socket, and binding it twice could only fail, so the second mention is
// folded into the first.
func parseListenAddresses(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("listen address cannot be empty")
	}
	var addresses []string
	seen := map[string]bool{}
	for _, field := range strings.Split(value, ",") {
		address := strings.TrimSpace(field)
		if address == "" {
			return nil, errors.New("listen address cannot be empty")
		}
		_, portText, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid listen address %q: %w", address, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid listen address %q: the port must be a number from 1 to 65535", address)
		}
		if seen[address] {
			continue
		}
		seen[address] = true
		addresses = append(addresses, address)
	}
	return addresses, nil
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
