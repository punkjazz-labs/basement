package update

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// A capability-less root updater may not inspect another user's /proc entry;
// only that specific refusal is skippable, because the healthz version probe
// still proves which build answers. Everything else stays fatal.
func TestExeVerificationSkippableOnlyForPermissionDenied(t *testing.T) {
	if !exeVerificationSkippable(fs.ErrPermission) {
		t.Fatal("a permission refusal must be skippable")
	}
	if !exeVerificationSkippable(fmt.Errorf("readlink: %w", fs.ErrPermission)) {
		t.Fatal("a wrapped permission refusal must be skippable")
	}
	if exeVerificationSkippable(fs.ErrNotExist) {
		t.Fatal("a missing process is not a permission problem and must stay fatal")
	}
}

func TestSystemHealthCheckerRequiresExpectedExecutableAndVersion(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	processDir := filepath.Join(procRoot, "1")
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "versions", "v2.0.0", managerFileName)
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, arm64ELF(nil), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(processDir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "cmdline"), []byte("basement\x00--listen\x00127.0.0.1:7070\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	checker := NewSystemHealthChecker(&fakeServiceController{})
	checker.ProcRoot = procRoot
	checker.StableChecks = 1
	checker.Timeout = time.Second
	checker.PollInterval = time.Millisecond
	checker.Client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "http://127.0.0.1:7070/healthz" {
			t.Fatalf("health URL = %q", request.URL.String())
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"ok","version":"v2.0.0"}`))}, nil
	})}
	if err := checker.Check(context.Background(), "v2.0.0", executable); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(context.Background(), "v3.0.0", executable); err == nil {
		t.Fatal("health response with the wrong version was accepted")
	}
	other := filepath.Join(root, "versions", "v3.0.0", managerFileName)
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, arm64ELF(nil), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(context.Background(), "v2.0.0", other); err == nil {
		t.Fatal("health check accepted the wrong executable slot")
	}
}
