---
title: Architecture
summary: The parts of docs-agent, how they connect, and the phase plan.
covers:
  - backend/**
---

# Architecture

docs-agent is a GitHub App. GitHub sends pull request events to the backend; the backend reads the diff, finds the docs that cover the changed files, asks an LLM whether they need to change, and reports the result as a check run that can block the merge.

## Parts

- **Backend** (`backend/`, Go): receives GitHub webhooks, talks to the GitHub and LLM APIs, owns the check run. `POST /webhook` verifies GitHub's signature. For a `pull_request` event (opened, synchronize, reopened) it stores the delivery and a durable job in one SQLite transaction, deduplicated by GitHub's delivery ID; other events are acknowledged without being stored. Accepted deliveries return 202. A worker processes jobs off that queue: it runs at most one job per PR at a time but PRs in parallel, a newer push supersedes the older job for the same PR, cancelling it if it is already running, and jobs left unfinished by a restart resume afterward. For a `pull_request` event (opened, synchronize, reopened) the job creates the `docs-agent` check run on the PR head commit, tracer-only so it always succeeds; analysis itself comes in a later task.
- **Frontend** (phase 2, TypeScript + shadcn/ui): org and repo settings, configurable docs structures.
- **Doc targets**: phase 1 writes to the repo's own `docs/` folder. Notion comes in phase 3.

Inside the backend, `internal/gate` is the domain and imports nothing else from the module. It declares the GitHub interface it needs; `internal/github` implements it. `internal/httpapi` turns webhooks into jobs on `internal/jobqueue`, the durable queue, and decodes them back into `gate.Service` calls when the worker runs them; `internal/gate/sqlite` implements both `gate`'s and `jobqueue`'s storage interfaces on one SQLite database. `internal/gate/sqlite` is an adapter, not part of the domain. Only `cmd/server` wires concrete types together. The queue's guarantees are in [Job queue](features/job-queue.md).

## Flow (phase 1 target)

1. A PR is opened or updated; GitHub sends `pull_request` to the backend.
2. The backend maps changed files to docs through each doc's `covers` globs (see [0002](decisions/0002-docs-structure.md)).
3. The LLM compares the diff with those docs and returns "no impact" or proposed edits.
4. The backend sets the `docs-agent` check: `success` for no impact, `action_required` with the proposal otherwise.
5. A developer applies, edits, or waives the proposal; the check turns green and the PR can merge.

## Phases

1. In-repo `docs/`, hard-coded default structure, merge check. See [0001](decisions/0001-in-repo-docs-first.md).
2. Frontend; structure becomes configurable.
3. One-way sync from repo docs to Notion.
4. Custom structures defined by users with their own prompt.
