---
title: Setup
summary: Run, test, and lint the backend locally.
covers:
  - Makefile
  - backend/go.mod
  - backend/.golangci.yml
  - backend/internal/config/**
  - .github/workflows/**
---

# Setup

Requires Go (version in `backend/go.mod`) and golangci-lint v2. To run it in production, see [Deploy](deploy.md).

| Command      | Does                                                   |
|--------------|--------------------------------------------------------|
| `make run`   | Start the backend on `:8080` (needs the env vars below); check `GET /healthz`. |
| `make test`  | `go test -race ./...`                                  |
| `make lint`  | golangci-lint, including formatting and import rules. Its cache is per worktree, in `.cache/`. |
| `make fmt`   | Apply gofmt and goimports.                             |
| `make generate` | Regenerate the three schemas in `action/` (proposal, result, scaffold) from the `internal/review` types, and `action/prompt.md` and `action/scaffold.md` from `internal/review/instructions`; a test fails when any is stale. |
| `make generated` | Run that staleness test alone, uncached (`go test -count=1 ./cmd/genaction`), because the generated files live outside the Go module. |
| `make check` | lint, test, generated, and govulncheck; what CI runs, plus `make eval-check`. |
| `make eval`, `make eval-check` | Score a runner on labeled cases, or validate them offline; see [Eval](eval.md). |

## Configuration

Only `backend/internal/config` reads the environment (forbidigo enforces it). The one exemption is the server runner's `git` subprocess, which forwards `PATH` from the server's environment (see [Server runner](../features/server-runner.md)). Copy `backend/.env.example` for the variables. Secret variables load into `config.Secret`, which prints and logs as `[redacted]`; call `Reveal()` only where the value is handed to its consumer: `cmd/server`, and the eval harness (`llmrunner/eval_*_test.go`), which wires runners the same way.

| Variable                      | Default  | Meaning                                                        |
|-------------------------------|----------|-----------------------------------------------------------------|
| `ADDR`                        | `:8080`  | Listen address.                                                 |
| `LOG_LEVEL`                   | `info`   | `debug`, `info`, `warn`, or `error`.                            |
| `DATABASE_PATH`               | `pollux.db` | Path to the SQLite database holding webhook deliveries, the job queue, and PR state. Created on first start. |
| `GITHUB_APP_ID`                | required | The GitHub App's numeric ID.                                    |
| `GITHUB_APP_PRIVATE_KEY_FILE`  | required | Path to the App's private key `.pem` file. See [github-app.md](github-app.md). |
| `GITHUB_WEBHOOK_SECRET`        | required | Shared secret used to verify `POST /webhook` signatures.        |
| `LLM_PROVIDER`                 | optional | `anthropic` or `openai` (any OpenAI-compatible chat completions endpoint: Gemini, OpenRouter, Ollama). Unset turns the analysis runner off. |
| `LLM_BASE_URL`                 | optional | API base URL. Required for `openai`; optional for `anthropic` (empty uses the adapter's default). |
| `LLM_API_KEY`                  | optional | Key for the LLM provider. Required for `anthropic`; optional for `openai` (e.g. Ollama has none). |
| `LLM_MODEL`                    | required when `LLM_PROVIDER` is set | Model used to review docs impact. |
| `LLM_TRIAGE_MODEL`             | defaults to `LLM_MODEL` | Cheaper model used for triage, if different. |
| `GITHUB_CLIENT_ID`             | optional | The GitHub App's client ID, for Sign in with GitHub. Set this and the next three, or none; see [Sign in](../features/sign-in.md). |
| `GITHUB_CLIENT_SECRET`         | with `GITHUB_CLIENT_ID` | A client secret generated on the App's settings page. |
| `PUBLIC_URL`                   | with `GITHUB_CLIENT_ID` | The public https URL (the Funnel URL); `PUBLIC_URL/auth/callback` must be registered on the App. |
| `SESSION_KEY`                  | with `GITHUB_CLIENT_ID` | 32 random bytes, base64-encoded (`openssl rand -base64 32`); seals the stored GitHub tokens. Changing it signs everyone out. |

Without `LLM_PROVIDER` set, the analysis runner is off, and repos without the pollux-agent workflow get a neutral "No analysis runner configured" check. With it set, `git` must be on the PATH: the server runner clones the PR head. See [Server runner](../features/server-runner.md).

### Example: Gemini free tier

```
LLM_PROVIDER=openai
LLM_BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai
LLM_API_KEY=<key from https://aistudio.google.com/apikey>
LLM_MODEL=gemini-3.7-flash
LLM_TRIAGE_MODEL=gemini-3.5-flash-lite
```

The free tier may train on your inputs, so use it only with a sandbox repo. It allows about 20 requests per model per day (roughly five PRs) and often answers 503 under load, so it suits development, not a team. These models were measured on 2026-10-04; see [Server runner](../features/server-runner.md).

See [Registering the GitHub App](github-app.md) for how to get the App ID, private key, and webhook secret.
