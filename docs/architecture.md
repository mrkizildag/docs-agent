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

- **Backend** (`backend/`, Go): receives GitHub webhooks, talks to the GitHub and LLM APIs, owns the check run. `POST /webhook` verifies GitHub's signature; a `pull_request` event (opened, synchronize, reopened) picks an analysis runner for the repo and reports its result as the `docs-agent` check run on the PR head commit. The backend handles it synchronously within the request until a durable job queue lands.
- **Frontend** (phase 2, TypeScript + shadcn/ui): org and repo settings, configurable docs structures.
- **Doc targets**: phase 1 writes to the repo's own `docs/` folder. Notion comes in phase 3.

Inside the backend, `internal/gate` is the domain and imports only `internal/review`, the contracts shared with the analysis runners. It declares the GitHub interface it needs; `internal/github` implements it and `internal/httpapi` calls `gate.Service`. Only `cmd/server` wires concrete types together.

Runner selection: a repo that has the docs-agent Actions workflow on its default branch runs analysis through the Actions runner; otherwise, if the server has `LLM_PROVIDER` configured, it runs through the server runner; otherwise the PR gets a neutral check titled "No analysis runner configured" linking the setup guide. No runner is wired into the server today, so every PR currently gets that neutral check. A repo with the workflow never falls back to the server runner.

Analysis contract: `internal/review` defines what a runner is asked to review and the proposals it returns. `action/proposal.schema.json` is generated from the proposal type by `make generate` and checked in; never edit it by hand, a test fails when it drifts. The schema checks shape and the `docs/` prefix; proposal validation in `internal/review` is the full check (anchor inside a diff hunk, one-line reason, index entry exactly when the proposal creates a new doc), and every runner's output must pass it before the gate acts on it.

Docs model: `internal/docs` reads the `docs/` tree of a checkout into docs (frontmatter and sections) and matches changed files to docs by their `covers` globs, relative to the repo root with `**` crossing folders. It takes a file system, never calls GitHub, and imports no other internal package. A doc with no frontmatter block, unparsable YAML, wrong field types, or a `covers` glob that is invalid, not repo-relative, or too costly to match is reported and skipped, never fatal, because phase 1 must work on existing docs it does not restructure; a missing `title` or `summary` is not an error. PR content is untrusted: the walk reads only regular files under 1 MiB, never symlinks. A section runs from its heading to the next heading of the same or higher level, so replacing a `##` section replaces its `###` subsections too; this is the unit a proposal replaces. Only `#` headings start sections; setext underlines and `#` lines inside code fences are body text. Duplicate headings are allowed.

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
