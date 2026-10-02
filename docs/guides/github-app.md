---
title: Registering the GitHub App
summary: Create the docs-agent GitHub App, point it at the backend, and install it.
covers:
  - backend/internal/config/**
  - backend/internal/httpapi/**
---

# Registering the GitHub App

docs-agent runs as a GitHub App. This covers creating the App, wiring its credentials into the backend's config, and installing it on a repo.

Use the StartMunich org for production. For local development, create a separate personal App (e.g. `docs-agent-dev`) against a sandbox repo, so test deliveries never reach production.

## Webhook URL and secret

GitHub needs a reachable HTTPS URL to deliver events to `POST /webhook`. For local dev, expose the backend with `tailscale funnel 8080` and use the resulting Tailscale Funnel hostname: `https://<host>/webhook`.

Generate the webhook secret with `openssl rand -hex 32`. Set the same value in the App's webhook secret field and in `GITHUB_WEBHOOK_SECRET` (see [Setup](setup.md)). The backend rejects any delivery whose `X-Hub-Signature-256` doesn't match this secret.

## Permissions

Grant only what the bot uses:

- **Checks**: read & write — sets the `docs-agent` check run.
- **Contents**: read & write — reads the diff, commits doc edits.
- **Pull requests**: read & write — review comments and suggestions.
- **Issues**: read & write — the PR conversation comment and summary comment use the issues API.
- **Metadata**: read-only — mandatory for every App.

Nothing else.

## Events

Subscribe to: Pull request, Pull request review comment, Issue comment, Check run.

## Installation scope

Under "Where can this GitHub App be installed?", choose "Only on this account".

## After creating the App

1. Note the App ID shown on the App's settings page → `GITHUB_APP_ID`.
2. Generate a private key and download the `.pem` file. Save it outside the repo (`*.pem` is gitignored regardless); point `GITHUB_APP_PRIVATE_KEY_FILE` at its path.
3. Install the App on the target repositories.

## Required status check

Once installed, an org admin adds `docs-agent` as a required status check in the repo's branch ruleset. The bot never creates or edits rulesets itself.

## Verify

Open the App's "Advanced → Recent deliveries" tab. The initial `ping` delivery should show a `202` response. Redeliver it after changing the webhook secret in the App settings (without updating `GITHUB_WEBHOOK_SECRET`) to confirm it now gets a `401`.
