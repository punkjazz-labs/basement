package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// VerifyPeerNode is an engine-generated step, never a recipe operation: the
// head asks the worker manager to run its own preflight and refuses to go
// further unless the worker reports ready. ADR 0004's per-node evaluation is
// satisfied by each node running its own guardrails, never by aggregating.
const VerifyPeerNode = "verify_peer_node"

// Fleet is implemented by an executor that can also run steps on a second
// Spark. The engine asks for it only when a recipe is distributed, so a
// manager wired with a single-node executor keeps working unchanged.
type Fleet interface {
	Plan(ctx context.Context, r recipe.Recipe) (Deployment, error)
}

// Deployment is the set of nodes one model runs on, resolved once when a job
// is planned. Peer is pinned for the whole job on purpose: re-resolving the
// worker per step would send a later teardown to whatever peer happens to be
// configured by then rather than to the machine this job actually started a
// rank on.
type Deployment struct {
	Head   Placement
	Worker Placement
	Peer   PeerTarget
}

func (d Deployment) Distributed() bool { return d.Head.Distributed() }

// PeerTarget is the one configured worker Spark: where it is and the API key
// this manager authenticates to it with. The key is never put in a receipt.
type PeerTarget struct {
	ID      string
	Name    string
	BaseURL string
	APIKey  string
}

// PeerDirectory resolves the single peer to use as the worker node. It must
// fail rather than choose when zero or more than one peer is configured.
type PeerDirectory func(ctx context.Context) (PeerTarget, error)

// PeerRunner runs work on the worker Spark through its manager's API. Step
// takes a progress callback because a forwarded download or image pull is as
// long as a local one, and the console has no other way to see it move.
type PeerRunner interface {
	Target(ctx context.Context) (PeerTarget, error)
	Preflight(ctx context.Context, target PeerTarget, execution Execution, r recipe.Recipe) (map[string]any, error)
	Step(ctx context.Context, target PeerTarget, execution Execution, op recipe.Operation, r recipe.Recipe, progress Progress) (map[string]any, error)
}

// FleetExecutor routes each step to the node its placement names: head (and
// every single-node recipe) runs locally, worker runs through the peer
// manager's API. Every receipt it returns names the node that produced it.
type FleetExecutor struct {
	local Executor
	peer  PeerRunner
	// localAddress resolves this node's IPv4 address on a named interface.
	// Injectable so tests never depend on the host's real interfaces.
	localAddress func(string) (string, error)
	hostname     func() string
}

func NewFleetExecutor(local Executor, peer PeerRunner) *FleetExecutor {
	return &FleetExecutor{local: local, peer: peer, localAddress: FabricAddress, hostname: localHostname}
}

func localHostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "this Spark"
	}
	return name
}

