package operations

import (
	"context"
	"encoding/json"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

type Execution struct {
	JobID           string
	Kind            string
	RemoveArtifacts bool
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
