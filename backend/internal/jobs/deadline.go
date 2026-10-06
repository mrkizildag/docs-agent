package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"math/bits"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

// OverdueSource lists the awaited analysis runs that are past their deadline.
type OverdueSource interface {
	OverdueRuns(ctx context.Context, now time.Time) ([]gate.OverdueRun, error)
}

const (
	// deadlineRetryCap is how long past its deadline a run is still retried.
	deadlineRetryCap = 24 * time.Hour
	// deadlineTailFromMinutes is the minutes overdue at which the power-of-two
	// buckets end; from there a retry comes every deadlineTailEveryMinutes.
	deadlineTailFromMinutes  = 1024
	deadlineTailEveryMinutes = 240
	// deadlineSweepEvery is how often SweepDeadlines runs EnqueueDeadlineJobs; the
	// give-up warn window matches it so the warn logs once.
	deadlineSweepEvery = 30 * time.Second
)

// SweepDeadlines enqueues deadline jobs for overdue runs every deadlineSweepEvery
// until ctx is done.
func SweepDeadlines(ctx context.Context, src OverdueSource, jobs Enqueuer, logger *slog.Logger) {
	ticker := time.NewTicker(deadlineSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := EnqueueDeadlineJobs(ctx, src, jobs, logger, now); err != nil {
				logger.Error("sweep deadlines", "err", err)
			}
		}
	}
}

// EnqueueDeadlineJobs enqueues deadline jobs for overdue runs. A run's jobs
// dedupe by nonce and by the power-of-two bucket of whole minutes past its
// deadline, so a run whose job failed is retried at about 0, 1, 2, 4, 8, ...
// minutes overdue, then every 240 minutes from 1024. Runs more than
// deadlineRetryCap overdue are dropped.
func EnqueueDeadlineJobs(ctx context.Context, src OverdueSource, jobs Enqueuer, logger *slog.Logger, now time.Time) error {
	overdue, err := src.OverdueRuns(ctx, now)
	if err != nil {
		return fmt.Errorf("enqueue deadline jobs: %w", err)
	}
	for _, run := range overdue {
		key, kind := prJobKey(run.Owner, run.Repo, run.Number), runDeadlineJobKind
		if run.Scaffold {
			key, kind = scaffoldJobKey(gate.RepoRef{Owner: run.Owner, Repo: run.Repo}), scaffoldDeadlineJobKind
		}
		late := now.Sub(run.Deadline)
		if late > deadlineRetryCap {
			if late < deadlineRetryCap+deadlineSweepEvery {
				logger.Warn("giving up on overdue run", "run", key, "nonce", run.Nonce)
			}
			continue
		}
		bucket := deadlineBucket(int(late / time.Minute))
		job, err := newJob(fmt.Sprintf("deadline:%s:%d", run.Nonce, bucket), key, kind, run)
		if err != nil {
			return err
		}
		if _, err := jobs.Enqueue(ctx, job); err != nil {
			return fmt.Errorf("enqueue deadline job for %s: %w", key, err)
		}
	}
	return nil
}

// deadlineBucket is the retry bucket of a run that is minutes overdue: the bit
// length of minutes below deadlineTailFromMinutes, then one bucket per
// deadlineTailEveryMinutes.
func deadlineBucket(minutes int) int {
	if minutes >= deadlineTailFromMinutes {
		return bits.Len(deadlineTailFromMinutes) + (minutes-deadlineTailFromMinutes)/deadlineTailEveryMinutes
	}
	return bits.Len(uint(minutes))
}
