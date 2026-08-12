package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RevokedRecipe is one revocation this machine has accepted from a signed
// index: the publisher no longer stands behind exactly this recipe version
// (ADR 0009 item 7). Reason is carried verbatim because it is what the owner
// reads when an install is refused, and AcceptedAt records when this machine
// learned of it, which is not the same moment the publisher revoked.
type RevokedRecipe struct {
	RecipeID      string `json:"recipe_id"`
	RecipeVersion int    `json:"recipe_version"`
	Reason        string `json:"reason"`
	RevokedAt     string `json:"revoked_at"`
	AcceptedAt    string `json:"accepted_at"`
}

// RecordRevocation is the only way a revocation enters this database, and
// there is deliberately no way for one to leave it. INSERT OR IGNORE also
// means the first accepted reason is the one that stands: a later index
// cannot rewrite the explanation the owner was already given, any more than
// it can withdraw the revocation itself.
func (s *Store) RecordRevocation(ctx context.Context, id string, version int, reason string, revokedAt time.Time) error {
	if strings.TrimSpace(id) == "" || version <= 0 {
		return errors.New("a revocation must name one exact recipe id and version")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("a revocation must carry a human-readable reason")
	}
	if revokedAt.IsZero() {
		return errors.New("a revocation must carry the time it was issued")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO recipe_revocations(recipe_id,recipe_version,reason,revoked_at,accepted_at) VALUES(?,?,?,?,?)`,
		id, version, reason, revokedAt.UTC().Format(time.RFC3339), now()); err != nil {
		return fmt.Errorf("record recipe revocation: %w", err)
	}
	return nil
}

// Revocations returns every revocation this machine has ever accepted, so a
// listing can mark the affected recipes and models in one read rather than
// one query per row.
func (s *Store) Revocations(ctx context.Context) ([]RevokedRecipe, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT recipe_id,recipe_version,reason,revoked_at,accepted_at FROM recipe_revocations ORDER BY recipe_id,recipe_version`)
	if err != nil {
		return nil, fmt.Errorf("list recipe revocations: %w", err)
	}
	defer rows.Close()
	revocations := []RevokedRecipe{}
	for rows.Next() {
		var entry RevokedRecipe
		if err := rows.Scan(&entry.RecipeID, &entry.RecipeVersion, &entry.Reason, &entry.RevokedAt, &entry.AcceptedAt); err != nil {
			return nil, err
		}
		revocations = append(revocations, entry)
	}
	return revocations, rows.Err()
}

// Revocation answers the one question an install has to ask: is this exact
// version revoked, and if so what was the owner told. A different version of
// the same recipe is a different question with its own answer.
func (s *Store) Revocation(ctx context.Context, id string, version int) (RevokedRecipe, bool, error) {
	entry := RevokedRecipe{RecipeID: id, RecipeVersion: version}
	err := s.db.QueryRowContext(ctx,
		`SELECT reason,revoked_at,accepted_at FROM recipe_revocations WHERE recipe_id=? AND recipe_version=?`, id, version).
		Scan(&entry.Reason, &entry.RevokedAt, &entry.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RevokedRecipe{}, false, nil
	}
	if err != nil {
		return RevokedRecipe{}, false, fmt.Errorf("read recipe revocation: %w", err)
	}
	return entry, true, nil
}
