---
title: Architecture
summary: The parts of docs-agent, how they connect, and the phase plan.
covers:
  - backend/**
  - action/**
---

# Architecture

docs-agent is a GitHub App. GitHub sends pull request events to the backend; the backend reads the diff, finds the docs that cover the changed files, asks an LLM whether they need to change, and reports the result as a check run that can block the merge.

## Parts

- **Backend** (`backend/`, Go): receives GitHub webhooks, talks to the GitHub and LLM APIs, owns the check run. `POST /webhook` verifies GitHub's signature. For a `pull_request` event (opened, synchronize, reopened) it stores the delivery and a durable job in one SQLite transaction, deduplicated by GitHub's delivery ID; other events are acknowledged without being stored. Accepted deliveries return 202. A worker processes jobs off that queue: it runs at most one job per PR at a time but PRs in parallel, a newer push supersedes the older job for the same PR, cancelling it if it is already running, and jobs left unfinished by a restart resume afterward. The `pull_request` job picks an analysis runner for the repo and reports its result as the `docs-agent` check run on the PR head commit.
- **Frontend** (phase 2, TypeScript + shadcn/ui): org and repo settings, configurable docs structures.
- **Doc targets**: phase 1 writes to the repo's own `docs/` folder. Notion comes in phase 3.

Inside the backend, `internal/gate` is the domain and imports only `internal/review`, the contracts shared with the analysis runners. It declares the GitHub and Store interfaces it needs; `internal/github` implements GitHub. `internal/httpapi` turns webhooks into jobs on `internal/jobqueue`, the durable queue, and decodes them back into `gate.Service` calls when the worker runs them; `internal/gate/sqlite` implements both `gate`'s and `jobqueue`'s storage interfaces on one SQLite database. `internal/gate/sqlite` is an adapter, not part of the domain. Only `cmd/server` wires concrete types together. The queue's guarantees are in [Job queue](features/job-queue.md). A runner that needs GitHub credentials outside the API client (the server runner's `git` clone) gets `github.Client.InstallationToken` from `cmd/server` as a function, not the client, so no runner imports `internal/github`; the token is narrowed to one repo with `contents: read`.

Runner selection: a repo that has the docs-agent Actions workflow on its default branch runs analysis through the Actions runner; otherwise, if the server has `LLM_PROVIDER` configured, it runs through the server runner; otherwise the PR gets a neutral check titled "No analysis runner configured" linking the setup guide. The server runner is described in [Server runner](features/server-runner.md). A repo with the workflow never falls back to the server runner.

Analysis contract: `internal/review` defines what a runner is asked to review and the proposals it returns. `action/proposal.schema.json` is generated from the proposal type by `make generate` and checked in; never edit it by hand, a test fails when it drifts. The schema checks shape and the `docs/` prefix; proposal validation in `internal/review` is the full check (anchor inside a diff hunk, one-line reason, index entry exactly when the proposal creates a new doc), and every runner's output must pass it before the gate acts on it.

Docs model: `internal/docs` reads the `docs/` tree of a checkout into docs (frontmatter and sections) and matches changed files to docs by their `covers` globs, relative to the repo root with `**` crossing folders. It takes a file system, never calls GitHub, and imports no other internal package. A doc with no frontmatter block, unparsable YAML, wrong field types, or a `covers` glob that is invalid, not repo-relative, or too costly to match is reported and skipped, never fatal, because phase 1 must work on existing docs it does not restructure; a missing `title` or `summary` is not an error. PR content is untrusted: the walk reads only regular files under 1 MiB, never symlinks. A section runs from its heading to the next heading of the same or higher level, so replacing a `##` section replaces its `###` subsections too; this is the unit a proposal replaces. Only `#` headings start sections; setext underlines and `#` lines inside code fences are body text. Duplicate headings are allowed.

## Flow (phase 1 target)

1. A PR is opened or updated; GitHub sends `pull_request` to the backend.
2. The backend lists the PR's changed files; the analysis runner maps them to docs through each doc's `covers` globs on its own checkout of the head commit (see [0002](decisions/0002-docs-structure.md)).
3. The LLM compares the diff with those docs and returns "no impact" or proposed edits.
4. The backend sets the `docs-agent` check: `success` for no impact, `action_required` with the proposal otherwise.
5. A developer applies, edits, or waives the proposal; the check turns green and the PR can merge.

## Phases

1. In-repo `docs/`, hard-coded default structure, merge check. See [0001](decisions/0001-in-repo-docs-first.md).
2. Frontend; structure becomes configurable.
3. One-way sync from repo docs to Notion.
4. Custom structures defined by users with their own prompt.
