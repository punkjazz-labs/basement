package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if _, err := client.Create(context.Background(), "worker", r.Runtime.Reference(), []string{"/managed/model"}, "/managed/cache", nil, r, worker); err != nil {
		t.Fatal(err)
	}
	hostConfig := body["HostConfig"].(map[string]any)
	if hostConfig["NetworkMode"] != "host" {
		t.Fatalf("distributed container is not on the host fabric: %#v", hostConfig)
	}
	// ADR 0006: the fabric is host networking and RDMA, not a shared IPC
	// namespace. The container keeps its own /dev/shm, sized from the recipe,
	// so shm_bytes is a real boundary for a two-Spark model too.
	if mode, shared := hostConfig["IpcMode"]; shared {
		t.Fatalf("distributed container asked for IPC mode %v; it must keep its own namespace", mode)
	}
	if shm, _ := hostConfig["ShmSize"].(float64); int64(shm) != r.Runtime.ShmBytes {
		t.Fatalf("ShmSize = %v, want the recipe's shm_bytes %d", hostConfig["ShmSize"], r.Runtime.ShmBytes)
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

// fabricPeer is a worker manager answering only the cable endpoint. body is
// written verbatim so a test can hand the head a real listener, a reported
// failure, or nonsense.
func fabricPeer(t *testing.T, body func() string) PeerTarget {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer worker-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.URL.Path != "/api/v1/internal/node/fabric" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body())
	}))
	t.Cleanup(server.Close)
	return PeerTarget{ID: "peer_1", Name: "spark-b", BaseURL: server.URL, APIKey: "worker-key"}
}

func fabricFleet(t *testing.T, headAddress string) *FleetExecutor {
	t.Helper()
	fleet := NewFleetExecutor(&recordingExecutor{}, NewPeerClient(func(context.Context) (PeerTarget, error) {
		return PeerTarget{}, errors.New("the peer is pinned by the test")
	}))
	fleet.localAddress = func(string) (string, error) { return headAddress, nil }
	fleet.hostname = func() string { return "spark-a" }
	return fleet
}

// The passing case, end to end through the peer API: the worker opens a
// listener on its own fabric address, the head dials it from its own, and the
// receipt records both ports, both addresses and the round trip.
func TestVerifyFabricProvesTheLinkAndRecordsBothEnds(t *testing.T) {
	r := twoSparkRecipe(t)
	withFabric(t, FabricLink{NetDev: "enp1s0f1np1", HCA: "rocep1s0f1"}, nil, "127.0.0.1", nil)
	target := fabricPeer(t, func() string {
		probe, err := ServeFabricProbe(r)
		if err != nil {
			t.Error(err)
			return `{"error":"probe failed"}`
		}
		encoded, _ := json.Marshal(probe)
		return string(encoded)
	})

	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2}
	receipt, err := fabricFleet(t, "127.0.0.1").Execute(context.Background(), Execution{Placement: head, Peer: &target}, recipe.Operation{Type: VerifyFabric}, r, nil)
	if err != nil {
		t.Fatalf("the cable check failed: %v", err)
	}
	for key, want := range map[string]any{
		"head_interface": "enp1s0f1np1", "head_address": "127.0.0.1", "head_hca": "rocep1s0f1",
		"worker_node": "spark-b", "worker_interface": "enp1s0f1np1", "worker_address": "127.0.0.1",
		"reachable": true, "node": "spark-a", "node_role": RoleHead,
	} {
		if receipt[key] != want {
			t.Errorf("receipt[%s] = %v, want %v", key, receipt[key], want)
		}
	}
	if _, timed := receipt["round_trip_ms"]; !timed {
		t.Errorf("the receipt does not record the round trip: %#v", receipt)
	}
}

