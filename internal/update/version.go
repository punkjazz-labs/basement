package update

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a stable manager release. Update manifests deliberately accept
// no prerelease, build metadata, missing component, or leading zero.
type Version struct {
	Major uint64
	Minor uint64
	Patch uint64
}

func ParseVersion(value string) (Version, error) {
	if !strings.HasPrefix(value, "v") {
		return Version{}, errors.New("version must start with v")
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return Version{}, errors.New("version must have major, minor, and patch components")
	}
	values := make([]uint64, len(parts))
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, errors.New("version components must be canonical decimal numbers")
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return Version{}, errors.New("version components must be decimal numbers")
			}
		}
		parsed, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("version component is out of range: %w", err)
		}
		values[index] = parsed
	}
	return Version{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

func (v Version) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func (v Version) Compare(other Version) int {
	left := [...]uint64{v.Major, v.Minor, v.Patch}
	right := [...]uint64{other.Major, other.Minor, other.Patch}
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

// IsNewer returns false for development or otherwise non-release builds.
func IsNewer(current, candidate string) bool {
	running, err := ParseVersion(current)
	if err != nil {
		return false
	}
	target, err := ParseVersion(candidate)
	return err == nil && target.Compare(running) > 0
}
