package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type HealthChecker interface {
	Check(context.Context, string, string) error
}

type SystemHealthChecker struct {
	Service      ServiceController
	ProcRoot     string
	Client       *http.Client
	LocalIPs     func() ([]net.IP, error)
	Timeout      time.Duration
	StableChecks int
	PollInterval time.Duration
}

func NewSystemHealthChecker(service ServiceController) *SystemHealthChecker {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	return &SystemHealthChecker{
		Service: service, ProcRoot: "/proc", Client: &http.Client{Transport: transport, Timeout: 2 * time.Second},
		LocalIPs: interfaceIPs, Timeout: 45 * time.Second, StableChecks: 3, PollInterval: time.Second,
	}
}

func (checker *SystemHealthChecker) Check(ctx context.Context, expectedVersion, expectedExecutable string) error {
	if checker == nil || checker.Service == nil {
		return errors.New("manager health checker has no service controller")
	}
	timeout := checker.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	stableChecks := checker.StableChecks
	if stableChecks <= 0 {
		stableChecks = 3
	}
	pollInterval := checker.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	consecutive := 0
	var lastErr error
	for {
		if err := checker.checkOnce(deadline, expectedVersion, expectedExecutable); err != nil {
			lastErr = err
			consecutive = 0
		} else {
			consecutive++
			if consecutive >= stableChecks {
				return nil
			}
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-deadline.Done():
			timer.Stop()
			if lastErr == nil {
				lastErr = deadline.Err()
			}
			return fmt.Errorf("manager did not become stably healthy: %w", lastErr)
		case <-timer.C:
		}
	}
}

func (checker *SystemHealthChecker) checkOnce(ctx context.Context, expectedVersion, expectedExecutable string) error {
	pid, err := checker.Service.MainPID(ctx)
	if err != nil || pid <= 0 {
		return errors.New("basement.service has no running main process")
	}
	procRoot := checker.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	executable, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
	if err != nil {
		return fmt.Errorf("read manager executable: %w", err)
	}
	expected, err := filepath.EvalSymlinks(expectedExecutable)
	if err != nil {
		return fmt.Errorf("resolve expected manager executable: %w", err)
	}
	actual, err := filepath.EvalSymlinks(executable)
	if err != nil || actual != expected {
		return errors.New("basement.service is not running the selected version slot")
	}
	commandLine, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return fmt.Errorf("read manager command line: %w", err)
	}
	listen, err := listenArgument(commandLine)
	if err != nil {
		return err
	}
	probe, err := checker.localProbeAddress(listen)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+probe+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := checker.client().Do(request)
	if err != nil {
		return fmt.Errorf("probe manager health: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("manager health returned HTTP %d", response.StatusCode)
	}
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	if err := decoder.Decode(&health); err != nil {
		return errors.New("manager health response is invalid")
	}
	if health.Status != "ok" || health.Version != expectedVersion {
		return errors.New("manager health response names the wrong version")
	}
	return nil
}

func (checker *SystemHealthChecker) localProbeAddress(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", errors.New("manager listen argument is invalid")
	}
	ip := net.ParseIP(host)
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	if ip == nil {
		return "", errors.New("manager listen address is not a local IP address")
	}
	if !ip.IsLoopback() {
		localIPs := checker.LocalIPs
		if localIPs == nil {
			localIPs = interfaceIPs
		}
		addresses, err := localIPs()
		if err != nil {
			return "", errors.New("local interface addresses are unavailable")
		}
		local := false
		for _, address := range addresses {
			if address.Equal(ip) {
				local = true
				break
			}
		}
		if !local {
			return "", errors.New("manager listen address is not assigned to this machine")
		}
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func (checker *SystemHealthChecker) client() *http.Client {
	if checker.Client != nil {
		return checker.Client
	}
	return NewSystemHealthChecker(checker.Service).Client
}

func listenArgument(commandLine []byte) (string, error) {
	arguments := strings.Split(strings.TrimRight(string(commandLine), "\x00"), "\x00")
	for index, argument := range arguments {
		if strings.HasPrefix(argument, "--listen=") {
			return strings.TrimPrefix(argument, "--listen="), nil
		}
		if argument == "--listen" && index+1 < len(arguments) {
			return arguments[index+1], nil
		}
	}
	return "", errors.New("basement.service main process has no listen argument")
}

func interfaceIPs() ([]net.IP, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		var value string
		switch typed := address.(type) {
		case *net.IPNet:
			result = append(result, typed.IP)
			continue
		case *net.IPAddr:
			result = append(result, typed.IP)
			continue
		default:
			value = address.String()
		}
		if host, _, splitErr := net.SplitHostPort(value); splitErr == nil {
			value = host
		} else if before, _, found := strings.Cut(value, "/"); found {
			value = before
		}
		if ip := net.ParseIP(value); ip != nil {
			result = append(result, ip)
		}
	}
	return result, nil
}
