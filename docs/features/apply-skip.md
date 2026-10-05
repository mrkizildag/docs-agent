---
title: Apply and Skip
summary: How a developer applies proposals (one, all, or by command) or waives the gate (Skip), who may, what is refused, and why a repeated delivery commits at most once.
covers:
  - backend/internal/gate/apply.go
  - backend/internal/gate/skip.go
  - backend/internal/httpapi/**
  - backend/internal/github/commit.go
---

# Apply and Skip

The check stays `action_required` until every proposal is applied or the gate is skipped. Both are driven by PR comments, so they reach the backend as `comment` jobs (see [Job queue](job-queue.md)). The comments they act on are described in [Proposal output](proposal-output.md).

## Triggers

- **Apply one:** ticking "Apply this change" on a proposal's review comment.
- **Apply all:** ticking "Apply all" in the summary, or commenting `/pollux-agent apply`.
- **Skip this commit / Skip this PR:** ticking the matching box in the summary, or commenting `/pollux-agent skip <reason>` or `/pollux-agent skip-pr <reason>`.

A tick is recognized only as a single `- [ ]` to `- [x]` flip in the edit's previous body (`changes.body.from`). Unticking, editing text, or ticking several boxes at once changes nothing, so the bot redrawing its own comments cannot trigger anything. Events whose sender is a bot are dropped for the same reason. Every comment from a human on a PR is enqueued, because it may be the reason a pending skip is waiting for.

## Feedback

A queued action can wait behind a running analysis, so the bot shows it was seen. When it picks up a tick or command, it reacts 👀 on that comment: the bot's own comment for a tick, the user's comment for a command or skip reason. The 👀 becomes 🚀 when the action completes and 😕 when it is refused, with a reply saying why. GitHub has no check-mark reaction, so the "Applied" reply starts with ✅ instead. If the job fails, the 👀 stays and a redelivery retries. Comments that ask for nothing get no reaction.

## Permissions

The sender of the event must have write access, checked through the collaborator-permission endpoint. The sender is the person who ticked or commented, never the comment's author: the proposal comments are authored by the bot, so author checks would let anyone with read access trigger a commit. A user without write access changes nothing and gets a reply saying why.

## Apply

Apply commits to the PR branch; the bot does not open a separate docs PR (phase 2).

- The edit comes from what the gate stored when it made the proposal (new content, the exact original section text, and the index entry for a new doc), not from a fresh analysis, which would be slow and nondeterministic. At apply time the original section is located in the doc at the branch head and must match exactly once; the section is replaced, or for a new doc the file and its `docs/README.md` index entry are added. A section that no longer matches is not guessed at.
- One commit covers everything applied by one action (one proposal, or all open ones including suggestion variants), made through the Git Data API (blobs, tree, commit, then a non-force ref update). Apply all also ticks every proposal comment's box and marks each proposal applied.
- Each applied proposal gets one reply in its own review thread naming the commit. The reply's ID is stored per proposal, so a redelivery or re-run never posts a second one.
- **Stale proposals.** Proposals remember the head they were made against. If the branch has since moved, Apply commits nothing and replies that the new push is being re-analyzed. The non-force ref update is the backstop: if the branch moves between the check and the update, GitHub rejects it and the gate reports the same thing.
- **Forks.** The bot cannot push to a fork, so every cross-repo PR is treated as one. Proposals render without Apply checkboxes, the summary says why, and `/pollux-agent apply` gets a reply saying why. Skip still works.
- **After the commit.** The bot's commit is a push, so it triggers exactly one re-analysis. A re-run that returns an applied proposal with the same content leaves it applied and does not post it again. The check counts only proposals that are not applied yet, so a re-run that returns nothing but applied proposals passes.
- **Trade-off.** The developer's local branch does not have the bot's commit, so they must pull before pushing again.

## Skip

Skip has two scopes: this commit (the check passes for the current head; the next push is analyzed again) and this PR (the check passes for every later push and no analysis runs).

Ticking a Skip box does not skip yet. The bot asks that user for a reason, and their next comment on the PR becomes the reason; only then is the skip recorded. The command form with a reason takes effect at once, and without a reason gets the same ask. The check and the summary show who skipped, the scope, and the reason. A skip passes the check for the head it was made on; a commit skip is cleared by the next push. If a push arrives while a commit skip is still waiting for its reason, the ask is cancelled and the bot says so in a comment, so the skip never carries over to code the user has not seen.

Skip needs the same write access as Apply and is allowed on forks. Nothing undoes a skip or an apply.

## Idempotency

Jobs run at least once, and GitHub redelivers, so a tick can arrive twice. The non-force ref update is the guard: the first delivery moves the branch from the old head, the second finds the old head stale and cannot commit again, so at most one commit results.

The gap is a crash after GitHub accepted the commit but before the gate saved it. The redelivery then sees the branch moved. If the live head is a bot commit whose parent is the head the proposals were made on, the gate adopts that commit as the applied one and finishes the state, thread replies, and summary instead of refusing or committing again.

## Summary redraws

Every summary redraw renders the Apply all and Skip checkboxes from stored state, plus the applied state of each proposal and the fork notice. Apply all is ticked once every proposal is applied. A skip box is ticked while its skip is pending or active; a commit skip counts only for the head it was made at. A re-run therefore never drops them or leaves a stale tick. When a tick is refused, the bot puts the box back so it can be ticked again.
