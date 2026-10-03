# docs-agent

A GitHub App that reviews pull requests, proposes updates to the repo's docs, and blocks the merge until docs are updated or waived.

## Commands

- `make check`: lint, test, and vulnerability scan. Run before every commit.
- `make test`, `make lint`, `make fmt`, `make run`: see [docs/guides/setup.md](docs/guides/setup.md).

## Docs

Read [docs/README.md](docs/README.md) first; it indexes every doc. Before editing a file, find the docs whose `covers` globs match it and read those. Update them in the same change when behavior they describe changes.

## Layout

- `backend/`: Go module. `cmd/server` wires; `internal/config` is the only package that reads the environment; `internal/httpapi` is transport; `internal/gate` is the domain (what check run a PR gets), imports only `internal/review` (contracts shared with the analysis runners), and declares the `GitHub` interface it needs; `cmd/genschema` generates `action/proposal.schema.json`; `internal/github` implements it over the GitHub API.
- `frontend/`: phase 2, TypeScript + shadcn/ui. Not created yet.
