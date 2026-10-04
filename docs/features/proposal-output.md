---
title: Proposal output
summary: What a PR gets when analysis proposes doc edits (check run, one review comment per proposal, a summary comment) and how re-runs keep those comments stable.
covers:
  - backend/internal/gate/**
  - backend/internal/review/review.go
  - backend/internal/github/comments.go
---

# Proposal output

When a runner returns proposals, the gate reports them three ways on the PR: the `pollux-agent` check run (`action_required`, listing doc paths and reasons), one review comment per proposal on the head commit, and one summary comment on the PR. A "no impact" result sets the check to `success` with the reason and posts no comments. Acting on a proposal is not built yet: the "Apply this change" checkbox is rendered unticked and does nothing until #14 (Apply, Apply all, Skip, permissions, forks).

## Where the section text comes from

Each proposal carries the current text of the section it replaces and that text's head-side line range in the doc. The server runner fills both from its own checkout of the head commit (see [Server runner](server-runner.md)); the Actions runner fills them from the doc fetched at the head SHA through the contents API (see [Actions runner](actions-runner.md)); they are never model output and are kept out of the generated JSON schema. Both are empty for a new doc. The range runs from the heading through the section's last line, including trailing blank lines. The gate may import only `internal/review`, so it cannot parse docs itself; it trusts these fields. A proposal whose doc or section is missing at the head, or whose heading matches more than one section, leaves them empty, and its comment degrades to the checkbox variant; guessing the section would let a suggestion overwrite the wrong one.

A proposal's content replaces the whole section, heading line included, because a suggestion replaces the heading line too. Both runner prompts and the generated schema say so. If the content still starts without a heading of the section's level, the gate puts back the original heading and the blank lines after it before rendering; otherwise applying the edit would delete the heading from the doc.

## Two comment variants

- **Suggestion.** Used only when the whole section, trailing blank lines included, lies inside one head-side hunk of that doc in the PR diff. GitHub only accepts review comments on diff lines, so this is the one case where the comment can sit on the doc's own lines. It is a GitHub `suggestion` block replacing exactly those lines, with the reason and no checkbox. The suggestion restates the section's trailing blank lines, otherwise applying it would eat the gap before the next heading. The code fence is made longer than any backtick run in the content so proposed code fences cannot close it early.
- **Checkbox.** Used for everything else: the doc is not in the diff, the section only partly overlaps a hunk or spans two, or the doc is new. The comment sits on the proposal's anchor line (a changed code line) and holds the reason, the doc path and section, the edit as a `diff` block (old section lines `-`, new lines `+`; a new doc shows only added lines plus its index entry), and an unticked "Apply this change" task-list item.

## Identity and re-runs

A proposal's identity is its doc path plus its normalized section heading (leading `#` and whitespace stripped; path alone for a new doc), hashed to a short ID. The gate remembers each proposal's ID, comment ID and URL, and state (`open` or `outdated`), plus the summary comment ID, per PR.

On each run, after the check run is created, the gate reconciles the new proposals against what it remembers:

- **Same proposal again:** its comment is edited in place. The PR's comment count does not change across pushes.
- **New proposal:** a new review comment.
- **Proposal gone:** its comment and summary row are marked `outdated`; the old body stays readable under an "Outdated" notice. The gate never deletes a comment: that would erase the thread's replies and history, and a proposal that returns would have to start over. A proposal that returns after being outdated reopens its old comment.
- **No impact:** every earlier proposal is marked outdated and the check is `success`.
- **Summary:** one issue comment listing every proposal with doc path, section, a link to its comment, and state. It is edited in place, and only created when there is something to list or one already exists.

A comment keeps its location and variant across edits. GitHub cannot move a review comment, and replacing it would churn the thread. A suggestion block on the wrong lines would overwrite them when applied, so on edit the suggestion body is used only if the existing comment already sits on that doc at the same lines; otherwise the body is the checkbox variant. If a user deletes a proposal's comment, an open proposal gets a fresh comment, and an outdated one is skipped rather than resurrected.

## Markers and crash recovery

Every proposal comment starts with a hidden `<!-- pollux-agent:proposal:ID -->` and the summary with `<!-- pollux-agent:summary -->`. The gate lists the PR's comments before writing and, when its stored state lacks a comment ID, adopts the comment carrying the marker. It adopts or edits only comments written by the App's own bot user, with the marker as the body's first line: anyone can paste a marker into a comment (proposal IDs are hashes of public data), and adopting theirs would make every later edit fail with 403. So if a run stops after GitHub accepted a comment but before the gate saved its ID, the next run reuses that comment instead of posting a second. Before its first create, a run saves the proposals it is about to post, so even a run that crashed on its very first post leaves state behind; the next run then lists comments and adopts or outdates them, including when it finds no impact. State is saved again after all writes succeed. For an Actions result, the pre-create save keeps the awaited run, so if a write fails the retried `workflow_run` job still matches it and finishes the comments; the run is cleared only after the comments are written. Any other failed run is redone by the next push (see [Job queue](job-queue.md)).

## Known limits

- If a later push deletes the anchor line, GitHub shows its own "Outdated" badge on a still-open proposal; the gate's state is unaffected.
- The two GitHub comment APIs (review and issue) have separate ID spaces and edit endpoints, so the gate tracks which kind each comment is.
