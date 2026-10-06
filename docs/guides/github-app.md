---
title: Registering the GitHub App
summary: Create the pollux-agent GitHub App, point it at the backend, and install it.
covers:
  - backend/internal/config/**
  - backend/internal/httpapi/**
  - backend/internal/github/**
---

# Registering the GitHub App

pollux runs as a GitHub App. This covers creating the App, wiring its credentials into the backend's config, and installing it on a repo.

Use the StartMunich org for production. For local development, create a separate personal App (e.g. `pollux-agent-dev`) against a sandbox repo, so test deliveries never reach production.

## Webhook URL and secret

GitHub needs a reachable HTTPS URL to deliver events to `POST /webhook`. For local dev, expose the backend with `tailscale funnel 8080` and use the resulting Tailscale Funnel hostname: `https://<host>/webhook`. For production, the URL is `https://<host>.<tailnet>.ts.net/webhook` from the Funnel set up in [Deploy](deploy.md).

Generate the webhook secret with `openssl rand -hex 32`. Set the same value in the App's webhook secret field and in `GITHUB_WEBHOOK_SECRET` (see [Setup](setup.md)). `POST /webhook` applies a global and per-client-IP rate limit before reading the body; over-limit requests get `429` and the body is not read. After that, the backend rejects any delivery whose `X-Hub-Signature-256` doesn't match this secret with `401`. GitHub does not automatically retry failed webhook deliveries.

## Permissions

Grant only what the bot uses:

- **Actions**: read & write — dispatches the pollux-agent workflow in repos that use the [Actions runner](actions-runner.md), reads its run, and downloads its result artifact.
- **Checks**: read & write — sets the `pollux-agent` check run.
- **Contents**: read & write — fetches the PR head and merge base for the server runner (with a token narrowed to that repo and `contents: read`), reads `docs/` at the merge base for the Actions runner, detects the `.github/workflows/pollux-agent.yml` workflow, commits doc edits, creates the scaffold branch.
- **Pull requests**: read & write — lists the PR's changed files and patches; review comments and suggestions; opens the docs scaffold PR.
- **Issues**: read & write — the PR conversation comment and summary comment use the issues API.
- **Metadata**: read-only — mandatory for every App.

Nothing else.

## Events

Subscribe to: Pull request, Pull request review comment, Issue comment (commands, and ticked boxes on the summary comment: Apply all, Skip, Re-run analysis), Check run (GitHub's Re-run on the `pollux-agent` check), Workflow run (tells the backend a dispatched analysis run finished). No extra permission is needed for Re-run.

## Installation scope

Under "Where can this GitHub App be installed?", choose "Only on this account".

## After creating the App

1. Note the App ID shown on the App's settings page → `GITHUB_APP_ID`.
2. Generate a private key and download the `.pem` file. Save it outside the repo (`*.pem` is gitignored regardless); point `GITHUB_APP_PRIVATE_KEY_FILE` at its path.
3. Install the App on the target repositories.

## Required status check

Once installed, an org admin adds `pollux-agent` as a required status check in the repo's branch ruleset. The bot never creates or edits rulesets itself.

## Verify

Open the App's "Advanced → Recent deliveries" tab. The initial `ping` delivery should show a `202` response. Redeliver it after changing the webhook secret in the App settings (without updating `GITHUB_WEBHOOK_SECRET`) to confirm it now gets a `401`.

Open a pull request in the sandbox repo and confirm it shows a `pollux-agent` check. With no analysis runner configured, it is neutral and titled "No analysis runner configured".
