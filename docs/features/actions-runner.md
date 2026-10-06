---
title: Actions runner
summary: How analysis runs in the target repo's GitHub Actions and the invariants that keep its result trustworthy and its check from sticking.
covers:
  - backend/internal/review/actions/**
  - backend/internal/gate/**
  - backend/internal/httpapi/**
  - backend/internal/github/client.go
  - backend/internal/github/contents.go
  - backend/internal/review/basedocs/**
  - backend/internal/github/docsatref.go
  - backend/internal/github/mergebase.go
---

# Actions runner

An analysis runner for repos that carry the pollux-agent workflow. The server never calls an LLM for these repos; the repo's own Actions run Claude Code with the repo's credential. Setup: [Using the Actions runner](../guides/actions-runner.md). The same workflow writes a repo's first `docs/` when dispatched with PR number 0; see [Docs scaffold](scaffold.md).

## Flow

1. A push to a PR reaches the gate. The server computes the candidate docs (those whose `covers` at the merge base, the base-branch commit the PR's diff starts from, match a changed file) and dispatches the workflow on the **default branch**, passing the head SHA, PR number, a fresh nonce, and the candidates. Dispatching the default branch's copy means a PR cannot change the workflow, its tools, or its secrets.
2. Before dispatching, the gate creates the in-progress check and saves an armed run (a placeholder nonce, head SHA, 10-minute deadline) on the PR's state. Once GitHub accepts the dispatch, the gate replaces it with the run ID, the dispatched nonce, and the runner's deadline. A dispatch error therefore still leaves a check and a run for the deadline sweep to close.
3. The workflow installs Claude Code, checks out the head commit into a subdirectory, and runs Claude Code, told to review the candidate docs, then uploads a result artifact (even when the agent step fails; earlier steps failing leave nothing to upload).
4. A `workflow_run` completed event arrives. The run ID maps to its PR.
5. The GitHub adapter only fetches the run's artifact zip; the runner (`review/actions`) unzips it, picks `result.json`, and caps the zip and the file at 10 MiB, so a missing or expired artifact, an oversized one, or a zip without `result.json` is a failed read, not a result. The gate accepts it only if the run is the one dispatched for the PR's current head and the artifact's head SHA and nonce match.
6. Proposals outside `docs/` or not matching the schema are rejected. For each proposal that replaces a section, the server fetches that doc at the head SHA (once per doc) and fills the proposal's original section text and line range, so comments show the old text and can become suggestions; a doc missing at head, a doc that does not parse, or a heading that is not exactly one match is an invalid result; only a doc too large to read (`review.ErrFileTooLarge`) leaves both empty. Otherwise the same transition as the server runner concludes the check: success ("No doc impact" with a reason) or action_required with the proposals.

## Invariants

- **Match by run ID, never SHA.** Runs report the default branch's SHA, not the PR head, so the head SHA cannot identify a run.
- **One ending path.** Server-runner results, artifact results, failed runs, and missed deadlines all conclude the check through the same transition.
- **Never stuck in progress.** A failed, cancelled, or timed-out run, an invalid result, a result the server cannot read, a dispatch error, a failed or oversized read of the docs (before dispatch the server reads every `.md` and `.mdx` under `docs/` at the merge base through the Git Data API, only that subtree with blobs cached by SHA; more than 500 docs or a failed read ends neutral "Analysis failed" without dispatching), or no result before the deadline ends neutral with a fixed one-line cause naming which; the artifact's own error text stays in the server log, since it can carry model output. A periodic sweep closes any armed run past its deadline neutral and retries with backoff if GitHub rejects the write (see [Job queue](job-queue.md)). The concluded state is saved before comments are posted; if posting still fails after retries the run is re-armed and the sweep ends the check neutral with a Re-run box, so a check never claims proposals that were never posted.
- **Size limits first.** A PR over the limits (see [Server runner](server-runner.md)) ends neutral "PR too large to analyze" before any review dispatch. More than 10 candidate docs also ends neutral, as in the server runner, without dispatch.
- **A new analysis supersedes.** Any new analysis of a pull request that awaits a run (a push, or a repeat event for the same head) first concludes the old in-progress check neutral ("Superseded by <short sha>", or "Superseded by a re-run" when the head is unchanged), then clears the pending run and dispatches the new head; the old run's late result matches nothing and is ignored. Events for any other run, a stale nonce, or a different head SHA change nothing.
- **Base candidates.** Matching `covers` at head would let a PR empty or narrow them to opt itself out, so the candidates come from the merge base. The run's "No doc impact" is trusted, the same trust the server runner gives triage. The workflow is still dispatched when no doc covers a changed file. A candidate the PR deletes concludes the check with a restore proposal and no dispatch (see [Server runner](server-runner.md)).
- **Read-only agent.** Claude Code gets Read, Grep, and Glob over the checked-out head commit and the diff file, nothing else is pre-approved; other tools are blocked, not merely unapproved, because pre-approval alone doesn't stop the model from trying them.
- **Auth failures are errors.** A rejected credential still reports a successful subtype, so the action treats Claude's error flag as failure and the check ends neutral with a fixed one-line cause naming which.
- **Isolated agent.** Claude Code is installed before the checkout, from a directory with no PR content, and runs in an empty directory outside the checkout. Project settings, hooks, `CLAUDE.md`, and MCP servers from the PR are never loaded (`--setting-sources user`, `--strict-mcp-config`), and reads outside the checkout and the diff are denied, so PR content cannot run code with the credential or read it from the runner.
- **Dispatch inputs change together.** An input is added in Dispatch (server), `action/action.yml`, `action/pollux-agent.yml`, and the setup guide's YAML at once. GitHub rejects an undeclared input, so a repo on an older workflow copy fails every review until it re-copies the workflow. The artifact name, result file name, dispatch input names, and result envelope fields are constants in `review/actions`, and a test pins them to both YAML files.
