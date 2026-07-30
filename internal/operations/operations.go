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
}

type Progress func(receipt any) error

type Executor interface {
	Execute(context.Context, Execution, recipe.Operation, recipe.Recipe, Progress) (map[string]any, error)
	Completed(context.Context, Execution, recipe.Operation, recipe.Recipe, json.RawMessage) bool
	ArtifactPath(recipe.Recipe) string
}
