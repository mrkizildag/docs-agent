---
title: Analysis history
summary: What pollux records about every analysis and every Apply, Skip, or outdated proposal, why it is written with the state, and how replays and re-runs stay idempotent.
covers:
  - backend/internal/gate/gate.go
  - backend/internal/gate/apply.go
  - backend/internal/gate/skip.go
  - backend/internal/gate/sqlite/pr.go
  - backend/internal/gate/sqlite/sqlite.go
---

# Analysis history

The store keeps each PR's current state, and every analysis overwrites the last. History is the append-only record next to it: what pollux concluded, with which runner and model, how long it took and what it cost, and what happened to each proposal afterwards. The dashboard and the eval work (#66) read it. Nothing in the gate reads it back; webhook handling, checks, and comments do not depend on it.

## What is recorded

Two tables, both keyed by PR.

- **Analyses**: one row per analysis run: head SHA, runner (`actions` or `server`; empty, with no start time, only for a run already in flight when history was introduced), model, verdict, a one-line reason or failure cause, proposal count, start and finish times, token and cost totals when the runner reports them, and the Actions run ID.
- **PR events**: one row per proposal outcome: who, what, scope, reason, and commit SHA. Kinds are `applied` (one per proposal, crediting the sender even when a crash left the commit to be adopted later), `skipped` (one per open proposal, or one row with no proposal when none was open), and `outdated` (one per proposal that a push made stale), so per-proposal outcomes can be counted.

## Verdicts

`no_impact`, `proposals`, `failed`, and `superseded`. The verdict is the analysis's real result, even when a Skip is active and the check passes regardless. `failed` carries the failure cause as the reason. `superseded` marks a run that never concluded because it was replaced: a push to a new head dropped it, or a Skip dropped the run still being awaited. Failures and supersessions have no runner result, so the start time and runner kind are kept on the awaited run itself and are the only source for them.

## The transaction invariant

The pure state transitions that change the state also return the rows that record the change, as a `History` next to the state. The caller passes both to the store's `SavePR`, which writes them in one transaction. A crash cannot leave history disagreeing with state: either both exist or neither does. The store never returns history when it loads a state, so a replay that changes no state returns and writes nothing. A transition's rows go to the save that persists its state change: a push that replaces an armed run saves the superseded row with the armed state, or with the failure if that save fails, and the outdated rows from reconciling comments are saved with the outdated proposals. Saves that record nothing pass an empty `History`.

## Idempotency

- **One row per run nonce, last wins.** A run can conclude twice (proposals, then neutral after a failed comment post). The second save replaces the first, so history matches the check.
- **Events are unique per key.** The key is built from the proposal, the commit or head, and for a skip the scope and user; saving the same event again writes nothing.
- A redelivered webhook, a re-run job, or a recovered crash therefore adds no row. A deliberate Re-run of the same head is a new run with a new nonce and writes a new row.
- Last wins is safe only because each PR's jobs run serially (see [Job queue](job-queue.md)); an older state saved after a newer conclusion of the same nonce would overwrite it.

## What is not stored

Model output, diffs, and doc text. Transcripts stay in Actions artifacts and server logs. Nothing from before the history existed is backfilled.

## Cost

Usage is reported by the runner, not computed by the gate; a runner that reports none leaves the columns empty, which is different from zero. Tokens and cost are reported independently, so a run can have one without the other; a JSON `null` counts as not reported.

- **Actions**: tokens (input, output, cache read, cache write) and `total_cost_usd` come from the result artifact. Under an OAuth token the cost is a list-price estimate, not spend, and `cost_basis` (`list`, or empty when unknown or mixed) is the only marker, so readers must label it as an estimate. See [Actions runner](actions-runner.md).
- **Server**: tokens only, summed over every model call. There is no pricing table, so cost stays empty. See [Server runner](server-runner.md).
- A failed analysis carries no usage.
