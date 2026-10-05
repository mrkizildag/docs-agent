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
- **Newer push wins.** A `pull_request` job supersedes older jobs with the same key and kind: pending ones are skipped, a running one has its context cancelled. Jobs of other kinds are not affected.
- **Job kinds.** `pull_request` (supersedes), `comment` (a human's PR comment or tick; does not supersede and never cancels analysis, but is serialized with it on the PR key; every human comment is enqueued because it may be the reason a pending skip awaits; see [Apply and Skip](apply-skip.md)), `workflow_run` (an Actions analysis run finished), and `run_deadline` (an awaited run passed its deadline). A sweep every 30 seconds enqueues one `run_deadline` job per overdue awaited run; its delivery ID is `deadline:<nonce>`, so repeated sweeps dedupe by nonce. A deadline job for a run that has since completed or been replaced by a new push does nothing.
- **At least once.** A job running at shutdown or crash stays `running` and is requeued on the next start, so it can run twice. Every handler must be safe to repeat for the same payload.
- **Failures are final.** A failed job is recorded with its error and not retried; the next push for the PR replaces it. Retries arrive with the failure-path work.

## Not yet

Delivery and job rows are never pruned. A retention job is a follow-up.
