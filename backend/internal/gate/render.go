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
	return proposalMarker(id) + "\n\n" + inertProse(p.Reason) + "\n\n" + fence + "suggestion\n" + content + fence + "\n"
}

// renderOutdated keeps the old comment body readable under an outdated notice,
// without its Apply box: ticking it would apply a proposal that no longer holds.
func renderOutdated(id, headSHA, old string) string {
	old = strings.TrimSpace(withoutCheckbox(strings.ReplaceAll(old, proposalMarker(id), ""), applyLabel))
	var b strings.Builder
	b.WriteString(proposalMarker(id))
	fmt.Fprintf(&b, "\n\n**Outdated: no longer needed as of %s**\n", shortSHA(headSHA))
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
	b.WriteString(inertProse(p.Reason))
	b.WriteString("\n\n")
	b.WriteString(proposalTarget(p))
	b.WriteString("\n\n")
	fence := fenceFor(diff.String())
	b.WriteString(fence + "diff\n" + diff.String() + fence + "\n")
	if p.IndexEntry != "" {
		fmt.Fprintf(&b, "\nIndex entry: %s\n", codeSpan(p.IndexEntry))
	}
	if fork {
		b.WriteString("\nApply is not available: this pull request comes from a fork the bot cannot push to.\n")
	} else {
		b.WriteString("\n" + checkbox(false, applyLabel) + "\n")
	}
	return b.String()
}

// checkbox is the markdown line for a box labelled label.
func checkbox(ticked bool, label string) string {
	if ticked {
		return "- [x] " + label
	}
	return "- [ ] " + label
}

// setCheckbox sets the box labelled label in body to ticked; ok is false when
// body has no such box in the other state.
func setCheckbox(body, label string, ticked bool) (string, bool) {
	from, to := checkbox(!ticked, label), checkbox(ticked, label)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == from {
			lines[i] = strings.Replace(l, from, to, 1)
			return strings.Join(lines, "\n"), true
		}
	}
	return body, false
}

