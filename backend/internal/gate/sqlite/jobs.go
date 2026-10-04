package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
)

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// Enqueue records delivery.DeliveryID and inserts a pending job for it, in one
// transaction. A delivery seen before is a no-op unless every job recorded
// against it ended failed: GitHub reuses a delivery ID on Redeliver, and a
// failed delivery must be retryable. If job.Supersedes, older pending jobs
// with the same key+kind are marked superseded, and the IDs of older running
// jobs with the same key+kind are returned (left running; the caller cancels
// them).
func (s *Store) Enqueue(ctx context.Context, job jobqueue.NewJob) (bool, []int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("enqueue job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deliveries (delivery_id, received_at) VALUES (?, ?)`,
		job.DeliveryID, ts); err != nil {
		if !isUniqueViolation(err) {
			return false, nil, fmt.Errorf("enqueue job: insert delivery %s: %w", job.DeliveryID, err)
		}
		retryable, err := deliveryOnlyHasFailedJobs(ctx, tx, job.DeliveryID)
		if err != nil {
			return false, nil, err
		}
		if !retryable {
			return false, nil, nil
		}
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (key, kind, payload, state, error, created_at, updated_at, delivery_id)
		 VALUES (?, ?, ?, ?, '', ?, ?, ?)`,
		job.Key, job.Kind, job.Payload, jobqueue.StatePending, ts, ts, job.DeliveryID)
	if err != nil {
		return false, nil, fmt.Errorf("enqueue job: insert job: %w", err)
	}

	newID, err := res.LastInsertId()
	if err != nil {
		return false, nil, fmt.Errorf("enqueue job: last insert id: %w", err)
	}

	var supersededRunning []int64
	if job.Supersedes {
		supersededRunning, err = supersede(ctx, tx, job.Key, job.Kind, newID, ts)
		if err != nil {
			return false, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("enqueue job: commit: %w", err)
	}

	return true, supersededRunning, nil
}

// deliveryOnlyHasFailedJobs reports whether every job recorded against
// deliveryID has ended in StateFailed, meaning a redelivery under the same ID
// should be allowed to enqueue a new job.
func deliveryOnlyHasFailedJobs(ctx context.Context, tx *sql.Tx, deliveryID string) (bool, error) {
	var notFailed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE delivery_id = ? AND state != ?`,
		deliveryID, jobqueue.StateFailed).Scan(&notFailed); err != nil {
		return false, fmt.Errorf("enqueue job: count non-failed jobs for delivery %s: %w", deliveryID, err)
	}
	return notFailed == 0, nil
}

// supersede marks older pending jobs with the given key+kind as superseded,
// and returns the IDs of older running jobs with the same key+kind.
func supersede(ctx context.Context, tx *sql.Tx, key, kind string, newID int64, ts string) ([]int64, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ? WHERE key = ? AND kind = ? AND state = ? AND id != ?`,
		jobqueue.StateSuperseded, ts, key, kind, jobqueue.StatePending, newID); err != nil {
		return nil, fmt.Errorf("enqueue job: supersede pending: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM jobs WHERE key = ? AND kind = ? AND state = ? AND id != ?`,
		key, kind, jobqueue.StateRunning, newID)
	if err != nil {
		return nil, fmt.Errorf("enqueue job: select running: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("enqueue job: scan running id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enqueue job: iterate running: %w", err)
	}

	return ids, nil
}

// Claim atomically marks the oldest pending job whose key has no running job
// as running and returns it. ok is false when there is nothing to claim.
func (s *Store) Claim(ctx context.Context) (jobqueue.Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return jobqueue.Job{}, false, fmt.Errorf("claim job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT id, key, kind, payload FROM jobs
		WHERE state = ?
		  AND key NOT IN (SELECT key FROM jobs WHERE state = ?)
		ORDER BY id
		LIMIT 1`,
		jobqueue.StatePending, jobqueue.StateRunning)

	var job jobqueue.Job
	if err := row.Scan(&job.ID, &job.Key, &job.Kind, &job.Payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return jobqueue.Job{}, false, nil
		}
		return jobqueue.Job{}, false, fmt.Errorf("claim job: scan: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ? WHERE id = ?`,
		jobqueue.StateRunning, now(), job.ID); err != nil {
		return jobqueue.Job{}, false, fmt.Errorf("claim job %d: update: %w", job.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return jobqueue.Job{}, false, fmt.Errorf("claim job %d: commit: %w", job.ID, err)
	}

	return job, true, nil
}

// Finish sets the terminal state, error message, and updated_at for job id.
func (s *Store) Finish(ctx context.Context, id int64, state jobqueue.State, errMsg string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, error = ?, updated_at = ? WHERE id = ?`,
		state, errMsg, now(), id); err != nil {
		return fmt.Errorf("finish job %d: %w", id, err)
	}
	return nil
}

// RequeueRunning moves every running job back to pending, for startup
// recovery after an unclean shutdown. It returns the number of jobs moved.
func (s *Store) RequeueRunning(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ? WHERE state = ?`,
		jobqueue.StatePending, now(), jobqueue.StateRunning)
	if err != nil {
		return 0, fmt.Errorf("requeue running jobs: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("requeue running jobs: rows affected: %w", err)
	}

	return int(n), nil
}
