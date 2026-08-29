package operations

import (
	"context"
	"encoding/json"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

type Execution struct {
	JobID string
	// ReservationID excludes this job's own persistent disk claim from the
	// other-work total handed to verify_disk. It is process-local execution
	// context and is never sent to Docker or serialized into a receipt.
	ReservationID string
	// ReplacesRecipeID names the model this job takes the runtime slot from,
	// resolved once when the job was planned, exactly as the peer is. The head
	// already names it to its own allocator; a two-Spark job has to say it on
	// the worker's wire too, because the worker keeps an admission claim of its
	// own and cannot see this manager's installed models. It is filled only
	// when the plan also carries the stop for that model, so a rank is never
	// declared free on a machine where nothing is going to stop it.
	ReplacesRecipeID string
	Kind             string
	RemoveArtifacts  bool
	// SharedArtifacts holds artifact keys (repository@revision) and artifact
	// paths still referenced by other installed models; removal must retain
	// them instead of deleting shared data.
	SharedArtifacts map[string]bool
	// ReservedBytes is the sum of every other running install job's
	// conservative disk footprint (recipe.Recipe.RequiredBytes). verify_disk
	// subtracts it from free space so two concurrent installs cannot both
	// pass preflight and jointly overflow the disk.
	ReservedBytes int64
	// Placement names this step's node in a two-Spark serve. Its zero value
	// means single-node serving, which is what every shipped recipe uses.
	Placement Placement
	// Peer is the worker Spark the step this Execution describes was planned
	// against, resolved once when the job was planned so a later step (a
	// teardown especially) can never act on a machine that was configured
	// after the job started. A job that switches away from a distributed
	// model carries two resolved peers - its own, if it is itself
	// distributed, and the model it is replacing's - and each step is pinned
	// to whichever one it belongs to. It is nil for single-node work and is
	// never serialized anywhere.
	Peer *PeerTarget
}

// Node roles in a distributed serve. The head runs the job, serves HTTP and
// is rank 0; the worker is rank 1 and serves nothing.
const (
	RoleHead   = "head"
	RoleWorker = "worker"
)

// Placement is a node's part in a distributed serve: which rank it is, what
// it is called in receipts, and where rank 0 listens for the other rank.
type Placement struct {
	Role     string `json:"role"`
	NodeName string `json:"node"`
	// PeerID identifies the worker this placement was resolved against. The
	// credential never travels with it.
	PeerID        string `json:"peer_id,omitempty"`
	NodeCount     int    `json:"node_count"`
	MasterAddress string `json:"master_address,omitempty"`
	MasterPort    int    `json:"master_port,omitempty"`
}

func (p Placement) Distributed() bool { return p.Role == RoleHead || p.Role == RoleWorker }

// Rank is the --node-rank this placement launches with. The community
// two-Spark recipe fixes the head at 0 and the single worker at 1.
func (p Placement) Rank() int {
	if p.Role == RoleWorker {
		return 1
	}
	return 0
}

type Progress func(receipt any) error

type Executor interface {
	Execute(context.Context, Execution, recipe.Operation, recipe.Recipe, Progress) (map[string]any, error)
	Completed(context.Context, Execution, recipe.Operation, recipe.Recipe, json.RawMessage) bool
	ArtifactPath(recipe.Recipe) string
	// RuntimeImageBytes reports the on-disk size of the recipe's pinned
	// runtime image when Docker holds it locally, for storage accounting.
	RuntimeImageBytes(context.Context, recipe.Recipe) (int64, bool)
}

// ManagedContainerLister is the optional executor capability the engine uses
// at switch time to see which basement-labeled containers this node's Docker
// daemon actually holds, instead of trusting the store's active-model pointer
// alone. A failed switch that was rolled back at the Docker level can leave
// the store pointing at a model that is not the one whose container is
// running, and a later switch planned from the store alone would then start a
// second model beside the first and run the machine out of memory. Executors
// that cannot see a Docker daemon (unit-test fakes) simply do not implement
// this, and the engine plans from the store as before.
type ManagedContainerLister interface {
	ManagedContainers(context.Context) ([]ManagedContainer, error)
}
