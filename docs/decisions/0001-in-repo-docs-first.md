---
title: "0001: In-repo docs first"
summary: Why phase 1 targets a docs/ folder in the repo instead of Notion.
covers: []
---

# 0001: In-repo docs first

**Status:** accepted, 2026-09-30

## Context

The goal is docs that stay current with code, gated at merge time. StartMunich keeps project docs in Notion, but writing to an external system from PR events is harder to review and riskier: PR content is untrusted and could steer the agent into editing pages.

## Decision

Phase 1 targets a `docs/` folder in each repo. Doc changes land in the same PR as the code and get reviewed as a normal diff. Notion becomes a one-way mirror of the repo docs in phase 3.

## Consequences

- Reviewable, revertible doc changes with no external writes in phase 1.
- The same docs serve coding agents (Claude Code, Cursor) through `AGENTS.md`.
- The docs open as an Obsidian vault with no conversion.
- Notion users see nothing until phase 3.
