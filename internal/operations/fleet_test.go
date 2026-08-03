package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// twoSparkRecipe is a shipped single-Spark recipe given the interconnect a
// two-Spark recipe must carry. No two-Spark recipe ships yet.
func twoSparkRecipe(t *testing.T) recipe.Recipe {
	t.Helper()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	if !ok {
		t.Fatal("base recipe missing")
	}
	r.Topology = recipe.Topology{SparkCount: 2, Interconnect: &recipe.Interconnect{
		Kind:       "connectx7",
		MasterPort: 29501,
		SharedEnvironment: map[string]string{
			"NCCL_IB_DISABLE": "0", "NCCL_IB_HCA": "rocep1s0f0", "NCCL_IB_GID_INDEX": "3",
			"NCCL_SOCKET_IFNAME": "enp1s0f0np0", "GLOO_SOCKET_IFNAME": "enp1s0f0np0",
			"TP_SOCKET_IFNAME": "enp1s0f0np0", "NCCL_IGNORE_CPU_AFFINITY": "1", "NCCL_DEBUG": "WARN",
		},
		WorkerEnvironment: map[string]string{"NCCL_DEBUG": "INFO"},
	}}
	r.Service.VLLM.TensorParallelSize = 2
	if err := recipe.Validate(r); err != nil {
		t.Fatalf("fixture is not a valid two-Spark recipe: %v", err)
	}
	return r
}

func argumentValue(args []string, flag string) (string, bool) {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			return args[index+1], true
		}
	}
	return "", false
}

func hasArgument(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func TestDistributedArgumentsFollowTheCommunityRecipe(t *testing.T) {
	r := twoSparkRecipe(t)
	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}
	worker := Placement{Role: RoleWorker, NodeName: "spark-b", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}

	headArgs := vllmArgs(r, head)
	for flag, want := range map[string]string{
		"--nnodes": "2", "--node-rank": "0", "--master-addr": "169.254.10.1", "--master-port": "29501",
		"--distributed-executor-backend": "mp", "--tensor-parallel-size": "2",
	} {
		if got, ok := argumentValue(headArgs, flag); !ok || got != want {
			t.Fatalf("head %s=%q, want %q", flag, got, want)
		}
	}
	if hasArgument(headArgs, "--headless") {
		t.Fatal("the head rank must serve HTTP, so it is never headless")
	}
	// ADR 0007: the model endpoint stays loopback-only, and under host
	// networking nothing publishes a port for it.
	if host, _ := argumentValue(headArgs, "--host"); host != "127.0.0.1" {
		t.Fatalf("head bound %q, want loopback", host)
	}
	if port, _ := argumentValue(headArgs, "--port"); port != "8000" {
		t.Fatalf("head port %q, want the recipe host port", port)
	}

	workerArgs := vllmArgs(r, worker)
	if rank, _ := argumentValue(workerArgs, "--node-rank"); rank != "1" {
		t.Fatalf("worker rank %q, want 1", rank)
	}
	if !hasArgument(workerArgs, "--headless") {
		t.Fatal("the worker rank serves no HTTP and must be headless")
	}

	// Single-node recipes must be untouched by any of this.
	recipes, _ := recipe.Builtin()
	single, _ := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	singleArgs := vllmArgs(single, Placement{})
	for _, flag := range []string{"--nnodes", "--node-rank", "--master-addr", "--master-port", "--headless", "--distributed-executor-backend"} {
		if hasArgument(singleArgs, flag) {
			t.Fatalf("single-Spark launch grew %s", flag)
		}
	}
	if host, _ := argumentValue(singleArgs, "--host"); host != "0.0.0.0" {
		t.Fatalf("single-Spark host changed to %q", host)
	}
}

func TestDistributedContainerGetsTheFabricAndItsEnvironment(t *testing.T) {
	// Detection reads the machine the test runs on (CI runners can hold a
	// real RDMA device); force the recipe-fallback path so the assertions
	// are about the recipe, not the host.
	previous := fabricLink
	t.Cleanup(func() { fabricLink = previous })
	fabricLink = func() (FabricLink, error) { return FabricLink{}, errors.New("no fabric in this test") }
	r := twoSparkRecipe(t)
	var body map[string]any
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ID":"worker-container"}`))}, nil
	})}}
	worker := Placement{Role: RoleWorker, NodeName: "spark-b", NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}
	if _, err := client.Create(context.Background(), "worker", r.Runtime.Reference(), []string{"/managed/model"}, "/managed/cache", r, worker); err != nil {
		t.Fatal(err)
	}
	hostConfig := body["HostConfig"].(map[string]any)
	if hostConfig["NetworkMode"] != "host" || hostConfig["IpcMode"] != "host" {
		t.Fatalf("distributed container is not on the host fabric: %#v", hostConfig)
	}
	if _, published := hostConfig["PortBindings"]; published {
		t.Fatal("host networking publishes nothing, so a port binding is misleading")
	}
	devices, ok := hostConfig["Devices"].([]any)
	if !ok || len(devices) != 1 || devices[0].(map[string]any)["PathOnHost"] != "/dev/infiniband" {
		t.Fatalf("RDMA devices missing: %#v", hostConfig["Devices"])
	}
	environment := map[string]bool{}
	for _, entry := range body["Env"].([]any) {
		environment[entry.(string)] = true
	}
	for _, want := range []string{"NCCL_IB_HCA=rocep1s0f0", "NCCL_IB_GID_INDEX=3", "NCCL_SOCKET_IFNAME=enp1s0f0np0", "GLOO_SOCKET_IFNAME=enp1s0f0np0", "TP_SOCKET_IFNAME=enp1s0f0np0"} {
		if !environment[want] {
			t.Fatalf("interconnect environment missing %s", want)
		}
	}
	// The per-role block wins over the shared one.
	if !environment["NCCL_DEBUG=INFO"] || environment["NCCL_DEBUG=WARN"] {
		t.Fatalf("worker environment did not override the shared value: %#v", environment)
	}
	if body["Labels"].(map[string]any)[labelNodeRole] != RoleWorker {
		t.Fatalf("container is not labelled with its rank: %#v", body["Labels"])
	}
}

