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

Requires Go (version in `backend/go.mod`) and golangci-lint v2.

| Command      | Does                                                   |
|--------------|--------------------------------------------------------|
| `make run`   | Start the backend on `:8080` (needs the env vars below); check `GET /healthz`. |
| `make test`  | `go test -race ./...`                                  |
| `make lint`  | golangci-lint, including formatting and import rules.  |
| `make fmt`   | Apply gofmt and goimports.                             |
| `make check` | lint, test, and govulncheck; what CI runs.             |

## Configuration

Only `backend/internal/config` reads the environment (a lint rule enforces it). Copy `backend/.env.example` for the variables. Secret variables load into `config.Secret`, which prints and logs as `[redacted]`; call `Reveal()` only in `cmd/server` where the value is handed to its consumer.

| Variable                      | Default  | Meaning                                                        |
|-------------------------------|----------|-----------------------------------------------------------------|
| `ADDR`                        | `:8080`  | Listen address.                                                 |
| `LOG_LEVEL`                   | `info`   | `debug`, `info`, `warn`, or `error`.                            |
| `DATABASE_PATH`               | `docs-agent.db` | Path to the SQLite database holding webhook deliveries, the job queue, and PR state. Created on first start. |
| `GITHUB_APP_ID`                | required | The GitHub App's numeric ID.                                    |
| `GITHUB_APP_PRIVATE_KEY_FILE`  | required | Path to the App's private key `.pem` file. See [github-app.md](github-app.md). |
| `GITHUB_WEBHOOK_SECRET`        | required | Shared secret used to verify `POST /webhook` signatures.        |
| `ANTHROPIC_API_KEY`            | required | Key for the LLM that reviews docs impact. Required at startup; unused until analysis lands. |

See [Registering the GitHub App](github-app.md) for how to get the App ID, private key, and webhook secret.
