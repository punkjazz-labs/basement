package fleet

import (
	"context"
	"errors"
	"net/http"

	"github.com/punkjazz-labs/basement/internal/store"
)

// The GPU power mode over the fleet plane. It is the smallest member operation
// there is: one word travels, the Spark that receives it writes the word down
// and asks its own GPU for it, and no recipe, deployment, job or serving
// container is touched at either end. A model that is streaming while the mode
// changes keeps streaming, and the cap applies to it live. That is the point
// of the feature rather than a thing to guard against.
//
// Reading a member's mode has no call here at all. Every node reports its own
// mode in the heartbeat the controller already collects (see heartbeat.go), so
// the fleet dashboard renders every row from what it has.

// PowerRuntime is the node-local boundary around this Spark's own power mode,
// the way IndependentRuntime is the boundary around placement. The fleet
// package never speaks to a GPU: it carries the owner's choice to the machine
// that owns one, and that machine stays the authority for what its driver
// accepts.
type PowerRuntime interface {
	PowerMode(context.Context) (store.PowerMode, error)
	SetPowerMode(context.Context, string) (store.PowerMode, error)
}

type powerModeRequest struct {
	Mode string `json:"mode"`
}

// errPowerRuntimeMissing is the honest answer while a manager is wired but its
// power boundary is not. It reads after "<name> could not do this:" as well as
// on its own, so it names a part of the Spark rather than the Spark again.
var errPowerRuntimeMissing = errors.New("the manager on this Spark is not ready to change its power mode")

// SetNodePowerMode puts one Spark of this fleet into one power mode. The local
// node answers from its own runtime; every other node answers over the same
// pinned mutual TLS transport placement uses. A failure comes back as the
// sentence the console shows, with the Spark named as its owner named it.
func (m *Manager) SetNodePowerMode(ctx context.Context, nodeID, mode string) (store.PowerMode, error) {
	normalized, err := store.NormalizePowerMode(mode)
	if err != nil {
		return store.PowerMode{}, err
	}
	target, local, err := m.placementNode(ctx, nodeID)
	if err != nil {
		return store.PowerMode{}, m.nodeFailure(ctx, nodeID, err)
	}
	if local {
		runtime := m.powerRuntime()
		if runtime == nil {
			return store.PowerMode{}, m.nodeFailure(ctx, nodeID, errPowerRuntimeMissing)
		}
		state, err := runtime.SetPowerMode(ctx, normalized)
		if err != nil {
			return store.PowerMode{}, m.nodeFailure(ctx, nodeID, err)
		}
		return state, nil
	}
	client, err := m.clientForNode(target)
	if err != nil {
		return store.PowerMode{}, m.nodeFailure(ctx, nodeID, err)
	}
	var state store.PowerMode
	if err := callFleetJSON(ctx, client, http.MethodPost, target.NodeURL+"/internal/fleet/v1/power-mode", powerModeRequest{Mode: normalized}, &state); err != nil {
		return store.PowerMode{}, m.nodeFailure(ctx, nodeID, err)
	}
	return state, nil
}

// powerMode is the member half. Only this node's adopted controller may ask,
// exactly as with every placement handler beside it, and the answer is this
// node's own setting as it stands after the change.
func (m *Manager) powerMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fleetMethodNotAllowed(w)
		return
	}
	if err := m.requireControllerCaller(r); err != nil {
		writeFleetError(w, http.StatusForbidden, err)
		return
	}
	var request powerModeRequest
	if err := decodeFleetBody(r, &request); err != nil {
		writeFleetError(w, http.StatusBadRequest, err)
		return
	}
	runtime := m.powerRuntime()
	if runtime == nil {
		writeFleetError(w, http.StatusServiceUnavailable, errPowerRuntimeMissing)
		return
	}
	state, err := runtime.SetPowerMode(r.Context(), request.Mode)
	if err != nil {
		writeFleetError(w, http.StatusConflict, err)
		return
	}
	writeFleetJSON(w, http.StatusOK, state)
}