// recordingExecutor is the local half of a fleet executor.
type recordingExecutor struct {
	calls []string
	fail  string
}

func (e *recordingExecutor) ArtifactPath(r recipe.Recipe) string { return "/local/" + r.ID }
func (e *recordingExecutor) RuntimeImageBytes(context.Context, recipe.Recipe) (int64, bool) {
	return 0, false
}
func (e *recordingExecutor) Execute(_ context.Context, _ Execution, op recipe.Operation, _ recipe.Recipe, _ Progress) (map[string]any, error) {
	e.calls = append(e.calls, op.Type)
	if op.Type == e.fail {
		return nil, errors.New("local step failed")
	}
	return map[string]any{"operation": op.Type}, nil
}
func (e *recordingExecutor) Completed(context.Context, Execution, recipe.Operation, recipe.Recipe, json.RawMessage) bool {
	return false
}

func TestFleetExecutorRoutesAndNamesEveryStep(t *testing.T) {
	r := twoSparkRecipe(t)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer worker-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/internal/node/preflight":
			_, _ = io.WriteString(w, `{"ready":true,"checks":[{"operation":"verify_disk","ok":true}]}`)
		case "/api/v1/internal/node/step":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body["operation"] == "start_container" {
				_, _ = io.WriteString(w, `{"receipt":{"container_id":"worker-id"},"error":"the container exited during startup"}`)
				return
			}
			_, _ = io.WriteString(w, `{"receipt":{"operation":"`+body["operation"].(string)+`"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer peer.Close()

	local := &recordingExecutor{}
	target := PeerTarget{ID: "peer_1", Name: "spark-b", BaseURL: peer.URL, APIKey: "worker-key"}
	fleet := NewFleetExecutor(local, NewPeerClient(func(context.Context) (PeerTarget, error) { return target, nil }))
	fleet.localAddress = func(string) (string, error) { return "169.254.10.1", nil }
	fleet.hostname = func() string { return "spark-a" }

	deployment, err := fleet.Plan(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	head, worker := deployment.Head, deployment.Worker
	if head.MasterAddress != "169.254.10.1" || worker.MasterAddress != "169.254.10.1" || worker.NodeName != "spark-b" || worker.Rank() != 1 {
		t.Fatalf("placements are wrong: %#v %#v", head, worker)
	}
	if deployment.Peer.ID != "peer_1" || deployment.Peer.APIKey != "worker-key" {
		t.Fatalf("the peer was not pinned when the job was planned: %#v", deployment.Peer)
	}
	pinned := deployment.Peer

	receipt, err := fleet.Execute(context.Background(), Execution{Placement: worker, Peer: &pinned}, recipe.Operation{Type: VerifyPeerNode}, r, nil)
	if err != nil {
		t.Fatalf("peer preflight: %v", err)
	}
	if receipt["node"] != "spark-b" || receipt["node_role"] != RoleWorker {
		t.Fatalf("peer preflight receipt does not name the node: %#v", receipt)
	}

	receipt, err = fleet.Execute(context.Background(), Execution{Placement: worker, Peer: &pinned}, recipe.Operation{Type: "pull_image"}, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if receipt["operation"] != "pull_image" || receipt["node"] != "spark-b" {
		t.Fatalf("worker step receipt is wrong: %#v", receipt)
	}
	if len(local.calls) != 0 {
		t.Fatalf("worker work ran locally: %v", local.calls)
	}

	receipt, err = fleet.Execute(context.Background(), Execution{Placement: head}, recipe.Operation{Type: "pull_image"}, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if receipt["node"] != "spark-a" || receipt["node_role"] != RoleHead || len(local.calls) != 1 {
		t.Fatalf("head step did not run locally and name its node: %#v %v", receipt, local.calls)
	}

	if _, err := fleet.Execute(context.Background(), Execution{Placement: worker, Peer: &pinned}, recipe.Operation{Type: "start_container"}, r, nil); err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("a failed worker step must surface its reason, got %v", err)
	}
}

func TestFleetExecutorLeavesSingleSparkWorkAlone(t *testing.T) {
	recipes, _ := recipe.Builtin()
	single, _ := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	local := &recordingExecutor{}
	fleet := NewFleetExecutor(local, NewPeerClient(func(context.Context) (PeerTarget, error) {
		t.Fatal("a single-Spark recipe must never consult a peer")
		return PeerTarget{}, nil
	}))
	deployment, err := fleet.Plan(context.Background(), single)
	if err != nil || deployment.Distributed() {
		t.Fatalf("single-Spark deployment: %#v %v", deployment, err)
	}
	receipt, err := fleet.Execute(context.Background(), Execution{}, recipe.Operation{Type: "pull_image"}, single, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, named := receipt["node"]; named {
		t.Fatalf("single-Spark receipt grew a node field: %#v", receipt)
	}
}

func TestPlacementsRefuseWhenNoWorkerIsConfigured(t *testing.T) {
	r := twoSparkRecipe(t)
	fleet := NewFleetExecutor(&recordingExecutor{}, NewPeerClient(func(context.Context) (PeerTarget, error) {
		return PeerTarget{}, errors.New("add the other Spark under Fleet first")
	}))
	if _, err := fleet.Plan(context.Background(), r); err == nil || !strings.Contains(err.Error(), "Fleet first") {
		t.Fatalf("got %v, want a refusal naming the missing peer", err)
	}
}
