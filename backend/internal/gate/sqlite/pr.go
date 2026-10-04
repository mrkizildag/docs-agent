package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
)

// LoadPR returns the state most recently saved for owner/repo#number, or the
// zero-HeadSHA state (identity fields filled from the args) if it was never saved.
func (s *Store) LoadPR(ctx context.Context, owner, repo string, number int) (gate.PRState, error) {
	state := gate.PRState{Owner: owner, Repo: repo, Number: number}

	row := s.db.QueryRowContext(ctx,
		`SELECT installation_id, head_sha FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?`,
		owner, repo, number)

	if err := row.Scan(&state.InstallationID, &state.HeadSHA); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, nil
		}
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: %w", owner, repo, number, err)
	}

	return state, nil
}

// SavePR upserts state, keyed by owner/repo/number.
func (s *Store) SavePR(ctx context.Context, state gate.PRState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pull_requests (owner, repo, number, installation_id, head_sha)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (owner, repo, number) DO UPDATE SET
			installation_id = excluded.installation_id,
			head_sha = excluded.head_sha`,
		state.Owner, state.Repo, state.Number, state.InstallationID, state.HeadSHA)
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: %w", state.Owner, state.Repo, state.Number, err)
	}
	return nil
}
