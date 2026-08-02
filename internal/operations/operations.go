package operations

import (
	"context"
	"encoding/json"

	"github.com/punkjazz-labs/runonspark-manager/internal/recipe"
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
