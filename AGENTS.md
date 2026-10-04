# docs-agent

A GitHub App that reviews pull requests, proposes updates to the repo's docs, and blocks the merge until docs are updated or waived.

## Commands

- `make check`: lint, test, and vulnerability scan. Run before every commit.
- `make test`, `make lint`, `make fmt`, `make run`: see [docs/guides/setup.md](docs/guides/setup.md).

## Docs

Read [docs/README.md](docs/README.md) first; it indexes every doc. Before editing a file, find the docs whose `covers` globs match it and read those. Update them in the same change when behavior they describe changes.

## Layout

- `backend/`: Go module. `cmd/server` wires; `internal/config` is the only package that reads the environment; `internal/httpapi` is transport: it turns webhooks into jobs and decodes them back into `gate` calls; `internal/jobqueue` is the durable per-PR queue and worker; `internal/gate` is the domain (what check run a PR gets), imports only `internal/review` (contracts shared with the analysis runners), and declares the `GitHub` and `Store` interfaces it needs; `internal/github` implements `GitHub` over the GitHub API; `internal/gate/sqlite` is an adapter implementing `gate.Store` and `jobqueue.Store`; `internal/docs` parses a `docs/` tree and matches changed files to docs by `covers`, and imports no other internal package; `cmd/genschema` generates `action/proposal.schema.json`.
- `frontend/`: phase 2, TypeScript + shadcn/ui. Not created yet.
