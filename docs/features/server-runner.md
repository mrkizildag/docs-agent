---
title: Server runner
summary: How the server decides whether a PR makes docs stale with only an LLM key, and why it is built to resist prompt injection.
covers:
  - backend/internal/llm/**
  - backend/internal/agent/**
  - backend/internal/review/llmrunner/**
  - backend/internal/github/files.go
---

# Server runner

The server runner is the analysis runner for repos without the docs-agent Actions workflow. It is on when `LLM_PROVIDER` is set; see [Setup](../guides/setup.md). It implements the runner contract from `internal/review` and returns either "no impact" with a reason or a list of validated proposals.

## Pipeline

1. **Changed files.** The gate lists the PR's files from GitHub, each with its patch and the head-side line ranges of its hunks, and passes them in the request. No changed files means "no impact" without cloning.
2. **Clone.** A depth-1 clone of the PR head commit, authenticated with the installation token. The token goes to git through its environment as an HTTP header, never in the remote URL, so it does not land in `.git/config` or in logs. `git` must be on the server's PATH.
3. **Candidate docs.** The runner parses `docs/` in the clone and keeps the docs whose `covers` globs match a changed file (see [Architecture](../architecture.md) for the docs model). Reading docs at the head commit means a doc the PR adds or edits is judged as the PR leaves it. No candidate means "no impact" without a model call; a doc that fails to parse is skipped.
4. **Triage.** One call per candidate doc to the triage model, given the PR's diff and the doc. Each answers impacted or not, with a reason. If every doc is "no", the result is "no impact" carrying those reasons, and nothing further runs. Most PRs end here, which keeps them cheap.
5. **Agent loop.** For impacted docs, the main model reads the clone through tools and finishes by calling `submit_proposals`. Its arguments must pass proposal validation (path under `docs/`, anchor inside a diff hunk). A submission that fails is not returned: the model is told why and may resubmit while steps remain.
6. **Verification.** One call per proposal asks whether it is right. Rejected proposals are dropped; if all are dropped, the result is "no impact".
7. **Section text.** For each surviving proposal the runner attaches the section's current text and its head-side line range from the clone (empty for a new doc). The model never supplies them; the gate uses them to render comments (see [Proposal output](proposal-output.md)).

The diff comes from GitHub's per-file patches, not from git in the clone, so anchors agree with what GitHub shows. The clone is for reading docs and the code around the change.

## Why read-only tools rooted in the clone

PR content (code, comments, docs, commit messages) is attacker-controlled text that the model reads. Safety comes from what the model can do, not from asking it nicely. The only tools are `read_file`, `grep`, `list_dir`, and the finishing `submit_proposals`: there is nothing to write, execute, or fetch, so an injected instruction has nothing to call. File access goes through `os.Root` on the clone, so `../`, absolute paths, and symlinks pointing out are refused by the OS-level check, not by string filtering. A refused call returns a tool error to the model and the run continues.

Everything taken from the PR (patch, doc text, the proposal under verification) reaches the model inside markers carrying a per-run random nonce, and every system prompt says text inside them is data. That lowers the odds of injection; it does not remove them, which is why the tools stay read-only and a human still applies every edit. Git itself runs on attacker-controlled content, so it gets a minimal environment: no server secrets, no system or global git config (so no host-configured filters or hooks), no terminal prompts, HTTPS only, and a token narrowed to the one repo with `contents: read`. Prompt inputs are capped (64 KiB per doc, 128 KiB of patch, with a visible truncation note) and more than 10 candidate docs is an analysis failure, so a PR cannot buy unbounded model spend. A file whose patch GitHub omits (large or binary) is shown to the model as omitted, not as an empty diff, and a renamed file matches docs covering its old path too.

The agent and LLM packages know nothing about GitHub or the review domain; the import boundary is enforced by depguard in `backend/.golangci.yml`.

## Limits

A run has a step cap, a token budget, and a deadline that leaves room inside the 3-minute target for GitHub calls. They are checked every step. Hitting any of them is an analysis failure, not a result: the error names which limit, and no partial proposals are returned. The gate decides how to report the failure (see the neutral-check work tracked in the repo issues). The caps (12 steps, 120k tokens counting thinking tokens, 150 s) come from a Gemini free-tier spike where runs took 1–3 steps and under 50k tokens. Three empty replies in a row (Gemini's malformed function calls) also end the run.

A newer push cancels the running job; the context reaches every model call and git command.

## Providers

Two adapters speak the providers' HTTP wire formats: OpenAI-compatible chat completions (Gemini, OpenRouter, Ollama, and others) and Anthropic's Messages API. Free-tier models are weaker at tool use, so malformed tool arguments are returned to the model as errors instead of crashing the run. In the spike, half the proposals anchored on unchanged lines or named sections loosely; validation sends those back to the model, and a section must name an existing heading. Gemini 3 attaches a thought signature to each tool call that must be sent back on the next turn, and counts thinking tokens only in the total.

Adapters must not trust the status code. GitHub Models was retired on 2026-07-30 and its endpoint now answers `200 text/plain "OK"`. A body that is not JSON, or has no `choices`/`content`, is an error.

Free tiers may train on inputs. Use them only against the sandbox repo.
