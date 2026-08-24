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

// The plain words for a membership status word. The fleet writes these words
// for itself, and a word a machine keeps for its own bookkeeping is not a word
// to put in front of the person who owns it: "version-mismatch" says nothing
// to them. Only the words that need translating are here.
//
// A status this map does not hold is written exactly as it arrived. Some of
// them already read plainly, and a raw word is still the truth, while a
// guessed one would not be.
var plainStatusWords = map[string]string{
	"version-mismatch": "on another manager version",
	"stale":            "not answering recently",
	"joining":          "still joining",
}

func plainStatus(status string) string {
	if plain, ok := plainStatusWords[status]; ok {
		return plain
	}
	return status
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
			// Every reason here is read in two places: under the name of this
			// Spark in the install dialog, and after "<name> could not do
			// this:" when an install is sent to it all the same. So each one
			// names a part of the machine or the fleet, never the machine
			// itself, and no line ends up with two subjects. Every reason is
			// also a whole sentence, with a capital and a full stop, because
			// the "Run on" list has one register and this is it.
			candidate.Eligible, candidate.Reason = false, "The fleet shows this Spark as "+plainStatus(node.Status)+", so it cannot take a model now."
		case node.ManagerVersion != m.version:
			candidate.Eligible, candidate.Reason = false, nodeVersionSkew
		case node.ManagerBuildIdentity != m.buildIdentity:
			candidate.Eligible, candidate.Reason = false, nodeBuildSkew
		case node.CatalogueDigest != m.digest():
			candidate.Eligible, candidate.Reason = false, nodeCatalogueSkew
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
	return PlacementCandidate{}, errors.New("the fleet does not hold a row for this Spark")
}
