---
title: Docs index
summary: Map of every doc in this repo and the conventions they follow.
covers: []
---

# Docs

Start here. Each entry is one file with a one-line summary; open only what the task needs.

## Index

- [Architecture](architecture.md): the parts of docs-agent, how they connect, and the phase plan.
- [Setup](guides/setup.md): run, test, and lint the backend locally.
- [0001: In-repo docs first](decisions/0001-in-repo-docs-first.md): why phase 1 targets a `docs/` folder, not Notion.
- [0002: Docs structure](decisions/0002-docs-structure.md): the folder layout and frontmatter every doc follows.

## Conventions

This folder is the default structure docs-agent creates in other repos, so it follows its own rules:

- One topic per file. Folders: `features/` (what a feature does and its invariants), `guides/` (how-to), `decisions/` (why we chose X, numbered).
- Every doc starts with frontmatter: `title`, `summary` (one line), `covers` (globs of the code it describes).
- Link docs with relative markdown links (`[x](../architecture.md)`). They render on GitHub and in Obsidian with wikilinks off and "New link format" set to "Relative path to file".
- Document what the code cannot say: why, data flow, invariants, external contracts. No file trees or signatures.
- Add every new doc to the index above.
