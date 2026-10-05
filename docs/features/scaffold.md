---
title: Docs scaffold
summary: How a repo with no docs/ folder gets one starting docs PR, exactly once, and what the waiting PRs' checks say meanwhile.
covers:
  - backend/internal/gate/scaffold.go
  - backend/internal/gate/sqlite/scaffold.go
  - backend/internal/github/scaffold.go
  - backend/internal/review/scaffold.go
  - backend/internal/review/llmrunner/scaffold.go
  - backend/internal/review/llmrunner/prompt.go
  - backend/internal/review/actions/actions.go
  - backend/internal/httpapi/job.go
  - action/action.yml
  - action/scaffold.md
---

# Docs scaffold

A PR whose head has no `docs/` folder has nothing to analyze. It gets a neutral check titled "No docs/ folder" and no analysis. pollux then writes a starting `docs/` from the repo's code and opens one PR with it. With no analysis runner configured nothing can write it, so the summary says a runner is needed and links the [setup guide](../guides/actions-runner.md).

## One scaffold per repo, ever

The scaffold is repo-keyed state, not PR state. A row per repo in SQLite moves through phases: idle, writing (or awaiting, for the Actions runner), written, opened. `opened` is terminal: once a scaffold PR has been opened there is never a second one, even after the first is closed unmerged or merged. A maintainer who closes it has answered the question.

The work runs as a scaffold job keyed by repo, so a repo's scaffold jobs are serialized and kept apart from its PR jobs. Every PR that finds no `docs/` registers as waiting and enqueues the job. When the PR opens, every waiting PR's check is updated with its link.

Before writing, the job probes the default branch. If it already has `docs/` (the PR's base was just stale), nothing is written and the waiting checks say to merge or rebase.

## Writing

The writer is chosen like the review runner, see [Architecture](../architecture.md).

- **Actions workflow repos**: the gate dispatches the existing workflow with `pr_number` "0" and `head_sha` set to the default branch tip. The action treats PR number 0 as scaffold mode and writes the files instead of reviewing a diff. The workflow file does not change. Installed workflows reference `action@main`, so the action change must reach `main` before the server that dispatches scaffolds is deployed.
- **Server runner**: the LLM agent runs over a clone of the default branch with the same read-only tools as review, confined to the clone, and with larger step, token, and time limits since it reads the whole repo.

The output is exactly three files: `docs/README.md`, `docs/architecture.md`, `docs/guides/setup.md`. Each needs frontmatter `title`, `summary`, and `covers`, and the index must link the other two. The result type has no field for any other path, so a model cannot write outside these files.

## Opening the PR

The branch `pollux-agent/docs-scaffold` is created off the default tip, with one commit and one PR into the default branch. The validated files are stored as soon as they are written, so a failed commit or PR step retries without running the model again. A crash after the branch or PR exists is recovered by adopting them, not creating duplicates.

Any failure before the PR exists, including a workflow run that fails, is cancelled, or passes its deadline, leaves the repo's state retryable: the next PR event on a docs-less PR tries again.

## Permissions

Creating the branch and the PR uses the App's Contents write and Pull requests write permissions, see [Registering the GitHub App](../guides/github-app.md).
