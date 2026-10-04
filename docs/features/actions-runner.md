---
title: Actions runner
summary: How analysis runs in the target repo's GitHub Actions and the invariants that keep its result trustworthy and its check from sticking.
covers:
  - backend/internal/review/actions/**
  - backend/internal/gate/**
  - backend/internal/httpapi/**
  - backend/internal/github/contents.go
---

# Actions runner

An analysis runner for repos that carry the pollux-agent workflow. The server never calls an LLM for these repos; the repo's own Actions run Claude Code with the repo's credential. Setup: [Using the Actions runner](../guides/actions-runner.md).

## Flow

1. A push to a PR reaches the gate. It dispatches the workflow on the **default branch**, passing the head SHA, PR number, and a fresh nonce. Dispatching the default branch's copy means a PR cannot change the workflow, its tools, or its secrets.
2. GitHub returns the run ID. The gate stores the run ID, nonce, head SHA, and a deadline on the PR's state and puts the check in progress.
3. The workflow installs Claude Code, checks out the head commit into a subdirectory, and runs Claude Code, then uploads a result artifact (always, even on failure).
4. A `workflow_run` completed event arrives. The run ID maps to its PR.
5. The runner downloads the artifact. The gate accepts it only if the run is the one dispatched for the PR's current head and the artifact's head SHA and nonce match.
6. Proposals outside `docs/` or not matching the schema are rejected. For each proposal that replaces a section, the server fetches that doc at the head SHA (once per doc) and fills the proposal's original section text and line range, so comments show the old text and can become suggestions; a missing doc or section leaves both empty. Otherwise the same transition as the server runner concludes the check: success ("No doc impact" with a reason) or action_required with the proposals.

## Invariants

- **Match by run ID, never SHA.** Runs report the default branch's SHA, not the PR head, so the head SHA cannot identify a run.
- **One ending path.** Server-runner results, artifact results, failed runs, and missed deadlines all conclude the check through the same transition.
- **Never stuck in progress.** A failed or cancelled run (the cause adds the run's result error when its artifact says why, such as an authentication failure), an invalid result, or no result before the deadline ends neutral with the cause. A periodic sweep enqueues a deadline job for overdue pending PRs, deduplicated by nonce.
- **A new analysis supersedes.** Any new analysis of a pull request that awaits a run (a push, or a repeat event for the same head) first concludes the old in-progress check neutral ("Superseded by <short sha>"), then clears the pending run and dispatches the new head; the old run's late result matches nothing and is ignored. Events for any other run, a stale nonce, or a different head SHA change nothing.
- **Read-only agent.** Claude Code gets Read, Grep, and Glob over the checked-out head commit and the diff file, nothing else is pre-approved; other tools are blocked, not merely unapproved, because pre-approval alone doesn't stop the model from trying them.
- **Auth failures are errors.** A rejected credential still reports a successful subtype, so the action treats Claude's error flag as failure and the check ends neutral with the cause.
- **Isolated agent.** Claude Code is installed before the checkout, from a directory with no PR content, and runs in an empty directory outside the checkout. Project settings, hooks, `CLAUDE.md`, and MCP servers from the PR are never loaded (`--setting-sources user`, `--strict-mcp-config`), and reads outside the checkout and the diff are denied, so PR content cannot run code with the credential or read it from the runner.
