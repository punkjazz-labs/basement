package recipe

import (
	"embed"
	"fmt"
)

//go:embed recipes/*.yaml
var recipeFiles embed.FS

func Builtin() ([]Recipe, error) {
	entries, err := recipeFiles.ReadDir("recipes")
	if err != nil {
		return nil, fmt.Errorf("read embedded recipes: %w", err)
	}
	result := make([]Recipe, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := recipeFiles.ReadFile("recipes/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read recipe %s: %w", entry.Name(), err)
		}
		r, err := DecodeStrict(data)
		if err != nil {
			return nil, fmt.Errorf("validate recipe %s: %w", entry.Name(), err)
		}
		result = append(result, r)
	}
	return result, nil
}

func Find(recipes []Recipe, id string) (Recipe, bool) {
	for _, r := range recipes {
		if r.ID == id {
			return r, true
		}
	}
	return Recipe{}, false
}
