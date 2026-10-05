package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const scaffoldColumns = `installation_id, phase, attempt, failures, base_sha, run_id, run_nonce, run_deadline, files, commit_sha, pr_number, pr_url`

// LoadScaffold returns the scaffold state saved for owner/repo, or the Idle
// zero-attempt state (identity fields filled from the args) if there is none.
func (s *Store) LoadScaffold(ctx context.Context, owner, repo string) (gate.ScaffoldState, error) {
	return loadScaffold(ctx, s.db, owner, repo)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadScaffold(ctx context.Context, q queryRower, owner, repo string) (gate.ScaffoldState, error) {
	state := gate.ScaffoldState{Owner: owner, Repo: repo, Phase: gate.ScaffoldIdle}

	var runID int64
	var nonce, deadline, files string
	err := q.QueryRowContext(ctx,
		`SELECT `+scaffoldColumns+` FROM repo_scaffolds WHERE owner = ? AND repo = ?`, owner, repo).
		Scan(&state.InstallationID, &state.Phase, &state.Attempt, &state.Failures, &state.BaseSHA, &runID, &nonce, &deadline, &files, &state.CommitSHA, &state.PRNumber, &state.PRURL)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return gate.ScaffoldState{}, fmt.Errorf("load scaffold %s/%s: %w", owner, repo, err)
	}

	if nonce != "" {
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return gate.ScaffoldState{}, fmt.Errorf("load scaffold %s/%s: parse run deadline %q: %w", owner, repo, deadline, err)
		}
		state.Run = &gate.AwaitingRun{RunID: runID, Nonce: nonce, Deadline: parsed}
	}
	if files != "" {
		state.Files = &review.Scaffold{}
		if err := json.Unmarshal([]byte(files), state.Files); err != nil {
			return gate.ScaffoldState{}, fmt.Errorf("load scaffold %s/%s: parse files: %w", owner, repo, err)
		}
	}
	return state, nil
}

// SaveScaffold upserts state, keyed by owner/repo. It never overwrites a stored
// installation_id: RequestScaffold owns it.
func (s *Store) SaveScaffold(ctx context.Context, state gate.ScaffoldState) error {
	var runID int64
	var nonce, deadline string
	if state.Run != nil {
		runID, nonce, deadline = state.Run.RunID, state.Run.Nonce, state.Run.Deadline.UTC().Format(time.RFC3339Nano)
	}
	var files []byte
	if state.Files != nil {
		var err error
		if files, err = json.Marshal(state.Files); err != nil {
			return fmt.Errorf("save scaffold %s/%s: encode files: %w", state.Owner, state.Repo, err)
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repo_scaffolds (owner, repo, `+scaffoldColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner, repo) DO UPDATE SET
			phase = excluded.phase,
			attempt = excluded.attempt,
			failures = excluded.failures,
			base_sha = excluded.base_sha,
			run_id = excluded.run_id,
			run_nonce = excluded.run_nonce,
			run_deadline = excluded.run_deadline,
			files = excluded.files,
			commit_sha = excluded.commit_sha,
			pr_number = excluded.pr_number,
			pr_url = excluded.pr_url`,
		state.Owner, state.Repo, state.InstallationID, state.Phase, state.Attempt, state.Failures, state.BaseSHA, runID, nonce, deadline, string(files), state.CommitSHA, state.PRNumber, state.PRURL)
	if err != nil {
		return fmt.Errorf("save scaffold %s/%s: %w", state.Owner, state.Repo, err)
	}
	return nil
}

// RequestScaffold creates owner/repo's Idle scaffold state if it has none and
// records waiter, in one transaction, and returns the state as it stands.
// Existing state keeps its phase; only installation_id is refreshed. A waiter
// already recorded is a no-op.
func (s *Store) RequestScaffold(ctx context.Context, installationID int64, owner, repo string, waiter gate.ScaffoldWaiter) (gate.ScaffoldState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return gate.ScaffoldState{}, fmt.Errorf("request scaffold %s/%s: begin: %w", owner, repo, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO repo_scaffolds (owner, repo, installation_id, phase) VALUES (?, ?, ?, ?) ON CONFLICT (owner, repo) DO UPDATE SET installation_id = excluded.installation_id`,
		owner, repo, installationID, gate.ScaffoldIdle); err != nil {
		return gate.ScaffoldState{}, fmt.Errorf("request scaffold %s/%s: create state: %w", owner, repo, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO scaffold_waiters (owner, repo, check_run_id) VALUES (?, ?, ?) ON CONFLICT (owner, repo, check_run_id) DO NOTHING`,
		owner, repo, waiter.CheckRunID); err != nil {
		return gate.ScaffoldState{}, fmt.Errorf("request scaffold %s/%s: record check run %d: %w", owner, repo, waiter.CheckRunID, err)
	}
	state, err := loadScaffold(ctx, tx, owner, repo)
	if err != nil {
		return gate.ScaffoldState{}, err
	}
	if err := tx.Commit(); err != nil {
		return gate.ScaffoldState{}, fmt.Errorf("request scaffold %s/%s: commit: %w", owner, repo, err)
	}
	return state, nil
}

// UnlinkedScaffoldWaiters returns the check runs recorded for owner/repo and not
// yet marked linked, oldest first.
func (s *Store) UnlinkedScaffoldWaiters(ctx context.Context, owner, repo string) ([]gate.ScaffoldWaiter, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT check_run_id FROM scaffold_waiters WHERE owner = ? AND repo = ? AND linked = 0 ORDER BY id`, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("list scaffold waiters of %s/%s: %w", owner, repo, err)
	}
	defer func() { _ = rows.Close() }()

	var waiters []gate.ScaffoldWaiter
	for rows.Next() {
		var w gate.ScaffoldWaiter
		if err := rows.Scan(&w.CheckRunID); err != nil {
			return nil, fmt.Errorf("scan scaffold waiter of %s/%s: %w", owner, repo, err)
		}
		waiters = append(waiters, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list scaffold waiters of %s/%s: %w", owner, repo, err)
	}
	return waiters, nil
}

// MarkScaffoldWaiterLinked marks owner/repo's check run checkRunID as no longer waiting.
func (s *Store) MarkScaffoldWaiterLinked(ctx context.Context, owner, repo string, checkRunID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE scaffold_waiters SET linked = 1 WHERE owner = ? AND repo = ? AND check_run_id = ?`, owner, repo, checkRunID); err != nil {
		return fmt.Errorf("mark scaffold waiter %d of %s/%s linked: %w", checkRunID, owner, repo, err)
	}
	return nil
}

// ScaffoldForRun reports whether owner/repo's scaffold awaits the workflow run runID.
func (s *Store) ScaffoldForRun(ctx context.Context, owner, repo string, runID int64) (bool, error) {
	if runID == 0 {
		return false, nil
	}
	var found int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM repo_scaffolds WHERE owner = ? AND repo = ? AND phase = ? AND run_id = ?`,
		owner, repo, gate.ScaffoldAwaiting, runID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find scaffold for run %d of %s/%s: %w", runID, owner, repo, err)
	}
	return true, nil
}
