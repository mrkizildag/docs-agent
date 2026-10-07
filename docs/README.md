---
title: Docs index
summary: Map of every doc in this repo and the conventions they follow.
covers: []
---

# Docs

Start here. Each entry is one file with a one-line summary; open only what the task needs.

## Index

- [Architecture](architecture.md): the parts of pollux, how they connect, and the phase plan.
- [Job queue](features/job-queue.md): how webhook work is queued, deduplicated, superseded, and recovered after a restart.
- [Actions runner](features/actions-runner.md): how analysis runs in the repo's own GitHub Actions, and the invariants that keep its result trustworthy and its check from sticking.
- [Server runner](features/server-runner.md): how the server decides whether a PR makes docs stale, why its tools are read-only, and its limits.
- [Proposal output](features/proposal-output.md): what a PR gets for proposed doc edits (review comments, summary) and how re-runs edit, retire, or replace them.
- [Apply and Skip](features/apply-skip.md): how proposals are applied or the gate waived, who may, what is refused, and why a repeated delivery commits at most once.
- [Analysis history](features/analysis-history.md): what is recorded for every analysis and Apply or Skip, why it is written with the state, and how replays stay idempotent.
- [Docs scaffold](features/scaffold.md): how a repo with no `docs/` gets one starting docs PR, exactly once, and what waiting checks say.
- [Sign in with GitHub](features/sign-in.md): how the dashboard knows who is asking and which repos they may see, and the session and access invariants.
- [Setup](guides/setup.md): run, test, and lint the backend locally.
- [Eval](guides/eval.md): measure either runner's docs-impact quality against labeled cases from this repo's history.
- [Sandbox repo](guides/sandbox.md): the private repo pollux is tested on end to end, what is set up there, and how to run a test PR.
- [Registering the GitHub App](guides/github-app.md): create the GitHub App, set its webhook and permissions, and install it.
- [Using the Actions runner](guides/actions-runner.md): set up a target repo so analysis runs in its own GitHub Actions with its own Claude credential.
- [Deploy](guides/deploy.md): run pollux in Docker on one host and expose its webhook through Tailscale Funnel.
- [0001: In-repo docs first](decisions/0001-in-repo-docs-first.md): why phase 1 targets a `docs/` folder, not Notion.
- [0002: Docs structure](decisions/0002-docs-structure.md): the folder layout and frontmatter every doc follows.

## Conventions

This folder is the default structure pollux creates in other repos, so it follows its own rules:

- One topic per file. Folders: `features/` (what a feature does and its invariants), `guides/` (how-to), `decisions/` (why we chose X, numbered).
- Every doc starts with frontmatter: `title`, `summary` (one line), `covers` (globs of the code it describes).
- Link docs with relative markdown links (`[x](../architecture.md)`). They render on GitHub and in Obsidian with wikilinks off and "New link format" set to "Relative path to file".
- Document what the code cannot say: why, data flow, invariants, external contracts. No file trees or signatures.
- Add every new doc to the index above.