// FabricAddress is the IPv4 address this node holds on the named interface.
// It becomes --master-addr for both ranks. The name comes from resolveFabric:
// the detected cabled port, or the recipe's pinned fallback.
func FabricAddress(name string) (string, error) {
	if name == "" {
		return "", errors.New("the recipe does not name an interconnect interface")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("interconnect interface %s was not found on this Spark: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("read addresses of interconnect interface %s: %w", name, err)
	}
	for _, addr := range addrs {
		network, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ipv4 := network.IP.To4(); ipv4 != nil {
			return ipv4.String(), nil
		}
	}
	return "", fmt.Errorf("interconnect interface %s has no IPv4 address, so the other Spark has nothing to connect to", name)
}

// Plan resolves both nodes of a distributed serve, and the peer they will be
// driven through, once. A single-node recipe gets a zero deployment, which
// every step then treats as local.
func (f *FleetExecutor) Plan(ctx context.Context, r recipe.Recipe) (Deployment, error) {
	if !r.Distributed() {
		return Deployment{}, nil
	}
	if r.Topology.Interconnect == nil {
		return Deployment{}, errors.New("this recipe needs two Sparks but does not describe the interconnect")
	}
	target, err := f.peer.Target(ctx)
	if err != nil {
		return Deployment{}, err
	}
	link, err := resolveFabric(r)
	if err != nil {
		return Deployment{}, err
	}
	address, err := f.localAddress(link.NetDev)
	if err != nil {
		return Deployment{}, err
	}
	port := r.Topology.Interconnect.MasterPort
	return Deployment{
		Head:   Placement{Role: RoleHead, NodeName: f.hostname(), NodeCount: r.Topology.SparkCount, MasterAddress: address, MasterPort: port},
		Worker: Placement{Role: RoleWorker, NodeName: target.Name, PeerID: target.ID, NodeCount: r.Topology.SparkCount, MasterAddress: address, MasterPort: port},
		Peer:   target,
	}, nil
}

// Local is the executor that runs work on this machine. A manager acting as
// a worker must reach it: running a worker-placed step back through the
// fleet executor would forward it to a peer again instead of executing it.
func (f *FleetExecutor) Local() Executor { return f.local }

func (f *FleetExecutor) ArtifactPath(r recipe.Recipe) string { return f.local.ArtifactPath(r) }

func (f *FleetExecutor) RuntimeImageBytes(ctx context.Context, r recipe.Recipe) (int64, bool) {
	return f.local.RuntimeImageBytes(ctx, r)
}

func (f *FleetExecutor) Execute(ctx context.Context, execution Execution, op recipe.Operation, r recipe.Recipe, progress Progress) (map[string]any, error) {
	if op.Type == VerifyPeerNode || execution.Placement.Role == RoleWorker {
		target, err := pinnedPeer(execution)
		if err != nil {
			return nil, err
		}
		if op.Type == VerifyPeerNode {
			receipt, err := f.peer.Preflight(ctx, target, execution, r)
			return named(receipt, execution.Placement), err
		}
		receipt, err := f.peer.Step(ctx, target, execution, op, r, namedProgress(progress, execution.Placement))
		return named(receipt, execution.Placement), err
	}
	receipt, err := f.local.Execute(ctx, execution, op, r, namedProgress(progress, execution.Placement))
	return named(receipt, execution.Placement), err
}

// Completed re-runs every worker step rather than trusting a remote
// already-satisfied answer. The worker steps are idempotent (downloads
// resume and verify, container creation is name-keyed), so repeating one
// after a resume is cheap and always safer than assuming.
func (f *FleetExecutor) Completed(ctx context.Context, execution Execution, op recipe.Operation, r recipe.Recipe, receipt json.RawMessage) bool {
	if op.Type == VerifyPeerNode || execution.Placement.Role == RoleWorker {
		return false
	}
	return f.local.Completed(ctx, execution, op, r, receipt)
}

// pinnedPeer is the worker this job was planned against. A step that reaches
// here without one would otherwise be free to act on whichever peer is
// configured right now, which is not necessarily the machine holding the
// rank this step is about.
func pinnedPeer(execution Execution) (PeerTarget, error) {
	if execution.Peer == nil || execution.Peer.BaseURL == "" {
		return PeerTarget{}, errors.New("the other Spark was not pinned when this job was planned, so it cannot be acted on")
	}
	return *execution.Peer, nil
}

// named stamps the node a receipt came from. Distributed jobs write two
// receipts per operation, and a timeline that cannot tell them apart is
// worse than no timeline.
func named(receipt map[string]any, placement Placement) map[string]any {
	if !placement.Distributed() {
		return receipt
	}
	if receipt == nil {
		receipt = map[string]any{}
	}
	receipt["node"] = placement.NodeName
	receipt["node_role"] = placement.Role
	return receipt
}

// namedProgress stamps the node onto every live receipt as well. Both Sparks
// run the same operation in a distributed job, so a bar carrying no node
// cannot say which machine is moving the bytes it is drawing.
func namedProgress(progress Progress, placement Placement) Progress {
	if progress == nil || !placement.Distributed() {
		return progress
	}
	return func(receipt any) error {
		values, ok := receipt.(map[string]any)
		if !ok {
			return progress(receipt)
		}
		return progress(named(values, placement))
	}
}

// PeerClient calls the worker manager's internal node endpoints. The peer's
// response is untrusted input: it is size-capped, only ever parsed as JSON,
// and redirects are refused so a hostile "peer" cannot bounce an
// authenticated call somewhere the user never approved.
type PeerClient struct {
	directory PeerDirectory
	client    *http.Client
}

func NewPeerClient(directory PeerDirectory) *PeerClient {
	return &PeerClient{directory: directory, client: &http.Client{
		Timeout:       0,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (p *PeerClient) Target(ctx context.Context) (PeerTarget, error) { return p.directory(ctx) }

func (p *PeerClient) Preflight(ctx context.Context, target PeerTarget, execution Execution, r recipe.Recipe) (map[string]any, error) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var response struct {
		Ready  bool             `json:"ready"`
		Checks []map[string]any `json:"checks"`
		Error  string           `json:"error"`
	}
	if err := p.post(callCtx, target, "/api/v1/internal/node/preflight", map[string]any{"recipe": r, "job_id": execution.JobID}, &response); err != nil {
		return nil, fmt.Errorf("the other Spark could not be asked to check itself: %w", err)
	}
	receipt := map[string]any{"peer": target.Name, "ready": response.Ready, "checks": response.Checks}
	if !response.Ready {
		return receipt, fmt.Errorf("the other Spark is not ready to run this model: %s", failedCheckSummary(response.Checks))
	}
	return receipt, nil
}

func failedCheckSummary(checks []map[string]any) string {
	for _, check := range checks {
		if ok, _ := check["ok"].(bool); ok {
			continue
		}
		operation, _ := check["operation"].(string)
		message, _ := check["error"].(string)
		if message == "" {
			message = "check did not pass"
		}
		return operation + ": " + message
	}
	return "no check reported a reason"
}

// peerProgressInterval paces how often the head asks a worker how far its
// running step has got. The worker answers a step call only once it is
// finished, so polling is the only channel a forwarded download has; two
// seconds keeps the console moving without adding load to a busy node.
var peerProgressInterval = 2 * time.Second

// peerProgressTimeout bounds one poll, so a worker that stops answering
// costs the next poll and nothing more. The step it describes is never
// failed by a poll that does not come back.
const peerProgressTimeout = 10 * time.Second

func (p *PeerClient) Step(ctx context.Context, target PeerTarget, execution Execution, op recipe.Operation, r recipe.Recipe, progress Progress) (map[string]any, error) {
	// Weight downloads and image pulls on the worker take as long as they do
	// on the head, so this call is bounded by the job's own context.
	var response struct {
		Receipt map[string]any `json:"receipt"`
		Error   string         `json:"error"`
	}
	body := map[string]any{
		"operation":        op.Type,
		"recipe":           r,
		"placement":        execution.Placement,
		"remove_artifacts": execution.RemoveArtifacts,
		"job_id":           execution.JobID,
	}
	if progress != nil {
		following, stopFollowing := context.WithCancel(ctx)
		followed := make(chan struct{})
		go func() {
			defer close(followed)
			p.follow(following, target, execution, op, progress)
		}()
		// The follower has to be finished before this returns: the caller
		// records the step's final receipt next, and a poll still in flight
		// would overwrite it with a stale one.
		defer func() {
			stopFollowing()
			<-followed
		}()
	}
	if err := p.post(ctx, target, "/api/v1/internal/node/step", body, &response); err != nil {
		return nil, fmt.Errorf("%s on the other Spark failed: %w", op.Type, err)
	}
	if response.Error != "" {
		return response.Receipt, fmt.Errorf("%s on the other Spark failed: %s", op.Type, response.Error)
	}
	return response.Receipt, nil
}

// follow republishes the worker's own view of the step it is running right
// now as this step's live progress. The worker holds that receipt in memory
// for as long as the step runs and nothing longer, so this can never
// resurrect progress from a step that already finished, and a peer that
// cannot answer at all simply leaves the step without live detail rather
// than failing it.
func (p *PeerClient) follow(ctx context.Context, target PeerTarget, execution Execution, op recipe.Operation, progress Progress) {
	ticker := time.NewTicker(peerProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var status struct {
			Operation string         `json:"operation"`
			Running   bool           `json:"running"`
			Receipt   map[string]any `json:"receipt"`
		}
		pollCtx, cancel := context.WithTimeout(ctx, peerProgressTimeout)
		polled := p.post(pollCtx, target, "/api/v1/internal/node/step/progress", map[string]any{"job_id": execution.JobID}, &status)
		cancel()
		if polled != nil || !status.Running || status.Operation != op.Type || len(status.Receipt) == 0 {
			continue
		}
		if err := progress(status.Receipt); err != nil {
			return
		}
	}
}

func (p *PeerClient) post(ctx context.Context, target PeerTarget, endpoint string, body any, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.BaseURL+endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+target.APIKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("the other Spark returned an unreadable response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return fmt.Errorf("the other Spark returned status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("the other Spark returned an unreadable response: %w", err)
	}
	return nil
}
