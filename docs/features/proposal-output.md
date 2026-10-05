---
title: Proposal output
summary: What a PR gets when analysis proposes doc edits (check run, one review comment per proposal, a summary comment) and how re-runs keep those comments stable.
covers:
  - backend/internal/gate/**
  - backend/internal/review/review.go
  - backend/internal/github/comments.go
---

# Proposal output

When a runner returns proposals, the gate reports them three ways on the PR: the `pollux-agent` check run (`action_required`, listing doc paths and reasons), one review comment per proposal on the head commit, and one summary comment on the PR. A "no impact" result sets the check to `success` with the reason and posts no comments. The checkboxes act on the proposals; see [Apply and Skip](apply-skip.md). The summary also carries the PR-wide "Apply all", "Skip this commit", and "Skip this PR" checkboxes, drawn from stored state on every redraw (the bot never draws "Apply all" ticked: once a proposal is applied and none is open, the line becomes "✅ All proposals applied."), each proposal's applied state, and, on a fork PR, a notice that Apply is unavailable (proposal comments then have no Apply checkbox).

## Failures and Re-run

Every analysis first creates an in-progress check and saves an armed run (nonce, 10-minute deadline), so a check is never missing or left pending: the deadline sweep closes any armed run neutral (see [Job queue](job-queue.md)). Any failure ends the check neutral, titled "Analysis failed" (or "PR too large to analyze"), with a one-line fixed cause that never quotes model output, provider responses, or artifact text; the detail stays in the server log. A superseded or interrupted analysis is not a failure: the next analysis closes its check as superseded.

A successful summary opens with "**pollux-agent** proposes N doc update(s)." (open proposals only) and has no Re-run box. After a failure the summary instead opens with the cause and has an unticked "- [ ] Re-run analysis" among its checkboxes; the cause is kept in saved state, so a later redraw (a Skip, a refused tick) keeps both. Proposals from an earlier successful run stay listed and are not marked outdated, because a failure says nothing about them. Ticking the box is a summary tick handled like the other summary checkboxes (see [Apply and Skip](apply-skip.md)): an edited `issue_comment` on the bot's summary, identified by its saved comment ID, becomes a `pull_request`-kind job that carries the tick (it does not supersede, and a push cancels it); a tick elsewhere, an edit by a bot, or any other edit does nothing. The sender needs write access (otherwise the bot replies and unticks the box); the comment gets 👀, then 🚀 once the re-run started, failed in a reported way, or had nothing to do. Any edit of an issue comment that ticks the Re-run box is queued; the check that it is the summary comment (by saved ID) happens when the job runs. Clicking GitHub's Re-run on the check (`check_run` `rerequested`) enqueues a `pull_request`-kind job directly. Either way the gate fetches the PR's current head and starts a fresh analysis with a new check run, and the box is unticked when that analysis next writes the summary (on its result or its failure; an Actions re-run leaves it ticked until the workflow finishes). The check's Re-run button needs no permission check of its own: GitHub only lets people with write access re-run a check. A re-run of a closed PR, or a second one while an analysis of the same head is still running, does nothing, so repeated ticks cannot stack model runs.

A re-run waits for an analysis already running on the PR instead of cancelling it, and a push supersedes a re-run like any analysis, so only the newest head produces the final check; a same-head check it replaces reads "Superseded by a re-run". For forks, `check_run` may carry no pull request; the handler then finds the PR by the check's head SHA in stored state. Posting comments after a result is retried a few times with backoff, and a retry adopts comments already created by their markers, so nothing is duplicated.

## Where the section text comes from

Each proposal carries the current text of the section it replaces and that text's head-side line range in the doc. The server runner fills both from its own checkout of the head commit (see [Server runner](server-runner.md)); the Actions runner fills them from the doc fetched at the head SHA through the contents API (see [Actions runner](actions-runner.md)); they are never model output and are kept out of the generated JSON schema. Both are empty for a new doc. The range runs from the heading through the section's last line, including trailing blank lines. The gate may import only `internal/review`, so it cannot parse docs itself; it trusts these fields. A proposal whose doc or section is missing at the head, or whose heading matches more than one section, leaves them empty, and its comment degrades to the checkbox variant; guessing the section would let a suggestion overwrite the wrong one.

A proposal's content replaces the whole section, heading line included, because a suggestion replaces the heading line too. Both runner prompts and the generated schema say so. If the content still starts without a heading of the section's level, the gate puts back the original heading and the blank lines after it before rendering; otherwise applying the edit would delete the heading from the doc.

## Two comment variants

- **Suggestion.** Used only when the whole section, trailing blank lines included, lies inside one head-side hunk of that doc in the PR diff. GitHub only accepts review comments on diff lines, so this is the one case where the comment can sit on the doc's own lines. It is a GitHub `suggestion` block replacing exactly those lines, with the reason and no checkbox. The suggestion restates the section's trailing blank lines, otherwise applying it would eat the gap before the next heading. The code fence is made longer than any backtick run in the content so proposed code fences cannot close it early.
- **Checkbox.** Used for everything else: the doc is not in the diff, the section only partly overlaps a hunk or spans two, or the doc is new. The comment sits on the proposal's anchor line (a changed code line) and holds the reason, the doc path and section, the edit as a `diff` block (old section lines `-`, new lines `+`; a new doc shows only added lines plus its index entry), and an unticked "Apply this change" task-list item.

## Identity and re-runs

A proposal's identity is its doc path plus its normalized section heading (leading `#` and whitespace stripped; path alone for a new doc), hashed to a short ID. The gate remembers each proposal's ID, comment ID and URL, and state (`open`, `outdated`, or `applied`), plus the summary comment ID, per PR.

On each run, after the check run is created, the gate reconciles the new proposals against what it remembers:

- **Same proposal again:** its comment is edited in place. The PR's comment count does not change across pushes.
- **New proposal:** a new review comment.
- **Proposal gone:** its comment and summary row are marked `outdated`; the old body stays readable under an "Outdated" notice. The gate never deletes a comment: that would erase the thread's replies and history, and a proposal that returns would have to start over. A proposal that returns after being outdated reopens its old comment.
- **Applied proposal:** stays applied while its content is unchanged. If a re-run returns it with different content, it reopens as an open proposal, because the doc no longer matches what was applied. A re-run that no longer returns it leaves it applied.
- **No impact:** every earlier proposal that is not applied is marked outdated and the check is `success`; applied ones stay applied.
- **Outdated bodies** carry no live "Apply this change" checkbox, so an outdated proposal cannot be applied by ticking; a tick on one is refused with a reply.
- **Summary:** one issue comment listing every proposal with doc path, section, a link to its comment, and state. It is edited in place, and only created when there is something to list or one already exists. On the first run it is created before the proposal comments, so it sits above them in the timeline (GitHub orders comments by creation time and cannot move them), then edited once to add their links.

A comment keeps its location and variant across edits. GitHub cannot move a review comment, and replacing it would churn the thread. A suggestion block on the wrong lines would overwrite them when applied, so on edit the suggestion body is used only if the existing comment already sits on that doc at the same lines; otherwise the body is the checkbox variant. If a user deletes a proposal's comment, an open proposal gets a fresh comment, and an outdated one is skipped rather than resurrected.

## Markers and crash recovery

Every proposal comment starts with a hidden `<!-- pollux-agent:proposal:ID -->` and the summary with `<!-- pollux-agent:summary -->`. The gate lists the PR's comments before writing and, when its stored state lacks a comment ID, adopts the comment carrying the marker. It adopts or edits only comments written by the App's own bot user, with the marker as the body's first line: anyone can paste a marker into a comment (proposal IDs are hashes of public data), and adopting theirs would make every later edit fail with 403. So if a run stops after GitHub accepted a comment but before the gate saved its ID, the next run reuses that comment instead of posting a second. Before its first create, a run saves the proposals it is about to post, so even a run that crashed on its very first post leaves state behind; the next run then lists comments and adopts or outdates them, including when it finds no impact. State is saved again after all writes succeed. The concluded state (check done, run cleared) is saved before comments are posted, so a comment failure cannot leave an armed run for the deadline sweep to overwrite a success with neutral. Any other failed run is redone by the next push or re-run (see [Job queue](job-queue.md)).

## Known limits

- If a later push deletes the anchor line, GitHub shows its own "Outdated" badge on a still-open proposal; the gate's state is unaffected.
- The two GitHub comment APIs (review and issue) have separate ID spaces and edit endpoints, so the gate tracks which kind each comment is.
