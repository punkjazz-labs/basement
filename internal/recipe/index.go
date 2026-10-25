package recipe

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"time"
)

// IndexSchemaVersion is the only schema_version this build understands. An
// index published with a different value is rejected outright: recipes
// arrive from a source this binary was not written to parse, and guessing
// at forward compatibility is how a malformed batch gets treated as valid.
const IndexSchemaVersion = 1

// Index is the top-level document at the remote recipe index URL. It carries
// the same recipe schema as the embedded YAML recipes (Recipe already tags
// every field for both yaml and json), so one validator covers both sources.
type Index struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Recipes       []Recipe  `json:"recipes"`
}

// rawIndex mirrors Index but keeps each recipe as unparsed JSON, so one
// malformed recipe entry cannot abort decoding of the whole index. Only the
// top-level shape (schema_version, generated_at, the recipes array itself)
// is required to be well-formed; anything wrong inside one recipe object is
// this recipe's problem alone.
type rawIndex struct {
	SchemaVersion int               `json:"schema_version"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Recipes       []json.RawMessage `json:"recipes"`
}

// VerifyAndParseIndex is the entire trust boundary for a remote index. It
// MUST be called with the exact bytes as fetched, before anything else in
// this program treats them as structured data:
//
//  1. verify the detached signature over indexBytes — unverified bytes never
//     reach the JSON decoder below this line;
//  2. decode the index envelope strictly;
//  3. decode and validate each recipe independently, dropping (not failing)
//     any recipe that does not decode or does not pass Validate, with a
//     human-readable reason, so one bad entry never poisons the batch.
//
// Downgrade protection (comparing GeneratedAt against the last accepted
// index) is the caller's responsibility, because it depends on state
// (the previously cached index) that this function does not have and must
// not need in order to be independently testable.
func VerifyAndParseIndex(indexBytes, signatureFile []byte, publicKey ed25519.PublicKey) (Index, []string, error) {
	if err := VerifySignature(indexBytes, signatureFile, publicKey); err != nil {
		return Index{}, nil, err
	}
	var raw rawIndex
	decoder := json.NewDecoder(bytes.NewReader(indexBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Index{}, nil, fmt.Errorf("decode index: %w", err)
	}
	if raw.SchemaVersion != IndexSchemaVersion {
		return Index{}, nil, fmt.Errorf("index schema_version %d is not supported (want %d)", raw.SchemaVersion, IndexSchemaVersion)
	}
	if raw.GeneratedAt.IsZero() {
		return Index{}, nil, fmt.Errorf("index generated_at is missing or zero")
	}
	valid := make([]Recipe, 0, len(raw.Recipes))
	var reasons []string
	for i, entry := range raw.Recipes {
		r, err := decodeRecipeJSON(entry)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("index recipe[%d] dropped: %s", i, err))
			continue
		}
		if err := Validate(r); err != nil {
			reasons = append(reasons, fmt.Sprintf("index recipe %s dropped: %s", r.ID, err))
			continue
		}
		valid = append(valid, r)
	}
	return Index{SchemaVersion: raw.SchemaVersion, GeneratedAt: raw.GeneratedAt, Recipes: valid}, reasons, nil
}

// decodeRecipeJSON decodes one recipe object with the same strictness as
// DecodeStrict (unknown fields rejected), but from JSON rather than YAML —
// the wire format published in the remote index — without also running
// Validate, so callers can attach recipe-specific context to a validation
// failure.
func decodeRecipeJSON(data json.RawMessage) (Recipe, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var r Recipe
	if err := decoder.Decode(&r); err != nil {
		return Recipe{}, err
	}
	return r, nil
}
