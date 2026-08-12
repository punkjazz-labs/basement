package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The version subcommand is the only thing on a machine that can name the
// helper build that is installed, and the only way a manager learns anything
// about a helper across a version boundary. It has to survive a round trip
// exactly.
func TestHelperVersionLineRoundTrips(t *testing.T) {
	digest := sha256.Sum256([]byte("helper"))
	want := HelperIdentity{Version: "v2.0.0", Protocol: 2, SHA256: hex.EncodeToString(digest[:])}
	got, err := ParseHelperVersion(HelperVersionLine(want) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestParseHelperVersionRefusesAnythingPartial(t *testing.T) {
	digest := hex.EncodeToString(func() []byte { sum := sha256.Sum256(nil); return sum[:] }())
	for _, line := range []string{
		"",
		"usage: basement-updater apply",
		"basement-updater version=v2.0.0 protocol=2",
		"basement-updater version=v2.0.0 protocol=two sha256=" + digest,
		"basement-updater version=v2.0.0 protocol=2 sha256=not-a-digest",
		"basement-updater version= protocol=2 sha256=" + digest,
		"basement-updater version=v2.0.0 protocol=2 unknown=" + digest,
		"other-binary version=v2.0.0 protocol=2 sha256=" + digest,
	} {
		if _, err := ParseHelperVersion(line); err == nil {
			t.Fatalf("accepted %q as a helper version line", line)
		}
	}
}

// The subcommand reports the bytes actually executing, takes no lock, writes
// nothing and needs no privilege. Running it against this test binary proves
// the whole path including the platform split behind /proc/self/exe.
func TestRunningHelperIdentityHashesTheRunningBinary(t *testing.T) {
	identity, err := RunningHelperIdentity("v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Version != "v2.0.0" || identity.Protocol != UpdaterProtocol {
		t.Fatalf("identity = %+v", identity)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := FileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	if identity.SHA256 != digest {
		t.Fatalf("digest = %q, want the running binary's %q", identity.SHA256, digest)
	}
	if _, err := ParseHelperVersion(HelperVersionLine(identity)); err != nil {
		t.Fatalf("the subcommand's own output does not parse: %v", err)
	}
}

func TestRunningHelperIdentityNamesAnUnstampedBuild(t *testing.T) {
	identity, err := RunningHelperIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Version != "dev" {
		t.Fatalf("version = %q, want a local build to say so", identity.Version)
	}
}

func TestHelperReportStates(t *testing.T) {
	digest := sha256.Sum256(arm64ELF([]byte("installed helper")))
	installed := hex.EncodeToString(digest[:])

	t.Run("ok", func(t *testing.T) {
		stager := stagerFixture(t)
		installHelper(t, stager, arm64ELF([]byte("installed helper")))
		stager.HelperVersionRunner = fixedHelperVersion("v1.0.0", 2, installed)
		report := stager.HelperReport()
		if report.State != helperStateOK || report.SHA256 != installed {
			t.Fatalf("report = %+v", report)
		}
		if report.Version != "v1.0.0" || report.Protocol != 2 || report.Warning != "" {
			t.Fatalf("report = %+v, want the installed build named with no warning", report)
		}
	})

	// Mode 0700 on the installed helper reads exactly like this, and it is
	// the failure this ADR spells the chmod out for. Root still executes the
	// helper, so the honest answer is unknown, never stale.
	t.Run("unreadable is unknown", func(t *testing.T) {
		stager := stagerFixture(t)
		stager.UpdaterBinaryPath = filepath.Join(t.TempDir(), "basement-updater")
		report := stager.HelperReport()
		if report.State != helperStateUnknown || report.SHA256 != "" {
			t.Fatalf("report = %+v, want unknown with no digest", report)
		}
		if report.Warning == "" {
			t.Fatal("an unreadable helper was reported without any explanation")
		}
	})

	// A helper that will not run is a warning, never a refusal: the update
	// is the repair.
	t.Run("will not run", func(t *testing.T) {
		stager := stagerFixture(t)
		installHelper(t, stager, arm64ELF([]byte("installed helper")))
		stager.HelperVersionRunner = func(context.Context, string) (string, error) {
			return "", errors.New("exit status 2")
		}
		report := stager.HelperReport()
		if report.State != helperStateOK || report.SHA256 != installed {
			t.Fatalf("report = %+v, the binary was still readable", report)
		}
		if report.Protocol != 0 || !strings.Contains(report.Warning, "did not answer") {
			t.Fatalf("report = %+v, want a warning and no claimed protocol", report)
		}
	})
}

func installHelper(t *testing.T, stager *Stager, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "basement-updater")
	if err := os.WriteFile(path, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	stager.UpdaterBinaryPath = path
	return path
}

func fixedHelperVersion(version string, protocol int, digest string) func(context.Context, string) (string, error) {
	line := HelperVersionLine(HelperIdentity{Version: version, Protocol: protocol, SHA256: digest})
	return func(context.Context, string) (string, error) { return line + "\n", nil }
}
