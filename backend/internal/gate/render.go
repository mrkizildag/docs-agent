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
func proposalComment(headSHA, id string, p review.Proposal, changed []review.ChangedFile) ReviewComment {
	if suggestable(p, changed) {
		rc := ReviewComment{CommitSHA: headSHA, Path: p.DocPath, Line: p.Lines.End, Body: renderSuggestion(id, p)}
		if p.Lines.Start != p.Lines.End {
			rc.StartLine = p.Lines.Start
		}
		return rc
	}
	return ReviewComment{CommitSHA: headSHA, Path: p.Anchor.File, Line: p.Anchor.Line, Body: renderCheckbox(id, p)}
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
func renderCheckbox(id string, p review.Proposal) string {
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
	b.WriteString("\n- [ ] Apply this change\n")
	return b.String()
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

const (
	rerunUnticked = "- [ ] Re-run analysis"
	rerunTicked   = "- [x] Re-run analysis"
)

// renderSummary is the summary comment body: the failure cause when the last
// analysis failed, one row per proposal in state, and the Re-run checkbox.
func renderSummary(state PRState, cause string) string {
	var b strings.Builder
	b.WriteString(summaryMarker)
	b.WriteString("\n\n")
	if cause != "" {
		b.WriteString("**Analysis failed:** " + cause + "\n\n")
	}
	if len(state.Proposals) > 0 {
		b.WriteString("| Doc | Section | Comment | State |\n| --- | --- | --- | --- |\n")
	}
	for _, p := range state.Proposals {
		section := "(new doc)"
		if p.Section != "" {
			section = strings.ReplaceAll(p.Section, "|", `\|`)
		}
		fmt.Fprintf(&b, "| `%s` | %s | [view](%s) | %s |\n", strings.ReplaceAll(p.DocPath, "|", `\|`), section, p.CommentURL, p.State)
	}
	if len(state.Proposals) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(rerunUnticked + "\n")
	return b.String()
}

// RerunTicked reports whether an edit of the summary comment from before to
// after ticked its Re-run analysis box.
func RerunTicked(before, after string) bool {
	return isSummary(before) && isSummary(after) && hasLine(before, rerunUnticked) && hasLine(after, rerunTicked)
}

func isSummary(body string) bool {
	first, _, _ := strings.Cut(body, "\n")
	return strings.TrimRight(first, "\r") == summaryMarker
}

func hasLine(body, line string) bool {
	for l := range strings.SplitSeq(body, "\n") {
		if strings.TrimRight(l, "\r") == line {
			return true
		}
	}
	return false
}
