---
title: Sandbox repo
summary: The private repo where pollux is tested end to end on real PRs, what is set up there, and how to run a test PR.
covers:
  - action/pollux-agent.yml
---

# Sandbox repo

[`mrkizildag/docs-agent-sandbox`](https://github.com/mrkizildag/docs-agent-sandbox) is the repo pollux is tried on before anything reaches a real team. It is private. Use it for every end-to-end check, and for free-tier models, which may train on inputs.

## What is there

- **Ledgerline**, a stdlib-only Python subscription billing service (customers, plans, invoices, tax, dunning, webhooks, rate limiting), with a full `docs/` tree that follows pollux's conventions: about 30 docs with `covers`, an index, guides, and decisions. It is big enough that a change touches real docs and small enough to read.
- The **dev GitHub App** (`pollux-agent-dev`), installed on the repo only, so test deliveries never reach production; see [Registering the GitHub App](github-app.md).
- The **Actions runner** workflow at `.github/workflows/pollux-agent.yml`, a copy of [action/pollux-agent.yml](../../action/pollux-agent.yml), with a `CLAUDE_CODE_OAUTH_TOKEN` secret from `claude setup-token`. Analysis runs on a Claude subscription, not an API key; see [Using the Actions runner](actions-runner.md).

When the workflow's inputs change here, copy the file to the sandbox's `main` again: GitHub rejects a dispatch with an input the workflow does not declare, and every review fails neutral.

## Running a test PR

1. Start the dev server on the machine the dev App's webhook URL points at (`https://<host>.<tailnet>.ts.net/webhook`, set in the App's settings). From the main checkout, load `backend/.env` into the environment and run `make run`, then `tailscale funnel 8080`. Check `https://<host>.<tailnet>.ts.net/healthz` answers `ok`.
2. Branch off the sandbox's `main`, make a code change whose doc impact you know (for example, change what a documented default or rule does), and open a PR. Say the expected result in the PR body so the run can be judged.
3. The `pollux-agent` check appears within seconds; the workflow run takes about 30 seconds. Check the proposals, the summary comment, and Apply or Skip.

No check at all means the delivery never reached the server: the App's "Advanced → Recent deliveries" shows it failed (a `500` from Funnel when nothing listens). GitHub does not retry, so once the server is up, push an empty commit to the PR (`git commit --allow-empty`) to send a new delivery.

A PR that changes only tests or internals should end with no impact. Close test PRs when done; do not merge them, so `main` and its docs stay a known baseline.

Workflow runs on a private repo use the account's Actions minutes. Measuring quality across many changes is the [eval](eval.md)'s job, which needs no GitHub at all; the sandbox checks that the whole loop works.
