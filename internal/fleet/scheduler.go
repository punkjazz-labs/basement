package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

type PlacementCandidate struct {
	NodeID       string         `json:"node_id"`
	DisplayName  string         `json:"display_name"`
	Eligible     bool           `json:"eligible"`
	Reason       string         `json:"reason,omitempty"`
	CurrentModel *ModelSnapshot `json:"current_model,omitempty"`
}

type PlacementPlan struct {
	RecipeID          string               `json:"recipe_id"`
	RecipeVersion     int                  `json:"recipe_version"`
	RecipeFingerprint string               `json:"recipe_fingerprint"`
	Candidates        []PlacementCandidate `json:"candidates"`
	RecommendedNodeID string               `json:"recommended_node_id,omitempty"`
	selected          recipe.Recipe
}

func (m *Manager) PlanIndependent(ctx context.Context, recipeID string) (PlacementPlan, error) {
	if err := m.requireFleetMutationAllowed(ctx); err != nil {
		return PlacementPlan{}, err
	}
	selected, ok := recipe.Find(m.effectiveRecipes(), recipeID)
	if !ok {
		return PlacementPlan{}, errors.New("the model is not in this controller's current catalogue")
	}
	if selected.Topology.SparkCount != 1 {
		return PlacementPlan{}, fmt.Errorf("%s requires %d nodes and cannot use independent placement", selected.DisplayName, selected.Topology.SparkCount)
	}
	fingerprint, err := RecipeFingerprint(selected)
	if err != nil {
		return PlacementPlan{}, err
	}
	summary, err := m.Summary(ctx)
	if err != nil {
		return PlacementPlan{}, err
	}
	if summary.Role == "member" {
		return PlacementPlan{}, errors.New("a fleet member cannot issue placement grants")
	}
	plan := PlacementPlan{RecipeID: selected.ID, RecipeVersion: selected.Version, RecipeFingerprint: fingerprint, Candidates: []PlacementCandidate{}, selected: selected}
	for _, node := range summary.Nodes {
		if len(plan.Candidates) == store.MaxFleetNodes {
			break
		}
		candidate := PlacementCandidate{NodeID: node.NodeID, DisplayName: node.DisplayName, Eligible: true}
		for index := range node.InstalledModels {
			if node.InstalledModels[index].Active {
				current := node.InstalledModels[index]
				candidate.CurrentModel = &current
				break
			}
		}
		switch {
		case node.Status != "fresh":
			candidate.Eligible, candidate.Reason = false, "the node is "+node.Status+" and cannot accept a placement"
		case node.ManagerVersion != m.version:
			candidate.Eligible, candidate.Reason = false, "the node manager version does not exactly match the controller"
		case node.ManagerBuildIdentity != m.buildIdentity:
			candidate.Eligible, candidate.Reason = false, "the node build identity does not exactly match the controller"
		case node.CatalogueDigest != m.digest():
			candidate.Eligible, candidate.Reason = false, "the node recipe catalogue does not exactly match the controller"
		}
		if candidate.Eligible && plan.RecommendedNodeID == "" {
			plan.RecommendedNodeID = candidate.NodeID
		}
		plan.Candidates = append(plan.Candidates, candidate)
	}
	return plan, nil
}

func (plan PlacementPlan) candidate(nodeID string) (PlacementCandidate, error) {
	for _, candidate := range plan.Candidates {
		if candidate.NodeID != nodeID {
			continue
		}
		if !candidate.Eligible {
			return PlacementCandidate{}, errors.New(candidate.Reason)
		}
		return candidate, nil
	}
	return PlacementCandidate{}, errors.New("the selected node is not in this fleet")
}