// The worker's port is up but holds no address, so there is nothing to dial.
// The head relays what the worker said and names which Spark said it.
func TestVerifyFabricRelaysAWorkerWithNoAddress(t *testing.T) {
	r := twoSparkRecipe(t)
	withFabric(t, FabricLink{NetDev: "enp1s0f1np1"}, nil, "127.0.0.1", nil)
	target := fabricPeer(t, func() string {
		return `{"error":"high-speed port enp1s0f0np0 has no address, so the two Sparks have nothing to meet on"}`
	})

	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2}
	receipt, err := fabricFleet(t, "127.0.0.1").Execute(context.Background(), Execution{Placement: head, Peer: &target}, recipe.Operation{Type: VerifyFabric}, r, nil)
	if err == nil || !strings.Contains(err.Error(), "the other Spark could not join the cable: high-speed port enp1s0f0np0 has no address") {
		t.Fatalf("got %v, want the worker's own reason relayed", err)
	}
	if receipt["head_address"] != "127.0.0.1" || receipt["worker_node"] != "spark-b" {
		t.Errorf("a failed check must still record what was known: %#v", receipt)
	}
}

// Both ends lit, no path between them. This is the failure the cable check
// exists for, and it must not read as anything else.
func TestVerifyFabricSaysWhenTheTwoPortsCannotMeet(t *testing.T) {
	r := twoSparkRecipe(t)
	withFabric(t, FabricLink{NetDev: "enp1s0f1np1"}, nil, "127.0.0.1", nil)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	target := fabricPeer(t, func() string {
		return fmt.Sprintf(`{"interface":"enP2p1s0f1np1","address":"127.0.0.1","port":%d,"token":"deadbeef"}`, port)
	})

	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2}
	receipt, err := fabricFleet(t, "127.0.0.1").Execute(context.Background(), Execution{Placement: head, Peer: &target}, recipe.Operation{Type: VerifyFabric}, r, nil)
	if err == nil || !strings.Contains(err.Error(), "cable is connected on both Sparks but their high-speed ports cannot reach each other") {
		t.Fatalf("got %v, want the unreachable-ports sentence", err)
	}
	if receipt["reachable"] != false || receipt["worker_interface"] != "enP2p1s0f1np1" {
		t.Errorf("a failed check must record both ends and the result: %#v", receipt)
	}
}

