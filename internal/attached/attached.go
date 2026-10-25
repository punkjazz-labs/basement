// Package attached reads the model services an operator runs on these Sparks
// outside basement and declares to it (ADR 0025). basement shows them, checks
// their health with one GET, and keeps its own models off the Sparks they
// name. It never installs, starts, stops, updates or calls them in any other
// way: their lifecycle belongs to whatever the declaration says manages them.
package attached

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DirectoryName is the operator-owned directory under the data directory.
// basement only ever reads it.
const DirectoryName = "attached-services.d"

const (
	DefaultHealthInterval = 30 * time.Second
	minHealthInterval     = 5
	maxHealthInterval     = 3600
	// A declaration is a few hundred bytes. Anything far larger is not one,
	// and is refused before it is parsed.
	maxDeclarationBytes = 64 << 10
	// The fleet admits four nodes (store.MaxFleetNodes), so no service can
	// span more Sparks than that.
	maxSparkCount = 4
)

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,79}$`)
	kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)
)

// Service is one declaration, one file. Every user-visible field is the
// operator's own words, shown as written: basement adds no attribution,
// licence or topology fact of its own, and an absent optional field is shown
// as n/a.
type Service struct {
	SchemaVersion   int    `yaml:"schema_version" json:"schema_version"`
	ID              string `yaml:"id" json:"id"`
	DisplayName     string `yaml:"display_name" json:"display_name"`
	ModelBy         string `yaml:"model_by,omitempty" json:"model_by,omitempty"`
	Kind            string `yaml:"kind" json:"kind"`
	ServedModelName string `yaml:"served_model_name,omitempty" json:"served_model_name,omitempty"`
	// ManagedBy names what owns the lifecycle, in free text. It is required:
	// a service with no named owner would read as one nobody runs.
	ManagedBy string `yaml:"managed_by" json:"managed_by"`
	// Endpoint is display text only. It is never dialled, because it usually
	// names an address on another machine or one reached through a tunnel.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// HealthURL is the one address basement calls, with GET, from this node.
	HealthURL                  string   `yaml:"health_url" json:"health_url"`
	HealthIntervalSeconds      int      `yaml:"health_interval_seconds,omitempty" json:"health_interval_seconds,omitempty"`
	Topology                   Topology `yaml:"topology" json:"topology"`
	Licence                    string   `yaml:"licence,omitempty" json:"licence,omitempty"`
	LicenceURL                 string   `yaml:"licence_url,omitempty" json:"licence_url,omitempty"`
	LicenceTerritoryExclusions []string `yaml:"licence_territory_exclusions,omitempty" json:"licence_territory_exclusions,omitempty"`
	Notes                      string   `yaml:"notes,omitempty" json:"notes,omitempty"`
}

type Topology struct {
	SparkCount  int    `yaml:"spark_count" json:"spark_count"`
	Parallelism string `yaml:"parallelism,omitempty" json:"parallelism,omitempty"`
	Nodes       []Node `yaml:"nodes" json:"nodes"`
}

// Node names one Spark the service runs on, by the name the fleet shows for
// it or by its stable node id. Exactly one of the two is given.
type Node struct {
	NodeName string `yaml:"node_name,omitempty" json:"node_name,omitempty"`
	NodeID   string `yaml:"node_id,omitempty" json:"node_id,omitempty"`
	Role     string `yaml:"role,omitempty" json:"role,omitempty"`
}

// HealthInterval is how often the health URL is called.
func (s Service) HealthInterval() time.Duration {
	if s.HealthIntervalSeconds == 0 {
		return DefaultHealthInterval
	}
	return time.Duration(s.HealthIntervalSeconds) * time.Second
}

// Names reports whether the declaration names the node with this id or this
// display name. A name compares without case and without any domain suffix,
// so a declaration written with a full DNS name still matches the short name
// a Spark reports for itself, and the other way round.
func (s Service) Names(nodeID, displayName string) bool {
	for _, node := range s.Topology.Nodes {
		if node.Matches(nodeID, displayName) {
			return true
		}
	}
	return false
}

func (n Node) Matches(nodeID, displayName string) bool {
	if n.NodeID != "" {
		return nodeID != "" && n.NodeID == nodeID
	}
	short := shortName(displayName)
	return short != "" && shortName(n.NodeName) == short
}

// Label is how the node is written in the console: its name, or its id when
// that is all the declaration gave.
func (n Node) Label() string {
	if n.NodeName != "" {
		return n.NodeName
	}
	return n.NodeID
}

func shortName(name string) string {
	short, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(name)), ".")
	return short
}

// Decode reads one declaration strictly: an unknown key is an error, exactly
// as it is for a recipe, because a misspelt key that was silently ignored
// would leave a Spark unprotected while the file looked correct.
func Decode(data []byte) (Service, error) {
	if len(data) > maxDeclarationBytes {
		return Service{}, fmt.Errorf("the file is larger than %d KiB, so it is not read as a declaration", maxDeclarationBytes>>10)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var service Service
	if err := decoder.Decode(&service); err != nil {
		if errors.Is(err, io.EOF) {
			return Service{}, errors.New("the file is empty")
		}
		return Service{}, fmt.Errorf("the file is not a valid declaration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Service{}, errors.New("the file holds more than one YAML document; declare one service per file")
	}
	if err := Validate(service); err != nil {
		return Service{}, err
	}
	return service, nil
}

func Validate(s Service) error {
	var problems []string
	if s.SchemaVersion != 1 {
		problems = append(problems, "schema_version must be 1")
	}
	if !idPattern.MatchString(s.ID) {
		problems = append(problems, "id must be 3 to 80 lowercase letters, digits or hyphens")
	}
	problems = append(problems, requiredText("display_name", s.DisplayName, 120)...)
	if !kindPattern.MatchString(s.Kind) {
		problems = append(problems, "kind must be a lowercase word such as chat or video-generation")
	}
	problems = append(problems, requiredText("managed_by", s.ManagedBy, 300)...)
	problems = append(problems, optionalText("model_by", s.ModelBy, 120)...)
	problems = append(problems, optionalText("served_model_name", s.ServedModelName, 200)...)
	problems = append(problems, optionalText("endpoint", s.Endpoint, 300)...)
	problems = append(problems, optionalText("licence", s.Licence, 200)...)
	if len(s.Notes) > 2000 {
		problems = append(problems, "notes must be at most 2000 characters")
	}
	if err := checkHealthURL(s.HealthURL); err != nil {
		problems = append(problems, err.Error())
	}
	if s.HealthIntervalSeconds != 0 && (s.HealthIntervalSeconds < minHealthInterval || s.HealthIntervalSeconds > maxHealthInterval) {
		problems = append(problems, fmt.Sprintf("health_interval_seconds must be between %d and %d", minHealthInterval, maxHealthInterval))
	}
	problems = append(problems, checkTopology(s.Topology)...)
	if s.LicenceURL != "" {
		// The console renders it as a link, so only an absolute HTTPS URL is
		// accepted; anything else could be a script URL in a link.
		parsed, err := url.Parse(s.LicenceURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			problems = append(problems, "licence_url must be an absolute https URL")
		}
		if strings.TrimSpace(s.Licence) == "" {
			problems = append(problems, "licence_url needs the licence it points at")
		}
	}
	if s.LicenceTerritoryExclusions != nil {
		blank := false
		for _, territory := range s.LicenceTerritoryExclusions {
			if strings.TrimSpace(territory) == "" {
				blank = true
			}
		}
		if len(s.LicenceTerritoryExclusions) == 0 || blank {
			problems = append(problems, "licence_territory_exclusions must be a non-empty list of non-blank strings")
		}
		if strings.TrimSpace(s.Licence) == "" {
			problems = append(problems, "licence_territory_exclusions needs the licence they come from")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func checkHealthURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("health_url must be an absolute http or https URL")
	}
	// The URL is shown in the console and written to the log on every failed
	// check, so it may not carry a credential.
	if parsed.User != nil {
		return errors.New("health_url must not carry a user name or password")
	}
	return nil
}

func checkTopology(topology Topology) []string {
	var problems []string
	if topology.SparkCount < 1 || topology.SparkCount > maxSparkCount {
		problems = append(problems, fmt.Sprintf("topology.spark_count must be between 1 and %d", maxSparkCount))
	}
	if len(topology.Nodes) != topology.SparkCount {
		problems = append(problems, fmt.Sprintf("topology.nodes must name exactly %d Sparks, one per spark_count", topology.SparkCount))
	}
	problems = append(problems, optionalText("topology.parallelism", topology.Parallelism, 120)...)
	seen := map[string]bool{}
	for index, node := range topology.Nodes {
		prefix := fmt.Sprintf("topology.nodes[%d]", index)
		name, id := strings.TrimSpace(node.NodeName), strings.TrimSpace(node.NodeID)
		if (name == "") == (id == "") {
			problems = append(problems, prefix+" must give exactly one of node_name and node_id")
			continue
		}
		if name != node.NodeName || id != node.NodeID {
			problems = append(problems, prefix+" must not start or end with spaces")
		}
		problems = append(problems, optionalText(prefix+".role", node.Role, 60)...)
		key := "id:" + id
		if name != "" {
			key = "name:" + shortName(name)
		}
		if seen[key] {
			problems = append(problems, prefix+" names a Spark already listed")
		}
		seen[key] = true
	}
	return problems
}

func requiredText(field, value string, limit int) []string {
	if strings.TrimSpace(value) == "" {
		return []string{field + " is required"}
	}
	return optionalText(field, value, limit)
}

func optionalText(field, value string, limit int) []string {
	if len(value) > limit {
		return []string{fmt.Sprintf("%s must be at most %d characters", field, limit)}
	}
	if strings.ContainsAny(value, "\r\n") {
		return []string{field + " must be one line"}
	}
	return nil
}
