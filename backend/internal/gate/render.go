package gate

import (
	"fmt"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const summaryMarker = "<!-- pollux-agent:summary -->"

func proposalMarker(id string) string {
	return "<!-- pollux-agent:proposal:" + id + " -->"
}

// proposalComment is the review comment for p: a suggestion on the doc's own
// lines when they lie within one head-side hunk of the PR diff, else the
// checkbox variant on the anchor line.
func proposalComment(headSHA, id string, p review.Proposal, changed []review.ChangedFile, fork bool) ReviewComment {
	if suggestable(p, changed) {
		rc := ReviewComment{CommitSHA: headSHA, Path: p.DocPath, Line: p.Lines.End, Body: renderSuggestion(id, p)}
		if p.Lines.Start != p.Lines.End {
			rc.StartLine = p.Lines.Start
		}
		return rc
	}
	return ReviewComment{CommitSHA: headSHA, Path: p.Anchor.File, Line: p.Anchor.Line, Body: renderCheckbox(id, p, fork)}
}

func suggestable(p review.Proposal, changed []review.ChangedFile) bool {
	if p.Section == "" || p.Lines.Start < 1 || p.Lines.End < p.Lines.Start {
		return false
	}
	for _, f := range changed {
		if f.Path != p.DocPath {
			continue
		}
		for _, h := range f.Hunks {
			if p.Lines.Start >= h.Start && p.Lines.End <= h.End {
				return true
			}
		}
	}
	return false
}

func renderSuggestion(id string, p review.Proposal) string {
	// Lines covers the section's trailing blank lines; restate them so the
	// suggestion does not remove the gap before the next heading.
	original := strings.TrimRight(p.Original, "\n")
	blank := max(len(strings.TrimPrefix(p.Original, original))-1, 0)
	content := strings.TrimRight(p.Content, "\n") + "\n" + strings.Repeat("\n", blank)
	fence := fenceFor(content)
	return proposalMarker(id) + "\n\n" + p.Reason + "\n\n" + fence + "suggestion\n" + content + fence + "\n"
}

// renderOutdated keeps the old comment body readable under an outdated notice.
func renderOutdated(id, headSHA, old string) string {
	old = strings.TrimSpace(strings.ReplaceAll(old, proposalMarker(id), ""))
	short := headSHA[:min(7, len(headSHA))]
	var b strings.Builder
	b.WriteString(proposalMarker(id))
	fmt.Fprintf(&b, "\n\n**Outdated: no longer needed as of %s**\n", short)
	if old != "" {
		b.WriteString("\n<details>\n<summary>Original proposal</summary>\n\n" + old + "\n\n</details>\n")
	}
	return b.String()
}

// renderCheckbox is the checkbox-variant review comment body: the edit as a
// diff of the section's old lines against the proposed ones.
func renderCheckbox(id string, p review.Proposal, fork bool) string {
	var diff strings.Builder
	if p.Original != "" {
		writePrefixed(&diff, "-", p.Original)
	}
	writePrefixed(&diff, "+", p.Content)

	var b strings.Builder
	b.WriteString(proposalMarker(id))
	b.WriteString("\n\n")
	b.WriteString(p.Reason)
	b.WriteString("\n\n")
	b.WriteString(proposalTarget(p))
	b.WriteString("\n\n")
	fence := fenceFor(diff.String())
	b.WriteString(fence + "diff\n" + diff.String() + fence + "\n")
	if p.IndexEntry != "" {
		fmt.Fprintf(&b, "\nIndex entry: `%s`\n", p.IndexEntry)
	}
	if fork {
		b.WriteString("\nApply is not available: this pull request comes from a fork the bot cannot push to.\n")
	} else {
		b.WriteString("\n- [ ] " + applyLabel + "\n")
	}
	return b.String()
}

// tickApply flips the unticked Apply checkbox in body; ok is false when body
// has no such line.
func tickApply(body string) (string, bool) {
	const unticked, ticked = "- [ ] " + applyLabel, "- [x] " + applyLabel
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l == unticked {
			lines[i] = ticked
			return strings.Join(lines, "\n"), true
		}
	}
	return body, false
}

