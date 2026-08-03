package recipe

import (
	"strings"
	"testing"
)

// twoSparkFixture takes a shipped single-Spark recipe and gives it the
// minimum a two-Spark recipe must carry. No two-Spark recipe ships yet, so
// the fixture lives here rather than in the recipe pack.
func twoSparkFixture(t *testing.T) Recipe {
	t.Helper()
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base, ok := Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("base recipe missing")
	}
	base.Topology = Topology{
		SparkCount: 2,
		Interconnect: &Interconnect{
			Kind:       "connectx7",
			MasterPort: 29501,
			SharedEnvironment: map[string]string{
				"NCCL_IB_DISABLE": "0", "NCCL_IB_HCA": "rocep1s0f0", "NCCL_IB_GID_INDEX": "3",
				"NCCL_SOCKET_IFNAME": "enp1s0f0np0", "GLOO_SOCKET_IFNAME": "enp1s0f0np0",
				"TP_SOCKET_IFNAME": "enp1s0f0np0", "NCCL_IGNORE_CPU_AFFINITY": "1", "NCCL_DEBUG": "WARN",
			},
		},
	}
	base.Service.VLLM.TensorParallelSize = 2
	return base
}

func TestTwoSparkRecipeNeedsAnInterconnect(t *testing.T) {
	valid := twoSparkFixture(t)
	if err := Validate(valid); err != nil {
		t.Fatalf("a complete two-Spark recipe was rejected: %v", err)
	}
	if !valid.Distributed() || valid.Topology.SocketInterface() != "enp1s0f0np0" {
		t.Fatalf("two-Spark topology did not read back: %#v", valid.Topology)
	}

	tests := []struct {
		name   string
		mutate func(*Recipe)
		want   string
	}{
		{"no interconnect", func(r *Recipe) { r.Topology.Interconnect = nil }, "must declare topology.interconnect"},
		{"unknown fabric", func(r *Recipe) { r.Topology.Interconnect.Kind = "ethernet" }, "must be connectx7"},
		{"privileged master port", func(r *Recipe) { r.Topology.Interconnect.MasterPort = 22 }, "non-privileged port"},
		{"no socket interface", func(r *Recipe) {
			delete(r.Topology.Interconnect.SharedEnvironment, "NCCL_SOCKET_IFNAME")
		}, "NCCL_SOCKET_IFNAME"},
		{"smuggled environment", func(r *Recipe) {
			r.Topology.Interconnect.WorkerEnvironment = map[string]string{"LD_PRELOAD": "/tmp/evil.so"}
		}, "outside the allowlist"},
		{"tensor parallelism does not span the topology", func(r *Recipe) { r.Service.VLLM.TensorParallelSize = 1 }, "numeric settings are invalid"},
		{"three Sparks", func(r *Recipe) { r.Topology.SparkCount = 3 }, "spark_count must be 1 or 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := twoSparkFixture(t)
			test.mutate(&candidate)
			err := Validate(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want a problem mentioning %q", err, test.want)
			}
		})
	}
}

func TestSingleSparkRecipesAreUnaffected(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recipes {
		if err := Validate(r); err != nil {
			t.Fatalf("shipped recipe %s became invalid: %v", r.ID, err)
		}
		if r.Distributed() || r.Topology.Interconnect != nil {
			t.Fatalf("shipped recipe %s is not single-Spark: %#v", r.ID, r.Topology)
		}
	}
	half := recipes[0]
	half.Topology.Interconnect = &Interconnect{Kind: "connectx7", MasterPort: 29501}
	err = Validate(half)
	if err == nil || !strings.Contains(err.Error(), "must not declare topology.interconnect") {
		t.Fatalf("got %v, want a single-Spark recipe to refuse an interconnect block", err)
	}
}
