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
| `make run`   | Start the backend on `:8080`; check `GET /healthz`.    |
| `make test`  | `go test -race ./...`                                  |
| `make lint`  | golangci-lint, including formatting and import rules.  |
| `make fmt`   | Apply gofmt and goimports.                             |
| `make check` | lint, test, and govulncheck; what CI runs.             |

## Configuration

Only `backend/internal/config` reads the environment (a lint rule enforces it). Copy `backend/.env.example` for the variables.

| Variable    | Default | Meaning                                  |
|-------------|---------|------------------------------------------|
| `ADDR`      | `:8080` | Listen address.                          |
| `LOG_LEVEL` | `info`  | `debug`, `info`, `warn`, or `error`.     |