func proposalTarget(p review.Proposal) string {
	if p.Section == "" {
		return fmt.Sprintf("New doc: `%s`", p.DocPath)
	}
	return fmt.Sprintf("`%s`, section %q", p.DocPath, p.Section)
}

func writePrefixed(b *strings.Builder, prefix, text string) {
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
}

// fenceFor returns a backtick fence longer than any backtick run in body, so
// proposed content containing code fences cannot close the diff block early.
func fenceFor(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

const rerunLabel = "Re-run analysis"

// renderSummary is the summary comment body: a heading (the failure cause when
// the last analysis failed, else the open proposal count), one row per proposal
// in state, then the PR-wide checkboxes redrawn from state. Re-run is offered
// for a failure only; Apply all is never drawn ticked.
func renderSummary(state PRState) string {
	var b strings.Builder
	b.WriteString(summaryMarker)
	b.WriteString("\n\n")
	applied, open := 0, 0
	for _, p := range state.Proposals {
		switch p.State {
		case ProposalApplied:
			applied++
		case ProposalOpen:
			open++
		case ProposalOutdated:
		}
	}
	if state.FailureCause != "" {
		b.WriteString("**Analysis failed:** " + state.FailureCause + "\n\n")
	} else {
		noun := "updates"
		if open == 1 {
			noun = "update"
		}
		fmt.Fprintf(&b, "**pollux-agent** proposes %d doc %s.\n\n", open, noun)
	}
	if len(state.Proposals) > 0 {
		b.WriteString("| Doc | Section | Comment | State |\n| --- | --- | --- | --- |\n")
	}
	for _, p := range state.Proposals {
		section := "(new doc)"
		if p.Section != "" {
			section = strings.ReplaceAll(p.Section, "|", `\|`)
		}
		status := string(p.State)
		if p.State == ProposalApplied {
			status = fmt.Sprintf("applied (%s)", p.AppliedSHA[:min(7, len(p.AppliedSHA))])
		}
		link := "-"
		if p.CommentURL != "" {
			link = fmt.Sprintf("[view](%s)", p.CommentURL)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", strings.ReplaceAll(p.DocPath, "|", `\|`), section, link, status)
	}

	if len(state.Proposals) > 0 {
		b.WriteString("\n")
		switch {
		case state.Fork:
			b.WriteString("Apply all is not available: this pull request comes from a fork the bot cannot push to.\n")
		case applied > 0 && open == 0:
			b.WriteString("✅ All proposals applied.\n")
		default:
			b.WriteString(checkboxLine(false, applyAllLabel))
		}
	}
	active := state.Skip
	if active != nil && active.Scope == SkipCommit && active.HeadSHA != state.HeadSHA {
		active = nil
	}
	pending := state.PendingSkip
	b.WriteString(checkboxLine(scopeIs(SkipCommit, pending, active), skipCommitLabel))
	b.WriteString(checkboxLine(scopeIs(SkipPR, pending, active), skipPRLabel))
	if state.FailureCause != "" {
		b.WriteString(checkboxLine(false, rerunLabel))
	}

	if active != nil {
		scope := "this PR"
		if active.Scope == SkipCommit {
			scope = "this commit"
		}
		fmt.Fprintf(&b, "\nSkipped by @%s for %s: %s\n", active.User, scope, active.Reason)
	}
	if state.PendingSkip != nil {
		fmt.Fprintf(&b, "\nWaiting for @%s to reply with a reason.\n", state.PendingSkip.User)
	}
	fmt.Fprintf(&b, "\nCommands: `%[1]s %[2]s`, `%[1]s %[3]s <reason>`, `%[1]s %[4]s <reason>`.\n", commandPrefix, applyCommand, skipCommand, skipPRCommand)
	return b.String()
}

func checkboxLine(ticked bool, label string) string {
	if ticked {
		return "- [x] " + label + "\n"
	}
	return "- [ ] " + label + "\n"
}

func scopeIs(scope SkipScope, pending *SkipAsk, active *Skip) bool {
	return pending != nil && pending.Scope == scope || active != nil && active.Scope == scope
}
