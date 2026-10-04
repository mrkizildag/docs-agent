---
title: Server runner
summary: How the server decides whether a PR makes docs stale with only an LLM key, and why it is built to resist prompt injection.
covers:
  - backend/internal/llm/**
  - backend/internal/agent/**
  - backend/internal/review/llmrunner/**
---

# Server runner

The server runner is the analysis runner for repos without the docs-agent Actions workflow. It is on when `LLM_PROVIDER` is set; see [Setup](../guides/setup.md). It implements the runner contract from `internal/review` and returns either "no impact" with a reason or a list of validated proposals.

## Pipeline

1. **Clone.** A depth-1 clone of the PR head commit, authenticated with the installation token. The token goes to git through its environment as an HTTP header, never in the remote URL, so it does not land in `.git/config` or in logs. `git` must be on the server's PATH.
2. **Triage.** One call per candidate doc to the triage model, given the PR's diff and the doc. Each answers impacted or not, with a reason. If every doc is "no", the result is "no impact" carrying those reasons, and nothing further runs. Most PRs end here, which keeps them cheap.
3. **Agent loop.** For impacted docs, the main model reads the clone through tools and finishes by calling `submit_proposals`. Its arguments must pass proposal validation (path under `docs/`, anchor inside a diff hunk). A submission that fails is not returned: the model is told why and may resubmit while steps remain.
4. **Verification.** One call per proposal asks whether it is right. Rejected proposals are dropped; if all are dropped, the result is "no impact".

The diff comes from GitHub's per-file patches, not from git in the clone, so anchors agree with what GitHub shows. The clone exists only for the model to read surrounding code.

## Why read-only tools rooted in the clone

PR content (code, comments, docs, commit messages) is attacker-controlled text that the model reads. Safety comes from what the model can do, not from asking it nicely. The only tools are `read_file`, `grep`, `list_dir`, and the finishing `submit_proposals`: there is nothing to write, execute, or fetch, so an injected instruction has nothing to call. File access goes through `os.Root` on the clone, so `../`, absolute paths, and symlinks pointing out are refused by the OS-level check, not by string filtering. A refused call returns a tool error to the model and the run continues.

The agent and LLM packages know nothing about GitHub or the review domain; the import boundary is enforced by depguard in `backend/.golangci.yml`.

## Limits

A run has a step cap, a token budget, and a deadline that leaves room inside the 3-minute target for GitHub calls. They are checked every step. Hitting any of them is an analysis failure, not a result: the error names which limit, and no partial proposals are returned. The gate decides how to report the failure (see the neutral-check work tracked in the repo issues). The cap values are placeholders until the free-tier spike reports real step and token counts.

A newer push cancels the running job; the context reaches every model call and git command.

## Providers

Two adapters speak the providers' HTTP wire formats: OpenAI-compatible chat completions (Gemini, OpenRouter, Ollama, and others) and Anthropic's Messages API. Free-tier models are weaker at tool use, so malformed tool arguments are returned to the model as errors instead of crashing the run.

Adapters must not trust the status code. GitHub Models was retired on 2026-07-30 and its endpoint now answers `200 text/plain "OK"`. A body that is not JSON, or has no `choices`/`content`, is an error.

Free tiers may train on inputs. Use them only against the sandbox repo.

## Current limitation

Candidate docs come from matching changed files against each doc's `covers` globs, which is not built yet. Until it lands, requests carry no candidate docs, so every real run returns "no impact" with no model call. Only fakes exercise the full pipeline.
