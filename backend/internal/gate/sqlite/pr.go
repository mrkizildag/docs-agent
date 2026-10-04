package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
)

// LoadPR returns the state most recently saved for owner/repo#number, or the
// zero-HeadSHA state (identity fields filled from the args) if it was never saved.
func (s *Store) LoadPR(ctx context.Context, owner, repo string, number int) (gate.PRState, error) {
	state := gate.PRState{Owner: owner, Repo: repo, Number: number}

	var run gate.AwaitingRun
	var deadline string
	row := s.db.QueryRowContext(ctx,
		`SELECT installation_id, head_sha, check_run_id, run_id, run_nonce, run_deadline
		FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?`,
		owner, repo, number)

	if err := row.Scan(&state.InstallationID, &state.HeadSHA, &state.CheckRunID, &run.RunID, &run.Nonce, &deadline); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, nil
		}
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: %w", owner, repo, number, err)
	}

	if run.RunID != 0 {
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: parse run deadline %q: %w", owner, repo, number, deadline, err)
		}
		run.Deadline = parsed
		state.Run = &run
	}

	return state, nil
}

// SavePR upserts state, keyed by owner/repo/number.
func (s *Store) SavePR(ctx context.Context, state gate.PRState) error {
	var run gate.AwaitingRun
	var deadline string
	if state.Run != nil {
		run = *state.Run
		deadline = run.Deadline.UTC().Format(time.RFC3339Nano)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pull_requests (owner, repo, number, installation_id, head_sha, check_run_id, run_id, run_nonce, run_deadline)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner, repo, number) DO UPDATE SET
			installation_id = excluded.installation_id,
			head_sha = excluded.head_sha,
			check_run_id = excluded.check_run_id,
			run_id = excluded.run_id,
			run_nonce = excluded.run_nonce,
			run_deadline = excluded.run_deadline`,
		state.Owner, state.Repo, state.Number, state.InstallationID, state.HeadSHA,
		state.CheckRunID, run.RunID, run.Nonce, deadline)
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: %w", state.Owner, state.Repo, state.Number, err)
	}
	return nil
}

// PRForRun returns the pull request that owner/repo's analysis run runID was
// dispatched for, and false if no pull request awaits it.
func (s *Store) PRForRun(ctx context.Context, owner, repo string, runID int64) (int, bool, error) {
	if runID == 0 {
		return 0, false, nil
	}

	var number int
	err := s.db.QueryRowContext(ctx,
		`SELECT number FROM pull_requests WHERE owner = ? AND repo = ? AND run_id = ?`,
		owner, repo, runID).Scan(&number)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find pr for run %d of %s/%s: %w", runID, owner, repo, err)
	}
	return number, true, nil
}

// OverdueRuns returns the awaited runs whose deadline is before now. Deadlines
// are compared as times, not as text, because RFC3339Nano does not sort.
func (s *Store) OverdueRuns(ctx context.Context, now time.Time) ([]gate.OverdueRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT owner, repo, number, run_nonce, run_deadline FROM pull_requests WHERE run_id != 0`)
	if err != nil {
		return nil, fmt.Errorf("list awaited runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var overdue []gate.OverdueRun
	for rows.Next() {
		var run gate.OverdueRun
		var deadline string
		if err := rows.Scan(&run.Owner, &run.Repo, &run.Number, &run.Nonce, &deadline); err != nil {
			return nil, fmt.Errorf("scan awaited run: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return nil, fmt.Errorf("parse run deadline %q of %s/%s#%d: %w", deadline, run.Owner, run.Repo, run.Number, err)
		}
		if now.After(parsed) {
			overdue = append(overdue, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list awaited runs: %w", err)
	}
	return overdue, nil
}
