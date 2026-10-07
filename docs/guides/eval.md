---
title: Eval
summary: Measure either runner's docs-impact quality against labeled cases from this repo's history, and add new cases.
covers:
  - eval/**
  - Makefile
  - action/run-claude.sh
  - backend/internal/config/config.go
  - backend/internal/review/llmrunner/eval_*
---

# Eval

The eval runs the [server runner](../features/server-runner.md) or the [Actions runner](../features/actions-runner.md) against real changes from this repo's history whose right answer is known, and scores what it proposes. Run it after changing a prompt, a model, or the pipeline, and compare against the previous run.

## Run

| Command           | Does |
|-------------------|------|
| `make eval`       | Runs every case against the configured model and writes a report. Costs model calls. |
| `make eval-check` | Validates the case files and the scorer, and runs the Actions path against a stub `claude`. No model, no key. CI runs it too, with full history. |

`EVAL_RUNNER` picks which runner is scored:

- `server` (default): the server runner, with the same `LLM_PROVIDER`, `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL`, and `LLM_TRIAGE_MODEL` as the server (see [Setup](setup.md)). Costs API calls.
- `actions`: the [Actions runner](../features/actions-runner.md), run on this machine instead of in GitHub Actions. It needs `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`, so it runs on a Claude subscription) or `ANTHROPIC_API_KEY`, plus `claude`, `jq`, `awk`, and `bash` on `PATH`. Set it in `backend/.env` (see `backend/.env.example`) and export it: `CLAUDE_CODE_OAUTH_TOKEN=… EVAL_RUNNER=actions make eval`.

| Variable           | Default       | Meaning |
|--------------------|---------------|---------|
| `EVAL_RUNS`        | 3             | Runs per case. Model output varies, so a case reports a pass rate. |
| `EVAL_PARALLEL`    | 2             | Runs in flight at once. Lower it on rate-limited tiers. |
| `EVAL_CASE`        | all           | Comma-separated case ids to run. |
| `EVAL_JUDGE_MODEL` | `LLM_MODEL`; Claude Code's default for `actions` | Model that checks whether proposals state the expected facts. With `actions` the judge also runs through `claude -p`, so nothing needs an API key. |

The run needs this repo's full history: cases name commits by SHA. Each run writes `eval/results/<time>-<model>/` (`<time>-actions-<judge>/` for `actions`) with `report.json`, `summary.md` (also printed), and per-run logs under `logs/`: runner logs for `server`, the action's output and Claude transcript for `actions`. The summary shows deltas against the newest earlier report in that folder. Results stay local (`eval/results/` is ignored by git): they hold model output and are only compared on the machine that ran them. Reports never hold credentials or local paths: a config field that holds either is redacted or left out of `report.json`.

## How a case is run

A case names a `base` and a `head` commit. The harness builds the input head from `head` with the whole `docs/` tree reset to `base`, so the runner sees the code change without the doc update the human made, and the changed files come from git in the shape GitHub's PR files API returns. The server runner clones that local repo instead of GitHub; nothing else about the pipeline changes.

The Actions runner runs unchanged too: a local stand-in for the GitHub Actions API answers its dispatch by running `action/run-claude.sh`, the same script the action's "Run Claude" step runs, over a checkout of the input head, then hands back the `result.json` it wrote. Each run gets an empty `HOME` and a minimal environment, so your own Claude Code settings, hooks, and memory never load, as on a fresh CI runner. `make eval-check` covers this path with a stub `claude`.

## Scoring

A run passes when all of these hold:

- **Verdict**: "no impact" or proposals, as labeled.
- **Recall**: every required doc gets a proposal, a new-doc proposal where the label says `new`.
- **Section**: the proposal replaces one of the label's acceptable headings.
- **Precision**: every proposal targets a required doc or one in `allow`.
- **Facts**: a judge model confirms the proposed content states each `must_say` fact and contradicts nothing.

A no-impact case passes on the verdict alone. A borderline case uses `verdict: either`: "no impact" passes, and so do proposals that stay inside its listed docs and sections (a listed doc without sections allows any section; `allow` and `must_say` are not used); it counts in the overall pass rate but in neither the impact nor the no-impact rate. Low scores never fail the command; only a broken case or harness does.

## Adding a case

Add `eval/cases/<id>.yaml`; the schema is in any existing case. Good sources are merged PRs that changed code and docs together (the docs diff is the label) and code-only commits whose docs were fixed later. Keep a share of no-impact cases, since history leans toward changes that needed docs. Write `must_say` as single claims the code at `head` supports, not the human's wording or operational advice the diff does not show; use `either` when both "no impact" and a small edit are defensible, and put docs that may change but need not in `allow`. Run `make eval-check` before committing.
