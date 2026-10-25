package attached

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validDeclaration = `schema_version: 1
id: video-pair
display_name: Video pair
model_by: Example Lab
kind: video-generation
served_model_name: video-model
managed_by: "systemd: video.service"
endpoint: "http://127.0.0.1:8790 (head)"
health_url: http://127.0.0.1:8790/health
topology:
  spark_count: 2
  parallelism: TP1 x SP2 pair
  nodes:
    - node_name: head-box
      role: head/rank0
    - node_id: node-worker-id
      role: worker/rank1
licence: Example Licence
licence_url: https://example.com/LICENSE
licence_territory_exclusions:
  - Somewhere
`

func TestShippedExampleIsAValidDeclaration(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "examples", "attached-services", "fasth3-gpu-pair.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := Decode(data)
	if err != nil {
		t.Fatalf("the example declaration must stay valid: %v", err)
	}
	if service.ID != "fasth3-gpu-pair" || service.Kind != "video-generation" || service.ServedModelName != "fasth3-omniref" {
		t.Fatalf("example identity = %+v", service)
	}
	if service.Topology.SparkCount != 2 || len(service.Topology.Nodes) != 2 {
		t.Fatalf("example topology = %+v", service.Topology)
	}
	want := []string{"European Union", "United Kingdom", "Republic of Korea", "United States of America"}
	if !reflect.DeepEqual(service.LicenceTerritoryExclusions, want) {
		t.Fatalf("territory exclusions = %v", service.LicenceTerritoryExclusions)
	}
	if service.LicenceURL != "" {
		t.Fatal("the example must not carry a licence URL nobody recorded a source for")
	}
	if !service.Names("", "head-box") || !service.Names("", "WORKER-BOX.local") || service.Names("", "other-box") {
		t.Fatal("the example must name exactly the two Sparks of the pair")
	}
	if service.HealthInterval() != 30*time.Second {
		t.Fatalf("health interval = %s", service.HealthInterval())
	}
}

func TestDecodeAcceptsAFullDeclaration(t *testing.T) {
	service, err := Decode([]byte(validDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	if service.DisplayName != "Video pair" || service.ManagedBy != "systemd: video.service" || service.Topology.Nodes[1].NodeID != "node-worker-id" {
		t.Fatalf("decoded = %+v", service)
	}
	if service.HealthInterval() != DefaultHealthInterval {
		t.Fatalf("an absent interval must default to %s, got %s", DefaultHealthInterval, service.HealthInterval())
	}
}

func TestDecodeRefusesWhatCannotBeTrusted(t *testing.T) {
	cases := map[string]struct {
		edit func(string) string
		want string
	}{
		"unknown key": {func(s string) string { return s + "nodes_typo: []\n" }, "field nodes_typo not found"},
		"schema":      {func(s string) string { return strings.Replace(s, "schema_version: 1", "schema_version: 2", 1) }, "schema_version must be 1"},
		"id":          {func(s string) string { return strings.Replace(s, "id: video-pair", "id: Video Pair", 1) }, "id must be"},
		"display name": {func(s string) string {
			return strings.Replace(s, "display_name: Video pair", "display_name: \"  \"", 1)
		}, "display_name is required"},
		"kind": {func(s string) string { return strings.Replace(s, "kind: video-generation", "kind: Video", 1) }, "kind must be"},
		"managed by": {func(s string) string {
			return strings.Replace(s, "managed_by: \"systemd: video.service\"", "managed_by: \"\"", 1)
		}, "managed_by is required"},
		"health scheme": {func(s string) string {
			return strings.Replace(s, "health_url: http://127.0.0.1:8790/health", "health_url: file:///etc/passwd", 1)
		}, "health_url must be an absolute http or https URL"},
		"health credential": {func(s string) string {
			return strings.Replace(s, "health_url: http://127.0.0.1:8790/health", "health_url: http://user:secret@127.0.0.1:8790/health", 1)
		}, "must not carry a user name or password"},
		"interval":    {func(s string) string { return s + "health_interval_seconds: 1\n" }, "health_interval_seconds must be between 5 and 3600"},
		"count":       {func(s string) string { return strings.Replace(s, "spark_count: 2", "spark_count: 3", 1) }, "must name exactly 3 Sparks"},
		"count range": {func(s string) string { return strings.Replace(s, "spark_count: 2", "spark_count: 5", 1) }, "spark_count must be between 1 and 4"},
		"both names": {func(s string) string {
			return strings.Replace(s, "    - node_name: head-box\n", "    - node_name: head-box\n      node_id: also\n", 1)
		}, "exactly one of node_name and node_id"},
		"duplicate node": {func(s string) string {
			return strings.Replace(s, "    - node_id: node-worker-id\n", "    - node_name: HEAD-BOX.local\n", 1)
		}, "names a Spark already listed"},
		"licence link": {func(s string) string {
			return strings.Replace(s, "licence_url: https://example.com/LICENSE", "licence_url: javascript:alert(1)", 1)
		}, "licence_url must be an absolute https URL"},
		"blank territory": {func(s string) string { return strings.Replace(s, "  - Somewhere\n", "  - \"\"\n", 1) }, "non-empty list of non-blank strings"},
		"territory without licence": {func(s string) string {
			return strings.Replace(strings.Replace(s, "licence: Example Licence\n", "", 1), "licence_url: https://example.com/LICENSE\n", "", 1)
		}, "needs the licence they come from"},
		"two lines": {func(s string) string {
			return strings.Replace(s, "endpoint: \"http://127.0.0.1:8790 (head)\"", "endpoint: \"a\\nb\"", 1)
		}, "endpoint must be one line"},
		"two documents": {func(s string) string { return s + "---\n" + s }, "more than one YAML document"},
		"empty":         {func(string) string { return "" }, "the file is empty"},
		"oversized":     {func(s string) string { return s + "# " + strings.Repeat("x", maxDeclarationBytes) + "\n" }, "larger than 64 KiB"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(test.edit(validDeclaration)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestNodeMatching(t *testing.T) {
	byName := Node{NodeName: "Head-Box.example.ts.net"}
	for _, name := range []string{"head-box", "HEAD-BOX", "head-box.local", " head-box "} {
		if !byName.Matches("", name) {
			t.Fatalf("%q should match %q", byName.NodeName, name)
		}
	}
	for _, name := range []string{"", "head-box0", "head", "other-box"} {
		if byName.Matches("node-1", name) {
			t.Fatalf("%q must not match %q", byName.NodeName, name)
		}
	}
	byID := Node{NodeID: "node-1"}
	if !byID.Matches("node-1", "anything") || byID.Matches("node-2", "node-1") || byID.Matches("", "") {
		t.Fatal("a node id matches that exact id and nothing else")
	}
	if byID.Label() != "node-1" || byName.Label() != byName.NodeName {
		t.Fatal("a node is labelled by its name, or by its id when it has no name")
	}
}
