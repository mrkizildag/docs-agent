---
title: Job queue
summary: How webhook work is queued, deduplicated, superseded, and recovered after a restart.
covers:
  - backend/internal/jobqueue/**
  - backend/internal/gate/sqlite/**
  - backend/internal/jobs/**
---

# Job queue

The webhook handler never does GitHub or LLM work itself. It stores the delivery and a job in one SQLite transaction and returns 202; a worker in the same process runs the job. See [Architecture](../architecture.md) for where this sits.

## Guarantees

- **Deduplicated by delivery.** A repeated `X-GitHub-Delivery` ID is acknowledged and changes nothing, including after a restart. The exception is a redelivery of a delivery whose jobs all failed: it is enqueued again, so GitHub's "Redeliver" retries failed work.
- **One job per PR at a time.** Jobs share a key (`owner/repo#number`); jobs with the same key run in order, one at a time, and different PRs run in parallel up to a fixed cap.
- **Newer push wins.** Each job has a kind (what it carries) and a supersede group (what it can cancel), both stored on the job. A `pull_request` job supersedes older jobs with the same key and group: pending ones are skipped, a running one has its context cancelled. Jobs in other groups are not affected. A re-run requested from the check run's Re-run button (`rerun`) or by ticking the summary's Re-run box (`comment_rerun`; see [Proposal output](proposal-output.md)) is in the `pull_request` group too but does not supersede: it queues behind a running analysis on the same key, so it never cancels one and then skips. A later push still supersedes a queued or running re-run, so only the newest head produces the final check.
- **Job kinds.** `pull_request` (supersedes), `rerun` (the check-run Re-run button's request) and `comment_rerun` (the summary Re-run tick, held as a comment event and routed to the comment handler; a push cancels both), `comment` (a human's new conversation comment, or a tick of the proposal or summary boxes other than Re-run; does not supersede and never cancels analysis, but is serialized with it on the PR key; only newly created conversation comments are enqueued, because one may be the reason a pending skip awaits; see [Apply and Skip](apply-skip.md)), `workflow_run` (an Actions analysis run finished), and `run_deadline` (an awaited run passed its deadline). A sweep every 30 seconds enqueues one `run_deadline` job per overdue awaited run; its delivery ID is `deadline:<nonce>:<bucket>`, so sweeps within one bucket dedupe by nonce and a deadline job whose GitHub write was rejected is retried in a later bucket until the check is closed. A deadline job for a run that has since completed or been replaced by a new push does nothing. Scaffold work runs on a per-repo key (`owner/repo#scaffold`) instead: `scaffold` (write and propose a repo's first `docs/`; its delivery ID carries the attempt, so a failed attempt is retried by the next PR event), `scaffold_run` (the scaffold's Actions run finished), and `scaffold_deadline` (the sweep found an awaited scaffold run past its deadline). None of them supersede; see [Docs scaffold](scaffold.md). The scaffold jobs go through the same worker as the others, so enqueueing one wakes it at once.
- **At least once.** A job running at shutdown or crash stays `running` and is requeued on the next start, so it can run twice. Every handler must be safe to repeat for the same payload.
- **No job-level retry.** A failed job is recorded with its error and not retried automatically; the next push or re-run for the PR replaces it, and a manual redelivery of its delivery runs it again. The only retry is the deadline job, which backstops a check left in progress: it is re-enqueued with exponential backoff (about 0, 1, 2, 4, 8 … 1024 minutes past the deadline, then every 4 hours) and given up, with a warning in the log, once the run is a day overdue.

Job kinds, keys, payload codecs, the handler that decodes a claimed job into a `gate` call, and the deadline sweep live in `internal/jobs`; `internal/httpapi` only turns webhooks into jobs through it. Jobs queued before `rerun` and `comment_rerun` existed carry the old `pull_request` payload (a re-run request or a comment event under `Rerun` or `Comment`) and are still decoded.

## Not yet

Delivery and job rows are never pruned. A retention job is a follow-up.
