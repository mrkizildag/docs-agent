---
title: Apply and Skip
summary: How a developer applies proposals (one, all, or by command) or waives the gate (Skip), who may, what is refused, and why a repeated delivery commits at most once.
covers:
  - backend/internal/gate/apply.go
  - backend/internal/gate/comment.go
  - backend/internal/gate/render.go
  - backend/internal/gate/skip.go
  - backend/internal/gate/transition.go
  - backend/internal/gate/analyze.go
  - backend/internal/gate/reconcile.go
  - backend/internal/gate/state.go
  - backend/internal/review/*.go
  - backend/internal/httpapi/**
  - backend/internal/github/commit.go
  - backend/internal/github/reactions.go
  - backend/internal/github/comments.go
---

# Apply and Skip

The check stays `action_required` until every proposal is applied or the gate is skipped. Both are driven by PR comments, so they reach the backend as `comment` jobs, except the Re-run tick (see [Job queue](job-queue.md)). The comments they act on are described in [Proposal output](proposal-output.md).

## Triggers

- **Apply one:** ticking "Apply this change" on a proposal's review comment.
- **Apply all:** ticking "Apply all" in the summary, or commenting `/pollux-agent apply`.
- **Skip this commit / Skip this PR:** ticking the matching box in the summary, or commenting `/pollux-agent skip <reason>` or `/pollux-agent skip-pr <reason>`.
- **Re-run analysis:** ticking "Re-run analysis" in a failure summary (see [Proposal output](proposal-output.md)). It is handled like the other summary ticks (the sender needs write access, the comment gets 👀 and then 🚀 once the re-run started, failed in a reported way, or had nothing to do), but it runs as a `comment_rerun` job in the `pull_request` supersede group (the check's Re-run button is the `rerun` kind in the same group), so a push cancels it instead of letting it analyze an old head. An infrastructure error leaves the 👀.

A tick is recognized only as a single `- [ ]` to `- [x]` flip in the edit's previous body (`changes.body.from`). Unticking, editing text, or ticking several boxes at once changes nothing, so the bot redrawing its own comments cannot trigger anything. Events whose sender is a bot are dropped first; that is the guard against the bot reacting to its own redraws, and the single-flip rule additionally filters human edits that are not a tick. Only newly created conversation comments are enqueued (not edits or deletions), because one may be the reason a pending skip is waiting for. A command is recognized only as the first line of a comment, and a skip reason must be a conversation comment, not a review comment.

## Feedback

A queued action can wait behind a running analysis, so the bot shows it was seen. When it picks up a tick or command, it reacts 👀 on that comment: the bot's own comment for a tick, the user's comment for a command or skip reason. The 👀 becomes 🚀 when the action completes and 😕 when it is refused, with a reply saying why. A tick on an outdated proposal is refused the same way. A PR the gate has never analyzed gets the same refusal for every action (Apply, Skip, Re-run), with a reply to push or reopen it. GitHub has no check-mark reaction, so the "Applied" reply starts with ✅ instead. If the job fails, the 👀 stays and a redelivery retries. A Re-run that a push cancels removes its 👀; the push's own analysis takes over. Comments that ask for nothing get no reaction.

## Permissions

The sender of the event must have write access, checked through the collaborator-permission endpoint. The sender is the person who ticked or commented, never the comment's author: the proposal comments are authored by the bot, so author checks would let anyone with read access trigger a commit. The permission is checked before anything else, so a user without write access gets no 👀: they get one reply saying why and a 😕, and nothing changes. The PR author is a writer like anyone else and may skip their own PR; the gate does not separate author from reviewer.

## Apply

Apply commits to the PR branch; the bot does not open a separate docs PR (phase 2).

- The edit comes from what the gate stored when it made the proposal (new content, the exact original section text, and the index entry for a new doc), not from a fresh analysis, which would be slow and nondeterministic. At apply time the original section is located in the doc at the branch head and must match exactly once; the section is replaced, or for a new doc the file and its `docs/README.md` index entry are added (the entry is skipped only if the `## Index` section already has that line). A section that no longer matches is not guessed at.
- One commit covers everything applied by one action (one proposal, or all open ones including suggestion variants), made through the Git Data API (blobs, tree, commit, then a non-force ref update). Apply all also ticks every proposal comment's box and marks each proposal applied.
- Each applied proposal gets one reply in its own review thread naming the commit. The reply's ID is stored per proposal, so a redelivery or re-run never posts a second one.
- **Validation and state.** Before committing, the stored proposals are validated again (doc path must be a `.md` or `.mdx` file with no control characters or backticks, section and index entry single-line); an invalid one refuses the whole action and commits nothing. A doc, `docs/README.md`, or a new-doc path held by a file too large to read (over 1 MiB at head) is refused with "<path> is too large to edit; nothing was committed." and the box is unticked; it is never read as missing. Apply on a closed PR is refused; the live PR is fetched at apply time. Edits to existing files keep the file's mode, looked up per path rather than by reading the whole repo tree, so Apply works in very large repos; a symlink or submodule at a proposal's path is refused. All section edits are located against the head content first, and new docs' index entries are added last, so one edit cannot shift another's target.
- **Stale proposals.** Proposals remember the head they were made against. If the live head has moved, Apply is refused: nothing is committed and the gate replies. The wording depends on state: while a newer push is being analyzed it says so; after a failed analysis it offers Re-run; under an active PR skip it says the PR is skipped, so its proposals are not refreshed; otherwise it asks for a push or to wait. The non-force ref update is the backstop: if the branch moves between the check and the update, GitHub rejects it and the gate reports the same thing. When GitHub rejects the ref update, the adapter re-reads the branch tip; a rejection for another reason (a protected branch) or a symlink/submodule at the path is refused with a reply ("GitHub rejected the commit: <reason>; nothing was committed.", with the reason GitHub gave), 😕, and nothing committed.
- **Forks.** The bot cannot push to a fork, so every cross-repo PR is treated as one, including one whose head repo was deleted. Proposals render without Apply checkboxes, the summary says why, and `/pollux-agent apply` gets a reply saying why. Skip still works.
- **After the commit.** The bot's commit is a push, so it triggers exactly one re-analysis. A re-run that returns an applied proposal with the same content leaves it applied and does not post it again. The check counts only proposals that are not applied yet, so a re-run that returns nothing but applied proposals passes.
- **Trade-off.** The developer's local branch does not have the bot's commit, so they must pull before pushing again.

## Skip

Skip has two scopes: this commit (the check passes for the current head; the next push is analyzed again) and this PR (the check passes for every later push and no analysis runs).

Ticking a Skip box does not skip yet. The bot asks that user for a reason, and their next comment on the PR becomes the reason; only then is the skip recorded. The command form with a reason takes effect at once, and without a reason gets the same ask. The check and the summary show who skipped, the scope, and the reason. A skip passes the check for the head it was made on; a commit skip is cleared by the next push. If a push to a new head arrives while either kind of skip is still waiting for its reason, the ask is cancelled and the bot says so in a comment, asking the user to tick again, so a skip never carries over to code the user has not seen. A redelivery of the same head keeps the ask, and the note is posted once the new head is saved, even if that push's analysis fails. A Re-run on a new head does the same.

The reason is for people: it is collapsed to one line, capped at 500 characters, with every `@` and `- [` broken by a zero-width space so it cannot ping anyone or pose as a checkbox. The skip is saved before the summary is redrawn, so a failed redraw cannot leave the check green with no record; a redelivery redraws it. Skip needs the same write access as Apply and is allowed on forks. Nothing undoes a skip or an apply.

## Idempotency

Jobs run at least once, and GitHub redelivers, so a tick can arrive twice. The non-force ref update is the guard: the first delivery moves the branch from the old head, the second finds the old head stale and cannot commit again, so at most one commit results.

Order matters because each step can crash. The gate saves what it intends before the side effect and what happened after it, so a retry can tell which half ran. Before committing, it saves a pending apply: the proposal IDs, the commit message, and the parent head. After the commit it marks the proposals applied and clears the pending apply. Each thread reply carries a hidden `<!-- pollux-agent:applied:ID:SHA -->` marker naming the proposal and the commit, and before posting the gate adopts an existing bot reply with that marker, so a crash between reply and save does not post it twice. The SHA is part of the marker so a proposal that is reopened and applied again gets a new "✅ Applied in <new sha>" reply instead of adopting the old one. A redelivery that finds nothing left to do still redraws the summary and ticks the applied boxes, since the first delivery may have died there. "✅ Nothing left to apply." is said only when nothing is applied at all; otherwise the redelivery adds no text and reacts 🚀.

The gap is a crash after GitHub accepted the commit but before the gate saved it. The bot's own commit is a push, and its job could run before any redelivery and mark the just-applied proposals outdated. So whichever runs first, the push job or the retried Apply, checks the pending apply, and so does a Re-run on that head. The pushed commit (fetched by SHA, not the branch tip, which the user may already have moved past) is adopted as the applied one only when it was authored by the bot, its message equals the pending message, and its only parent is the pending parent. The retried Apply does this before any stale check. The gate then marks those proposals applied, with that commit's SHA, before reconciling, and finishes the state, thread replies, and summary instead of refusing or committing again.

## Summary redraws

Every summary redraw renders the Apply all and Skip checkboxes from stored state, plus the applied state of each proposal and the fork notice. Apply all is never drawn ticked: once every proposal is applied the line reads "✅ All proposals applied.". A failure summary also redraws its cause and Re-run box. A skip box is ticked while its skip is pending or active; a commit skip counts only for the head it was made at. A re-run therefore never drops them or leaves a stale tick. When a tick is refused, the bot puts the box back so it can be ticked again.
