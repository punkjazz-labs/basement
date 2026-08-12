package update

import (
	"errors"
	"strconv"
	"strings"
)

// The root updater helper is a second signed binary on the machine, and until
// protocol 2 nothing could name the build that was installed. These two
// functions are the whole contract between the helper's `version` subcommand
// and the manager that reads it: one line, key=value fields, no state
// directory, no lock, no privilege.
//
// The line is also how the manager learns which request schema the installed
// helper can accept. That matters because both sides decode strictly: a
// schema-2 request handed to a helper that predates protocol 2 is refused as
// an unknown field, and the update it belonged to fails for a reason that has
// nothing to do with the manager. ADR 0020 leaves that case unstated; asking
// the helper first is the only answer available without weakening the strict
// decoding the ADR keeps on purpose.

const helperVersionPrefix = "basement-updater"

// HelperIdentity is what one `basement-updater version` line says.
type HelperIdentity struct {
	Version  string
	Protocol int
	SHA256   string
}

// HelperVersionLine renders the helper's own answer. It is built from the
// version stamped into the binary at release time and the digest of the bytes
// actually executing.
func HelperVersionLine(identity HelperIdentity) string {
	return helperVersionPrefix +
		" version=" + identity.Version +
		" protocol=" + strconv.Itoa(identity.Protocol) +
		" sha256=" + identity.SHA256
}

// ParseHelperVersion reads one line back. Anything it does not recognize is
// an error rather than a partial answer, because a partial answer here would
// read as a helper that is older than it is.
func ParseHelperVersion(output string) (HelperIdentity, error) {
	line := strings.TrimSpace(output)
	if index := strings.IndexAny(line, "\r\n"); index >= 0 {
		line = strings.TrimSpace(line[:index])
	}
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != helperVersionPrefix {
		return HelperIdentity{}, errors.New("the root updater helper did not report a version line")
	}
	var identity HelperIdentity
	for _, field := range fields[1:] {
		name, value, ok := strings.Cut(field, "=")
		if !ok || value == "" {
			return HelperIdentity{}, errors.New("the root updater helper version line is malformed")
		}
		switch name {
		case "version":
			identity.Version = value
		case "protocol":
			protocol, err := strconv.Atoi(value)
			if err != nil || protocol < 1 {
				return HelperIdentity{}, errors.New("the root updater helper reported an invalid protocol")
			}
			identity.Protocol = protocol
		case "sha256":
			if !hexDigestPattern.MatchString(value) {
				return HelperIdentity{}, errors.New("the root updater helper reported an invalid digest")
			}
			identity.SHA256 = value
		default:
			return HelperIdentity{}, errors.New("the root updater helper version line is malformed")
		}
	}
	if identity.Version == "" || identity.Protocol == 0 || identity.SHA256 == "" {
		return HelperIdentity{}, errors.New("the root updater helper version line is incomplete")
	}
	return identity, nil
}

// RunningHelperIdentity answers the `version` subcommand. It reads the bytes
// executing right now, takes no lock, writes nothing, and needs no privilege.
func RunningHelperIdentity(version string) (HelperIdentity, error) {
	path, err := runningExecutablePath()
	if err != nil {
		return HelperIdentity{}, err
	}
	digest, err := FileDigest(path)
	if err != nil {
		return HelperIdentity{}, err
	}
	if version == "" {
		version = "dev"
	}
	return HelperIdentity{Version: version, Protocol: UpdaterProtocol, SHA256: digest}, nil
}