// withoutCheckbox is body without the lines of the box labelled label, ticked or not.
func withoutCheckbox(body, label string) string {
	var kept []string
	for l := range strings.SplitSeq(body, "\n") {
		if t := strings.TrimSpace(l); t != checkbox(false, label) && t != checkbox(true, label) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func proposalTarget(p review.Proposal) string {
	if p.Section == "" {
		return "New doc: " + codeSpan(p.DocPath)
	}
	return codeSpan(p.DocPath) + ", section " + codeSpan(p.Section)
}

func writePrefixed(b *strings.Builder, prefix, text string) {
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
}

// fenceFor returns a backtick fence longer than any backtick run in body, so
// proposed content containing code fences cannot close the diff block early.
func fenceFor(body string) string {
	return strings.Repeat("`", max(3, longestBacktickRun(body)+1))
}

func longestBacktickRun(body string) int {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// codeSpan renders model-written identifier text as one inline code span: one
// line, delimited by a backtick run longer than any inside it, so nothing in it
// is interpreted as Markdown.
func codeSpan(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	delim := strings.Repeat("`", longestBacktickRun(text)+1)
	return delim + text + delim
}

// tableCodeSpan is codeSpan for a table cell, where GFM splits cells on `|`
// before it parses code spans.
func tableCodeSpan(text string) string {
	return strings.ReplaceAll(codeSpan(text), "|", `\|`)
}

// inertProse renders model-written prose as one line of Markdown that cannot
// mention, link or autolink, embed an image, open HTML, decode an entity or
// start a block. Balanced inline code spans stay as written; every other
// backtick is escaped.
func inertProse(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	var b strings.Builder
	for i := 0; i < len(text); {
		switch c := text[i]; c {
		case '`':
			n := backtickRunAt(text, i)
			if end := closingBacktickRun(text, i+n, n); end >= 0 {
				b.WriteString(text[i : end+n])
				i = end + n
			} else {
				b.WriteString(strings.Repeat("\\`", n))
				i += n
			}
			continue
		case '<':
			b.WriteString("&lt;")
		case '&':
			b.WriteString("&amp;")
		case '@':
			b.WriteString("@\u200b")
		case ':':
			if strings.HasPrefix(text[i:], "://") {
				b.WriteString(":\u200b")
			} else {
				b.WriteByte(c)
			}
		case 'w', 'W':
			if len(text) >= i+4 && strings.EqualFold(text[i:i+4], "www.") {
				b.WriteString(text[i:i+3] + "\u200b")
				i += 3
				continue
			}
			b.WriteByte(c)
		case '(':
			if i > 0 && text[i-1] == ']' {
				b.WriteString("\\(")
			} else {
				b.WriteByte(c)
			}
		case '\\', '[', ']':
			b.WriteString("\\" + string(c))
		default:
			b.WriteByte(c)
		}
		i++
	}
	return escapeBlockStart(b.String())
}

func backtickRunAt(text string, i int) int {
	n := 0
	for i+n < len(text) && text[i+n] == '`' {
		n++
	}
	return n
}

// closingBacktickRun returns the index of the first backtick run of exactly n
// at or after from, or -1.
func closingBacktickRun(text string, from, n int) int {
	for j := from; j < len(text); {
		if text[j] != '`' {
			j++
			continue
		}
		m := backtickRunAt(text, j)
		if m == n {
			return j
		}
		j += m
	}
	return -1
}

// escapeBlockStart keeps a line from opening as a heading, quote, list item,
// checkbox, rule or fence.
func escapeBlockStart(line string) string {
	if line == "" {
		return line
	}
	if strings.IndexByte("#>-+*=~", line[0]) >= 0 {
		return "\\" + line
	}
	d := 0
	for d < len(line) && line[d] >= '0' && line[d] <= '9' {
		d++
	}
	if d > 0 && d < len(line) && (line[d] == '.' || line[d] == ')') {
		return line[:d] + "\\" + line[d:]
	}
	return line
}

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
			section = tableCodeSpan(p.Section)
		}
		status := string(p.State)
		if p.State == ProposalApplied {
			status = fmt.Sprintf("applied (%s)", shortSHA(p.AppliedSHA))
		}
		link := "-"
		if p.CommentURL != "" {
			link = fmt.Sprintf("[view](%s)", p.CommentURL)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", tableCodeSpan(p.DocPath), section, link, status)
	}

	if len(state.Proposals) > 0 {
		b.WriteString("\n")
		switch {
		case state.Fork:
			b.WriteString("Apply all is not available: this pull request comes from a fork the bot cannot push to.\n")
		case applied > 0 && open == 0:
			b.WriteString("✅ All proposals applied.\n")
		default:
			b.WriteString(checkbox(false, applyAllLabel) + "\n")
		}
	}
	active := state.Skip
	if !skipActive(state) {
		active = nil
	}
	pending := state.PendingSkip
	b.WriteString(checkbox(scopeIs(SkipCommit, pending, active), skipCommitLabel) + "\n")
	b.WriteString(checkbox(scopeIs(SkipPR, pending, active), skipPRLabel) + "\n")
	if state.FailureCause != "" {
		b.WriteString(checkbox(false, rerunLabel) + "\n")
	}

	if active != nil {
		fmt.Fprintf(&b, "\nSkipped by @%s for this %s: %s\n", active.User, active.Scope.noun(), active.Reason)
	}
	if state.PendingSkip != nil {
		fmt.Fprintf(&b, "\nWaiting for @%s to reply with a reason.\n", state.PendingSkip.User)
	}
	fmt.Fprintf(&b, "\nCommands: `%[1]s %[2]s`, `%[1]s %[3]s <reason>`, `%[1]s %[4]s <reason>`.\n", commandPrefix, applyCommand, skipCommand, skipPRCommand)
	return b.String()
}

func scopeIs(scope SkipScope, pending *SkipAsk, active *Skip) bool {
	return pending != nil && pending.Scope == scope || active != nil && active.Scope == scope
}
