---
title: "0002: Docs structure"
summary: The folder layout and frontmatter every doc follows, and why.
covers: []
---

# 0002: Docs structure

**Status:** accepted as the starting point, 2026-09-30. We will try alternatives in a test repo before hard-coding it.

## Context

Agents work best with progressive disclosure: a small map first, then only the files the task needs. The agent also needs to know which docs a code change affects.

## Decision

```
docs/
  README.md        index: every doc with its one-line summary
  architecture.md  parts, connections, data flow
  features/        one file per feature: behavior and invariants
  guides/          how-to: setup, deploy, common changes
  decisions/       numbered records of why we chose X
```

Every doc starts with:

```yaml
---
title: Authentication
summary: How login, sessions and API tokens work.
covers:
  - internal/auth/**
---
```

- `covers` maps code to docs. A changed file matching a glob means that doc is a review candidate; an agent editing a file can look up the docs that apply.
- Links are relative markdown links so they work on GitHub and in Obsidian.
- The layout borrows from Diátaxis (how-to, reference, explanation) and drops tutorials, which internal projects rarely need.

## Consequences

- A doc with no `covers` is never flagged by code changes; that fits indexes and decisions.
- Overlapping globs are allowed; one file can be covered by several docs.
- Existing `docs/` folders are never restructured automatically; the agent offers a one-time adoption PR instead.
