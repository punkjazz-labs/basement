package recipe

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
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
	SchemaVersion int          `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Recipes       []Recipe     `json:"recipes"`
	Revoked       []Revocation `json:"revoked,omitempty"`
}

// Revocation is the publisher saying it no longer stands behind exactly one
// recipe version (ADR 0009 item 7). Every field is deliberate:
//
//   - ID and Version name one recipe at one version. There is no wildcard,
//     no range, and no product-wide form, so the mechanism cannot be turned
//     into a kill switch for a fleet: revoking a hundred versions costs a
//     hundred visible, auditable entries in a signed, immutable document.
//   - Reason is required and is shown verbatim to the person whose model it
//     concerns, so a revocation can never arrive as an unexplained refusal.
//   - RevokedAt is RFC3339 by virtue of being a time.Time: encoding/json
//     accepts nothing else, so a malformed timestamp cannot slip through.
type Revocation struct {
	ID        string    `json:"id"`
	Version   int       `json:"version"`
	Reason    string    `json:"reason"`
	RevokedAt time.Time `json:"revoked_at"`
}

// rawIndex mirrors Index but keeps each recipe as unparsed JSON, so one
// malformed recipe entry cannot abort decoding of the whole index. Only the
// top-level shape (schema_version, generated_at, the recipes array itself)
// is required to be well-formed; anything wrong inside one recipe object is
// this recipe's problem alone.
//
// Revocations are the deliberate exception: they decode into their final
// type, and a malformed one fails the whole index rather than being dropped
// with a reason. A recipe we cannot parse is a recipe we simply do not
// offer; a revocation we cannot parse is a safety statement we would be
// silently discarding, and the honest response to that is to refuse the
// document and keep using the last one we did understand.
type rawIndex struct {
	SchemaVersion int               `json:"schema_version"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Recipes       []json.RawMessage `json:"recipes"`
	Revoked       []json.RawMessage `json:"revoked"`
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
	revoked, err := decodeRevocations(raw.Revoked)
	if err != nil {
		return Index{}, nil, err
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
		// A verified label is earned by qualification evidence recorded in
		// this repository and is not transferable over the wire (ADR 0009
		// item 4), so whatever the index claims, a recipe arrives at most as
		// a candidate. Without this, a signed index could hand a user a
		// recipe calling itself basement-verified with nothing behind it.
		if r.Trust != "basement-candidate" || r.Verification != "candidate" {
			reasons = append(reasons, fmt.Sprintf("index recipe %s demoted to candidate: verified status is earned locally, not received", r.ID))
			r.Trust = "basement-candidate"
			r.Verification = "candidate"
		}
		valid = append(valid, r)
	}
	return Index{SchemaVersion: raw.SchemaVersion, GeneratedAt: raw.GeneratedAt, Recipes: valid, Revoked: revoked}, reasons, nil
}

// decodeRevocations decodes the index's revoked array with the same
// strictness the recipe objects get, and refuses the whole document on the
// first entry it cannot make sense of. Rejecting an unknown field is what
// keeps the schema from growing a wildcard by accident: an index that says
// {"id": "x", "versions": "*"} or adds "all_versions": true is refused here
// rather than quietly read as revoking one unnamed version.
func decodeRevocations(entries []json.RawMessage) ([]Revocation, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	revoked := make([]Revocation, 0, len(entries))
	for i, entry := range entries {
		decoder := json.NewDecoder(bytes.NewReader(entry))
		decoder.DisallowUnknownFields()
		var r Revocation
		if err := decoder.Decode(&r); err != nil {
			return nil, fmt.Errorf("index revoked[%d]: %w", i, err)
		}
		if strings.TrimSpace(r.ID) == "" {
			return nil, fmt.Errorf("index revoked[%d]: id is required", i)
		}
		if r.Version <= 0 {
			return nil, fmt.Errorf("index revoked[%d]: version must name one exact recipe version", i)
		}
		if strings.TrimSpace(r.Reason) == "" {
			// A refusal the owner cannot read is a refusal they cannot act
			// on, so an unexplained revocation is not a revocation we accept.
			return nil, fmt.Errorf("index revoked[%d]: reason is required and must be human-readable", i)
		}
		if r.RevokedAt.IsZero() {
			return nil, fmt.Errorf("index revoked[%d]: revoked_at is required and must be RFC3339", i)
		}
		revoked = append(revoked, r)
	}
	return revoked, nil
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