// No cable on the head at all: nothing is asked of the worker, and the owner
// is told to connect the cable rather than shown an interface name.
func TestVerifyFabricAsksForTheCableBeforeCallingTheWorker(t *testing.T) {
	r := twoSparkRecipe(t)
	withFabric(t, FabricLink{}, errors.New("no high-speed port has link (rocep1s0f0/enp1s0f0np0: no link); connect the cable between the two Sparks"), "", nil)
	target := fabricPeer(t, func() string {
		t.Error("the worker was called even though this Spark has no cable")
		return `{}`
	})
	fleet := NewFleetExecutor(&recordingExecutor{}, NewPeerClient(func(context.Context) (PeerTarget, error) {
		return PeerTarget{}, errors.New("the peer is pinned by the test")
	}))
	fleet.localAddress = func(string) (string, error) {
		return "", errors.New("high-speed port enp1s0f0np0 has no address, so the two Sparks have nothing to meet on")
	}

	head := Placement{Role: RoleHead, NodeName: "spark-a", NodeCount: 2}
	_, err := fleet.Execute(context.Background(), Execution{Placement: head, Peer: &target}, recipe.Operation{Type: VerifyFabric}, r, nil)
	if err == nil || !strings.Contains(err.Error(), "connect the cable between the two Sparks") {
		t.Fatalf("got %v, want the cable request", err)
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

// TestWorkerStepProgressReachesTheConsole is the two-Spark half of the
// download and image-pull progress the console shows: the worker's step call
// only answers when the step is over, so the head has to poll for what is
// happening in between and fold it into the same progress channel a local
// step uses.
func TestWorkerStepProgressReachesTheConsole(t *testing.T) {
	previous := peerProgressInterval
	t.Cleanup(func() { peerProgressInterval = previous })
	peerProgressInterval = time.Millisecond

	r := twoSparkRecipe(t)
	release := make(chan struct{})
	var polls atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/internal/node/step":
			<-release
			_, _ = io.WriteString(w, `{"receipt":{"artifacts":[{"bytes_verified":900}]}}`)
		case "/api/v1/internal/node/step/progress":
			if body["job_id"] != "job-1" {
				t.Errorf("the worker was polled for the wrong job: %#v", body)
			}
			count := polls.Add(1)
			_, _ = fmt.Fprintf(w, `{"operation":"download_artifact","running":true,"receipt":{"bytes_complete":%d,"bytes_total":900}}`, count*100)
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
	pinned := deployment.Peer

	var seen []map[string]any
	var mu sync.Mutex
	progress := func(receipt any) error {
		mu.Lock()
		defer mu.Unlock()
		values, ok := receipt.(map[string]any)
		if !ok {
			t.Errorf("live receipt is %T, want a map", receipt)
			return nil
		}
		seen = append(seen, values)
		// Exactly two, not at-least: a third poll can land before the
		// follower notices the release, and this channel closes once.
		if len(seen) == 2 {
			close(release)
		}
		return nil
	}
	execution := Execution{JobID: "job-1", Placement: deployment.Worker, Peer: &pinned}
	receipt, err := fleet.Execute(context.Background(), execution, recipe.Operation{Type: "download_artifact"}, r, progress)
	if err != nil {
		t.Fatal(err)
	}
	if receipt["node"] != "spark-b" {
		t.Fatalf("final worker receipt does not name the node: %#v", receipt)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("the worker step reported %d live receipts, want the polled ones", len(seen))
	}
	for index, update := range seen {
		if update["bytes_total"] != float64(900) || update["bytes_complete"] == nil {
			t.Fatalf("live receipt %d lost the worker's own numbers: %#v", index, update)
		}
		// Without the node, the console cannot say which Spark is downloading.
		if update["node"] != "spark-b" || update["node_role"] != RoleWorker {
			t.Fatalf("live receipt %d does not name the node: %#v", index, update)
		}
	}
}

func TestHeadStepProgressIsNamedToo(t *testing.T) {
	r := twoSparkRecipe(t)
	local := &progressingExecutor{}
	fleet := NewFleetExecutor(local, NewPeerClient(func(context.Context) (PeerTarget, error) {
		return PeerTarget{ID: "peer_1", Name: "spark-b", BaseURL: "http://127.0.0.1:1", APIKey: "k"}, nil
	}))
	fleet.localAddress = func(string) (string, error) { return "169.254.10.1", nil }
	fleet.hostname = func() string { return "spark-a" }
	deployment, err := fleet.Plan(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var seen []map[string]any
	_, err = fleet.Execute(context.Background(), Execution{JobID: "job-1", Placement: deployment.Head}, recipe.Operation{Type: "download_artifact"}, r, func(receipt any) error {
		seen = append(seen, receipt.(map[string]any))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0]["node"] != "spark-a" || seen[0]["node_role"] != RoleHead {
		t.Fatalf("head live receipts do not name the node: %#v", seen)
	}

	// A single-Spark job must gain nothing at all from any of this.
	recipes, _ := recipe.Builtin()
	single, _ := recipe.Find(recipes, "qwen36-35b-a3b-nvfp4-1s")
	seen = nil
	if _, err := fleet.Execute(context.Background(), Execution{}, recipe.Operation{Type: "download_artifact"}, single, func(receipt any) error {
		seen = append(seen, receipt.(map[string]any))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("single-Spark progress count changed: %#v", seen)
	}
	if _, named := seen[0]["node"]; named {
		t.Fatalf("single-Spark live receipt grew a node field: %#v", seen[0])
	}
}

// progressingExecutor reports one live receipt before it finishes, the way a
// real download does.
type progressingExecutor struct{ recordingExecutor }

func (e *progressingExecutor) Execute(ctx context.Context, execution Execution, op recipe.Operation, r recipe.Recipe, progress Progress) (map[string]any, error) {
	if progress != nil {
		if err := progress(map[string]any{"bytes_complete": 10, "bytes_total": 100}); err != nil {
			return nil, err
		}
	}
	return e.recordingExecutor.Execute(ctx, execution, op, r, progress)
}
