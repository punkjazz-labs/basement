package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/punkjazz-labs/basement/internal/attached"
)

// SetAttachedServices wires the operator's attached-service declarations
// (ADR 0025). Called once at startup, before the listener starts. A manager
// without a registry answers with an empty list, which is what a Spark with
// no attached-services.d directory looks like too.
func (s *Server) SetAttachedServices(registry *attached.Registry) {
	s.attached = registry
}

// attachedNodeView is one Spark a declaration names, and whether this
// manager itself keeps its own models off it. Protected is false for a name
// this manager cannot match to a Spark it places models on, which is how a
// misspelt node name shows up in the console instead of passing silently.
type attachedNodeView struct {
	Name      string `json:"name"`
	NodeID    string `json:"node_id,omitempty"`
	Role      string `json:"role,omitempty"`
	ThisSpark bool   `json:"this_spark"`
	Protected bool   `json:"protected"`
}

type attachedServiceView struct {
	attached.Entry
	Nodes []attachedNodeView `json:"nodes"`
}

type attachedServicesResponse struct {
	Services []attachedServiceView `json:"services"`
	Errors   []attached.Problem    `json:"errors"`
}

// attachedServices lists every declared service with its live health, and
// every declaration file that was refused. It reads memory and the local
// store only; the health it reports is the prober's last answer, never a
// request made on the console's behalf.
func (s *Server) attachedServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	response := attachedServicesResponse{Services: []attachedServiceView{}, Errors: []attached.Problem{}}
	if s.attached == nil {
		writeJSON(w, http.StatusOK, response)
		return
	}
	snapshot := s.attached.Snapshot()
	response.Errors = append(response.Errors, snapshot.Problems...)
	if len(snapshot.Services) == 0 {
		writeJSON(w, http.StatusOK, response)
		return
	}
	places, err := s.placementTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, entry := range snapshot.Services {
		view := attachedServiceView{Entry: entry, Nodes: make([]attachedNodeView, 0, len(entry.Declaration.Topology.Nodes))}
		for _, node := range entry.Declaration.Topology.Nodes {
			item := attachedNodeView{Name: node.Label(), NodeID: node.NodeID, Role: node.Role}
			for _, place := range places {
				if node.Matches(place.id, place.name) {
					item.Protected = true
					item.ThisSpark = item.ThisSpark || place.self
				}
			}
			view.Nodes = append(view.Nodes, item)
		}
		response.Services = append(response.Services, view)
	}
	writeJSON(w, http.StatusOK, response)
}

type placementTarget struct {
	id, name string
	self     bool
}

// placementTargets lists the Sparks on which this manager itself refuses
// runtime claims for an attached service: this Spark, through its allocator,
// and, unless it is a fleet member, every Spark it can place models on, which
// are its fleet rows (through the placement planner) and its paired peer
// (through the two-Spark worker check and delegated installs). A member plans
// nothing, so it speaks only for itself.
func (s *Server) placementTargets(ctx context.Context) ([]placementTarget, error) {
	var places []placementTarget
	if s.fleetManager != nil {
		places = append(places, placementTarget{id: s.fleetManager.Identity().NodeID, name: s.fleetManager.DisplayName(), self: true})
	}
	config, err := s.store.FleetConfig(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if config.Role == "member" {
		return places, nil
	}
	nodes, err := s.store.FleetNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		places = append(places, placementTarget{id: node.NodeID, name: node.DisplayName})
	}
	peers, err := s.store.Peers(ctx)
	if err != nil {
		return nil, err
	}
	for _, peer := range peers {
		places = append(places, placementTarget{id: peer.ID, name: peer.Name})
	}
	return places, nil
}

// refuseHeldPeer refuses a delegated install on a paired Spark an attached
// service holds. That Spark's own allocator would refuse a serving claim only
// if the declaration were on it as well, so the head that holds the
// declaration says no before it asks.
func (s *Server) refuseHeldPeer(w http.ResponseWriter, id, name string) bool {
	if s.attached == nil {
		return false
	}
	holder, held := s.attached.Holder(id, name)
	if !held {
		return false
	}
	writeError(w, http.StatusConflict, errors.New("the attached service "+holder+" has "+name+" reserved outside Basement, so no model can be placed there; stop that service where it is managed and remove its declaration first"))
	return true
}
