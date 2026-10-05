---
title: Job queue
summary: How webhook work is queued, deduplicated, superseded, and recovered after a restart.
covers:
  - backend/internal/jobqueue/**
  - backend/internal/gate/sqlite/**
---

# Job queue

The webhook handler never does GitHub or LLM work itself. It stores the delivery and a job in one SQLite transaction and returns 202; a worker in the same process runs the job. See [Architecture](../architecture.md) for where this sits.

## Guarantees

- **Deduplicated by delivery.** A repeated `X-GitHub-Delivery` ID is acknowledged and changes nothing, including after a restart.
- **One job per PR at a time.** Jobs share a key (`owner/repo#number`); jobs with the same key run in order, one at a time, and different PRs run in parallel up to a fixed cap.
- **Newer push wins.** A `pull_request` job supersedes older jobs with the same key and kind: pending ones are skipped, a running one has its context cancelled. Jobs of other kinds are not affected. A re-run (see [Proposal output](proposal-output.md)) is a `pull_request`-kind job too, so a re-run and a push supersede each other and only the newest produces the final check.
- **Job kinds.** `pull_request` (supersedes), `workflow_run` (an Actions analysis run finished), and `run_deadline` (an awaited run passed its deadline). A sweep every 30 seconds enqueues one `run_deadline` job per overdue awaited run; its delivery ID is `deadline:<nonce>` plus the current minute, so sweeps within a minute dedupe and a deadline job whose GitHub write was rejected is retried the next minute until the check is closed. A deadline job for a run that has since completed or been replaced by a new push does nothing.
- **At least once.** A job running at shutdown or crash stays `running` and is requeued on the next start, so it can run twice. Every handler must be safe to repeat for the same payload.
- **No job-level retry.** A failed job is recorded with its error and not retried; the next push or re-run for the PR replaces it. The only retry is the per-minute deadline job, which backstops a check left in progress.

## Not yet

Delivery and job rows are never pruned. A retention job is a follow-up.
