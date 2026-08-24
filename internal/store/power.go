package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// The two GPU power modes a Spark runs in, and the exact words every layer
// says them with: this column, the local API, and the fleet plane all use
// "full" or "cool" and nothing else.
//
// "full" is what every Spark has always done. "cool" caps the GPU clock, so
// the machine draws less power and makes less heat. Which clock the cap uses
// is not stored beside the mode: it is one qualified number in internal/power,
// because a number the owner can type is a number somebody can get wrong.
const (
	PowerModeFull = "full"
	PowerModeCool = "cool"
)

// ErrPowerMode is the one thing a power mode write refuses about its own
// input. It is a sentinel so that a handler can answer a word it does not know
// with 400 and a database it cannot read with 500.
var ErrPowerMode = errors.New(`the power mode must be "full" or "cool"`)

// PowerMode is this Spark's own power setting.
//
// Failure is empty while the mode is in force. It holds one plain sentence
// when basement asked the GPU for the mode and the machine did not take it.
// The mode stays what the owner chose even then, because the choice is
// durable and basement asks for it again at every start; the sentence says
// only that the machine has not taken it yet.
type PowerMode struct {
	Mode      string `json:"mode"`
	Failure   string `json:"failure,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// NormalizePowerMode accepts the two words and nothing else.
func NormalizePowerMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case PowerModeFull:
		return PowerModeFull, nil
	case PowerModeCool:
		return PowerModeCool, nil
	}
	return "", ErrPowerMode
}

// PowerMode reads the setting. A database with no row yet, and a row holding a
// word this release does not know, both read as full speed: that is what a
// machine does while nothing has capped it, so it is the honest answer and it
// keeps a strange row from ever becoming a command to the GPU.
func (s *Store) PowerMode(ctx context.Context) (PowerMode, error) {
	var mode PowerMode
	err := s.db.QueryRowContext(ctx, `SELECT mode,failure,updated_at FROM node_power WHERE singleton=1`).
		Scan(&mode.Mode, &mode.Failure, &mode.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PowerMode{Mode: PowerModeFull}, nil
	}
	if err != nil {
		return PowerMode{}, err
	}
	normalized, normalizeErr := NormalizePowerMode(mode.Mode)
	if normalizeErr != nil {
		normalized = PowerModeFull
	}
	mode.Mode = normalized
	return mode, nil
}

// SetPowerMode records the owner's choice. The old failure sentence leaves
// with the old mode: a choice nobody has acted on yet has not failed. What the
// GPU says about the new one arrives afterwards, through
// RecordPowerModeFailure.
func (s *Store) SetPowerMode(ctx context.Context, mode string) (PowerMode, error) {
	normalized, err := NormalizePowerMode(mode)
	if err != nil {
		return PowerMode{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO node_power(singleton,mode,failure,updated_at) VALUES(1,?,'',?)
ON CONFLICT(singleton) DO UPDATE SET mode=excluded.mode,failure='',updated_at=excluded.updated_at`, normalized, now()); err != nil {
		return PowerMode{}, err
	}
	return s.PowerMode(ctx)
}

// RecordPowerModeFailure says what happened when the GPU was asked for the
// stored mode. An empty sentence means the mode is in force, and it clears
// whatever an earlier attempt left behind. The mode itself is never touched
// here, so a Spark that could not take the cap still tries for it after a
// restart rather than quietly forgetting what its owner asked for.
func (s *Store) RecordPowerModeFailure(ctx context.Context, failure string) (PowerMode, error) {
	failure = strings.TrimSpace(failure)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO node_power(singleton,mode,failure,updated_at) VALUES(1,?,?,?)
ON CONFLICT(singleton) DO UPDATE SET failure=excluded.failure,updated_at=excluded.updated_at`, PowerModeFull, failure, now()); err != nil {
		return PowerMode{}, err
	}
	return s.PowerMode(ctx)
}
