package update_test

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestManagerCrossCompilesForEveryPublishedTarget catches a portability break
// at test time rather than at tag time. The release workflow builds the
// manager for three operating systems and two architectures, and CI only
// builds for the machine it runs on, so an import that does not exist on
// Windows compiled cleanly on every push and failed the moment a tag was
// pushed, after the tag already existed. That is exactly what golang.org/x/
// sys/unix did once the self-update landed.
//
// The target list is the release workflow's, and the two have to stay in
// step: a target added there without being added here goes back to being
// discovered by a failed release.
func TestManagerCrossCompilesForEveryPublishedTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiling every target is slow")
	}
	targets := []struct{ os, arch string }{
		{"linux", "arm64"}, {"linux", "amd64"},
		{"darwin", "arm64"}, {"darwin", "amd64"},
		{"windows", "arm64"}, {"windows", "amd64"},
	}
	for _, target := range targets {
		t.Run(target.os+"/"+target.arch, func(t *testing.T) {
			t.Parallel()
			command := exec.Command("go", "build", "-o", os.DevNull, "./cmd/basement")
			command.Dir = "../.."
			command.Env = append(command.Environ(),
				"GOOS="+target.os, "GOARCH="+target.arch, "CGO_ENABLED=0")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("the manager does not build for %s/%s, which the release publishes:\n%s",
					target.os, target.arch, strings.TrimSpace(string(output)))
			}
		})
	}
}

// The root updater helper ships for exactly one target, and its version
// subcommand reads /proc/self/exe, which only exists on Linux. That split is
// the same shape as the import that once broke a release at tag time, so it
// is checked here rather than discovered there. The host target is built too,
// because that is what runs this package's own tests.
func TestUpdaterHelperBuildsForItsPublishedTargetAndTheHost(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiling is slow")
	}
	for _, target := range []struct{ os, arch string }{
		{"linux", "arm64"}, {runtime.GOOS, runtime.GOARCH},
	} {
		t.Run(target.os+"/"+target.arch, func(t *testing.T) {
			t.Parallel()
			command := exec.Command("go", "build", "-o", os.DevNull, "./cmd/basement-updater")
			command.Dir = "../.."
			command.Env = append(command.Environ(),
				"GOOS="+target.os, "GOARCH="+target.arch, "CGO_ENABLED=0")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("the root updater helper does not build for %s/%s:\n%s",
					target.os, target.arch, strings.TrimSpace(string(output)))
			}
		})
	}
}
