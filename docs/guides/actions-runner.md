---
title: Using the Actions runner
summary: Set up a repo so pollux analysis runs in its own GitHub Actions with its own Claude credential.
covers:
  - action/**
---

# Using the Actions runner

With the Actions runner, your repo runs the analysis in its own GitHub Actions using your own Claude credential. The pollux server makes no LLM calls for the repo, so a team can use its Claude subscription or API key without giving the server a key. The check, proposals, and safety guarantees are the same as the server runner's. How it works: [Actions runner](../features/actions-runner.md).

## Setup

1. Install the pollux-agent GitHub App on the repo (see [Registering the GitHub App](github-app.md)).
2. Add `.github/workflows/pollux-agent.yml` on the default branch. The exact file lives at [action/pollux-agent.yml](../../action/pollux-agent.yml) in this repo; copy it from there.

   ```yaml
   on:
     workflow_dispatch:
       inputs: {head_sha: {required: true}, pr_number: {required: true}, nonce: {required: true}}
   permissions: {contents: read}
   jobs:
     pollux-agent:
       runs-on: ubuntu-latest
       steps:
         - uses: mrkizildag/pollux-agent/action@main
           with:
             head_sha: ${{ inputs.head_sha }}
             pr_number: ${{ inputs.pr_number }}
             nonce: ${{ inputs.nonce }}
             claude_code_oauth_token: ${{ secrets.CLAUDE_CODE_OAUTH_TOKEN }}
             anthropic_api_key: ${{ secrets.ANTHROPIC_API_KEY }}
   ```

3. Add one repo secret, either:
   - `CLAUDE_CODE_OAUTH_TOKEN`: run `claude setup-token` and copy only the single `sk-ant-oat01-...` line. A bad paste shows up as a neutral `pollux-agent` check whose cause is a 401.
   - `ANTHROPIC_API_KEY`: an Anthropic API key.

## Detection

No other config. Once the workflow exists on the default branch, the repo uses this runner and never the server runner. A workflow that exists only on a PR branch does nothing: the gate always dispatches the default branch's copy.

## Docs scaffold

The same workflow writes a repo's starting `docs/` when the PR has none: the server dispatches it with `pr_number` "0" and the default branch tip as `head_sha`, and the action runs in scaffold mode. No workflow change is needed. Because installed workflows use `action@main`, merge the action change to `main` before deploying a server that dispatches scaffolds. See [Docs scaffold](../features/scaffold.md).

## Subscription terms

Running an org-wide bot on one person's Claude subscription may not count as ordinary individual use under Anthropic's consumer terms. For team repos, prefer an `ANTHROPIC_API_KEY` secret.

## Latency

Expect about 30 seconds per PR: a spike measured 27 s from dispatch to result, about 20 s of it fixed Actions overhead. A run that fails or doesn't report in time ends the check neutral with the cause; it is never left in progress.
