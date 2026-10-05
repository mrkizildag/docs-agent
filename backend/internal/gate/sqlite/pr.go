package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

// LoadPR returns the state most recently saved for owner/repo#number, or the
// zero-HeadSHA state (identity fields filled from the args) if it was never saved.
func (s *Store) LoadPR(ctx context.Context, owner, repo string, number int) (gate.PRState, error) {
	state := gate.PRState{Owner: owner, Repo: repo, Number: number}

	var run gate.AwaitingRun
	var deadline string
	row := s.db.QueryRowContext(ctx,
		`SELECT installation_id, head_sha, check_run_id, run_id, run_nonce, run_deadline, summary_comment_id, head_ref, proposals_sha,
			fork, pending_skip_user, pending_skip_scope, skip_user, skip_scope, skip_reason, skip_head_sha, failure_cause, pending_apply
		FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?`,
		owner, repo, number)

	var pending gate.SkipAsk
	var skip gate.Skip
	var pendingApply string
	if err := row.Scan(&state.InstallationID, &state.HeadSHA, &state.CheckRunID, &run.RunID, &run.Nonce, &deadline, &state.SummaryCommentID, &state.HeadRef, &state.ProposalsSHA,
		&state.Fork, &pending.User, &pending.Scope, &skip.User, &skip.Scope, &skip.Reason, &skip.HeadSHA, &state.FailureCause, &pendingApply); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, nil
		}
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: %w", owner, repo, number, err)
	}

	if pending.User != "" {
		state.PendingSkip = &pending
	}
	if skip.User != "" {
		state.Skip = &skip
	}

	if pendingApply != "" {
		state.PendingApply = &gate.PendingApply{}
		if err := json.Unmarshal([]byte(pendingApply), state.PendingApply); err != nil {
			return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: parse pending apply: %w", owner, repo, number, err)
		}
	}

	if run.Nonce != "" {
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: parse run deadline %q: %w", owner, repo, number, deadline, err)
		}
		run.Deadline = parsed
		state.Run = &run
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, doc_path, section, comment_id, comment_url, state, content, original, index_entry, applied_sha, reply_id FROM pr_proposals
		WHERE owner = ? AND repo = ? AND number = ? ORDER BY position`,
		owner, repo, number)
	if err != nil {
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d proposals: %w", owner, repo, number, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var p gate.ProposalState
		if err := rows.Scan(&p.ID, &p.DocPath, &p.Section, &p.CommentID, &p.CommentURL, &p.State, &p.Content, &p.Original, &p.IndexEntry, &p.AppliedSHA, &p.ReplyID); err != nil {
			return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d proposals: %w", owner, repo, number, err)
		}
		state.Proposals = append(state.Proposals, p)
	}
	if err := rows.Err(); err != nil {
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d proposals: %w", owner, repo, number, err)
	}

	return state, nil
}

// SavePR upserts state, keyed by owner/repo/number, replacing the PR's
// proposal rows in the same transaction.
func (s *Store) SavePR(ctx context.Context, state gate.PRState) error {
	var run gate.AwaitingRun
	var deadline string
	if state.Run != nil {
		run = *state.Run
		deadline = run.Deadline.UTC().Format(time.RFC3339Nano)
	}

	var pending gate.SkipAsk
	if state.PendingSkip != nil {
		pending = *state.PendingSkip
	}
	var skip gate.Skip
	if state.Skip != nil {
		skip = *state.Skip
	}

	var pendingApply []byte
	if state.PendingApply != nil {
		var err error
		if pendingApply, err = json.Marshal(state.PendingApply); err != nil {
			return fmt.Errorf("save pr %s/%s#%d: encode pending apply: %w", state.Owner, state.Repo, state.Number, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: begin: %w", state.Owner, state.Repo, state.Number, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO pull_requests (owner, repo, number, installation_id, head_sha, check_run_id, run_id, run_nonce, run_deadline, summary_comment_id, head_ref, proposals_sha,
			fork, pending_skip_user, pending_skip_scope, skip_user, skip_scope, skip_reason, skip_head_sha, failure_cause, pending_apply)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner, repo, number) DO UPDATE SET
			installation_id = excluded.installation_id,
			head_sha = excluded.head_sha,
			check_run_id = excluded.check_run_id,
			run_id = excluded.run_id,
			run_nonce = excluded.run_nonce,
			run_deadline = excluded.run_deadline,
			summary_comment_id = excluded.summary_comment_id,
			head_ref = excluded.head_ref,
			proposals_sha = excluded.proposals_sha,
			fork = excluded.fork,
			pending_skip_user = excluded.pending_skip_user,
			pending_skip_scope = excluded.pending_skip_scope,
			skip_user = excluded.skip_user,
			skip_scope = excluded.skip_scope,
			skip_reason = excluded.skip_reason,
			skip_head_sha = excluded.skip_head_sha,
			failure_cause = excluded.failure_cause,
			pending_apply = excluded.pending_apply`,
		state.Owner, state.Repo, state.Number, state.InstallationID, state.HeadSHA,
		state.CheckRunID, run.RunID, run.Nonce, deadline, state.SummaryCommentID, state.HeadRef, state.ProposalsSHA,
		state.Fork, pending.User, pending.Scope, skip.User, skip.Scope, skip.Reason, skip.HeadSHA, state.FailureCause, string(pendingApply))
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: %w", state.Owner, state.Repo, state.Number, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pr_proposals WHERE owner = ? AND repo = ? AND number = ?`,
		state.Owner, state.Repo, state.Number); err != nil {
		return fmt.Errorf("save pr %s/%s#%d: clear proposals: %w", state.Owner, state.Repo, state.Number, err)
	}
	for i, p := range state.Proposals {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pr_proposals (owner, repo, number, position, id, doc_path, section, comment_id, comment_url, state,
				content, original, index_entry, applied_sha, reply_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			state.Owner, state.Repo, state.Number, i, p.ID, p.DocPath, p.Section, p.CommentID, p.CommentURL, p.State,
			p.Content, p.Original, p.IndexEntry, p.AppliedSHA, p.ReplyID)
		if err != nil {
			return fmt.Errorf("save pr %s/%s#%d: proposal %s: %w", state.Owner, state.Repo, state.Number, p.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save pr %s/%s#%d: commit: %w", state.Owner, state.Repo, state.Number, err)
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

// PRsForHead returns the numbers of owner/repo's stored pull requests whose
// head is headSHA.
func (s *Store) PRsForHead(ctx context.Context, owner, repo, headSHA string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT number FROM pull_requests WHERE owner = ? AND repo = ? AND head_sha = ? ORDER BY number`,
		owner, repo, headSHA)
	if err != nil {
		return nil, fmt.Errorf("find prs for head %s of %s/%s: %w", headSHA, owner, repo, err)
	}
	defer func() { _ = rows.Close() }()

	var numbers []int
	for rows.Next() {
		var number int
		if err := rows.Scan(&number); err != nil {
			return nil, fmt.Errorf("scan pr for head %s of %s/%s: %w", headSHA, owner, repo, err)
		}
		numbers = append(numbers, number)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find prs for head %s of %s/%s: %w", headSHA, owner, repo, err)
	}
	return numbers, nil
}

// OverdueRuns returns the awaited runs whose deadline is before now. Deadlines
// are compared as times, not as text, because RFC3339Nano does not sort.
func (s *Store) OverdueRuns(ctx context.Context, now time.Time) ([]gate.OverdueRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT owner, repo, number, run_nonce, run_deadline FROM pull_requests WHERE run_nonce != ''`)
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
			run.Deadline = parsed
			overdue = append(overdue, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list awaited runs: %w", err)
	}
	return overdue, nil
}
