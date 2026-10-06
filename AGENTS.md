# pollux

A GitHub App that reviews pull requests, proposes updates to the repo's docs, and blocks the merge until docs are updated or waived.

## Commands

- `make check`: lint, test, and vulnerability scan. Run before every commit.
- `make test`, `make lint`, `make fmt`, `make run`: see [docs/guides/setup.md](docs/guides/setup.md).

## Docs

Read [docs/README.md](docs/README.md) first; it indexes every doc. Before editing a file, find the docs whose `covers` globs match it and read those. Update them in the same change when behavior they describe changes.

## Layout

- `backend/`: Go module. `cmd/server` wires; `internal/config` is the only package that reads the environment; `internal/httpapi` is transport: it turns webhooks into jobs; `internal/jobs` owns job kinds, keys, and payloads, decodes claimed jobs back into `gate` calls, and runs the deadline sweep; `internal/jobqueue` is the durable per-PR queue and worker; `internal/gate` is the domain (what check run and proposal comments a PR gets, and the per-repo docs scaffold flow), imports only `internal/review` (contracts shared with the analysis runners), and declares the `GitHub`, `ScaffoldQueue`, and `Store` interfaces it needs; `internal/github` implements `GitHub` over the GitHub API; `internal/jobs` also implements `gate.ScaffoldQueue` by enqueueing repo-keyed scaffold jobs through the worker; `internal/gate/sqlite` is an adapter implementing `gate.Store` and `jobqueue.Store`; `internal/docs` parses a `docs/` tree and matches changed files to docs by `covers`, and imports no other internal package; `internal/llm` is the provider-neutral `Model` interface with OpenAI-compatible and Anthropic adapters; `internal/agent` is the read-only tool loop over `os.Root` with step, token, and deadline limits; `internal/review/basedocs` picks the candidate docs from `covers` at the PR's merge base, and proposes restoring a covering doc the PR deleted, for both runners; `internal/review/llmrunner` implements `review.Runner` for the server (clone, triage, agent loop, verification); `internal/review/actions` implements `review.AsyncRunner` for the Actions runner (dispatch the repo's workflow, read back and validate its result artifact, fill each proposal's section text from the doc at head) and reaches GitHub only through its own `WorkflowAPI` interface; neither `llm` nor `agent` imports the domain; `cmd/genschema` generates the proposal, result, and scaffold schemas in `action/`; `cmd/healthcheck` is the compose health probe (the image has no curl). Test support: `internal/e2e` holds the whole-system tests (webhook → queue → gate → store → runner); `internal/gate/gatetest`, `internal/gate/sqlite/sqlitetest`, `internal/llm/llmtest`, and `internal/gitfixture` are the shared fakes and fixtures, imported only by tests.
- `Dockerfile`, `compose.yaml`, `deploy/`: the single-host production deploy and its `pollux-deploy` script; see [docs/guides/deploy.md](docs/guides/deploy.md).
- `frontend/`: phase 2, TypeScript + shadcn/ui. Not created yet.
