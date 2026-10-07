package gate_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestProposalCommentBodies(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{changed: []review.ChangedFile{
		{Path: "docs/s.md", Hunks: []review.LineRange{{Start: 1, End: 10}}, Patch: "@@ -1 +1,10 @@"},
		{Path: "a.go", Hunks: []review.LineRange{{Start: 1, End: 10}}},
		{Path: "b.go", Hunks: []review.LineRange{{Start: 1, End: 10}}},
	}}
	checkbox := review.Proposal{
		DocPath: "docs/a.md", Section: "Usage", Reason: "flag renamed", Anchor: review.Anchor{File: "a.go", Line: 4},
		Original: "## Usage\nold\n", Lines: review.LineRange{Start: 3, End: 4}, Content: "## Usage\nnew\n",
	}
	newDoc := review.Proposal{
		DocPath: "docs/b.md", Reason: "new feature", Anchor: review.Anchor{File: "b.go", Line: 9},
		Content: "# B\nbody\n", IndexEntry: "- [B](b.md)",
	}
	suggest := review.Proposal{
		DocPath: "docs/s.md", Section: "Run", Reason: "port changed", Anchor: review.Anchor{File: "c.go", Line: 2},
		Original: "## Run\nport 1\n", Lines: review.LineRange{Start: 5, End: 6}, Content: "## Run\nport 2\n",
	}
	proposalService(t, gh, &fakeStore{}, review.Proposals{checkbox, newDoc, suggest})

	if len(gh.reviewComments) != 3 || gh.createIssue != 1 {
		t.Fatalf("review comments = %d, summaries = %d, want 3 and 1", len(gh.reviewComments), gh.createIssue)
	}

	cb := gh.reviewComments[0]
	if cb.Path != "a.go" || cb.Line != 4 || cb.CommitSHA != "abc123" {
		t.Errorf("checkbox comment anchored at %s:%d on %s, want a.go:4 on abc123", cb.Path, cb.Line, cb.CommitSHA)
	}
	for _, want := range []string{"flag renamed", "`docs/a.md`", "section `Usage`", "```diff\n-## Usage\n-old\n+## Usage\n+new\n```", "- [ ] Apply this change"} {
		if !strings.Contains(cb.Body, want) {
			t.Errorf("checkbox body missing %q:\n%s", want, cb.Body)
		}
	}

	nd := gh.reviewComments[1]
	if nd.Path != "b.go" || nd.Line != 9 {
		t.Errorf("new doc comment anchored at %s:%d, want b.go:9", nd.Path, nd.Line)
	}
	for _, want := range []string{"new feature", "`docs/b.md`", "```diff\n+# B\n+body\n```", "- [B](b.md)", "- [ ] Apply this change"} {
		if !strings.Contains(nd.Body, want) {
			t.Errorf("new doc body missing %q:\n%s", want, nd.Body)
		}
	}
	if strings.Contains(nd.Body, "```diff\n-") {
		t.Errorf("new doc body has removed lines:\n%s", nd.Body)
	}

	sg := gh.reviewComments[2]
	if sg.Path != "docs/s.md" || sg.StartLine != 5 || sg.Line != 6 {
		t.Errorf("suggestion anchored at %s:%d-%d, want docs/s.md:5-6", sg.Path, sg.StartLine, sg.Line)
	}
	if !strings.Contains(sg.Body, "port changed") || !strings.Contains(sg.Body, "```suggestion\n## Run\nport 2\n```") || strings.Contains(sg.Body, "[ ]") {
		t.Errorf("suggestion body = %q, want reason, suggestion block, no checkbox", sg.Body)
	}

	summary := gh.comments[0].Body
	for i, want := range []string{"| `docs/a.md` | `Usage` |", "| `docs/b.md` | (new doc) |", "| `docs/s.md` | `Run` |"} {
		row := want + " [view](" + gh.comments[i+1].URL + ") | open |"
		if !strings.Contains(summary, row) {
			t.Errorf("summary missing row %q:\n%s", row, summary)
		}
	}
}

func TestProposalCommentOffersApplyOnlyOffAFork(t *testing.T) {
	t.Parallel()

	p := review.Proposal{DocPath: "docs/a.md", Section: "Usage", Reason: "r", Original: "## Usage\nold\n", Content: "## Usage\nnew\n"}
	tests := []struct {
		name         string
		fork         bool
		wantCheckbox bool
	}{
		{name: "same repo offers apply", wantCheckbox: true},
		{name: "fork explains why not", fork: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pr := testPR()
			pr.Fork = tc.fork
			_, writes, _ := gate.Reconcile(gate.PRState{}, pr, review.Proposals{p}, nil, nil)

			body := writes[0].Review.Body
			if got := strings.Contains(body, "- [ ] Apply this change"); got != tc.wantCheckbox {
				t.Errorf("body has apply checkbox = %v, want %v:\n%s", got, tc.wantCheckbox, body)
			}
			if got := strings.Contains(body, "fork"); got == tc.wantCheckbox {
				t.Errorf("body mentions fork = %v, want %v", got, !tc.wantCheckbox)
			}
		})
	}
}

func TestSuggestionCommentNotesForkOnly(t *testing.T) {
	t.Parallel()

	p := review.Proposal{DocPath: "docs/a.md", Section: "Usage", Reason: "r", Lines: review.LineRange{Start: 3, End: 4}, Original: "## Usage\nold\n", Content: "## Usage\nnew\n"}
	changed := []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 10}}}}
	for _, fork := range []bool{false, true} {
		pr := testPR()
		pr.Fork = fork
		_, writes, _ := gate.Reconcile(gate.PRState{}, pr, review.Proposals{p}, changed, nil)

		body := writes[0].Review.Body
		if !strings.Contains(body, "```suggestion") {
			t.Fatalf("fork = %v: body is not a suggestion:\n%s", fork, body)
		}
		if got := strings.Contains(body, "Apply is not available: this pull request comes from a fork"); got != fork {
			t.Errorf("fork = %v: body has fork note = %v:\n%s", fork, got, body)
		}
	}
}

func TestOutdatedProposalCommentOnlySwapsItsMarkerAndResolves(t *testing.T) {
	t.Parallel()

	id := gate.ProposalID("docs/a.md", "A")
	old := "<!-- pollux-agent:proposal:" + id + " -->\n\nflag renamed\n\n- [ ] Apply this change\n"
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen}}}
	existing := []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: old}}

	state, writes, _ := gate.Reconcile(prev, testPR(), review.NoImpact{Reason: "x"}, nil, existing)

	want := "<!-- pollux-agent:superseded:" + id + " -->\n\nflag renamed\n\n- [ ] Apply this change\n"
	if w := writes[0]; w.ID != 1 || !w.Resolve || w.Body != want {
		t.Errorf("write = %+v, want comment 1 with only its marker swapped, thread resolved", w)
	}
	if got := state.Proposals[0]; got.State != gate.ProposalOutdated || got.CommentID != 1 {
		t.Errorf("proposal = %+v, want outdated and still pointing at comment 1 so a tick is refused", got)
	}
}

func TestSupersededProposalCommentOnlySwapsItsMarker(t *testing.T) {
	t.Parallel()

	id := gate.ProposalID("docs/a.md", "A")
	old := "<!-- pollux-agent:proposal:" + id + " -->\n\nflag renamed\n\n- [x] Apply this change\n"
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalApplied, Content: "## A\nold\n"}}}
	p := proposal("docs/a.md", "A")
	p.Content = "## A\nnewer\n"

	_, writes, _ := gate.Reconcile(prev, testPR(), review.Proposals{p}, nil, []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: old}})

	want := "<!-- pollux-agent:superseded:" + id + " -->\n\nflag renamed\n\n- [x] Apply this change\n"
	if got := writes[0].Body; got != want {
		t.Errorf("superseded body = %q, want %q", got, want)
	}
}

func TestSupersededSuggestionCommentLosesItsSuggestionFence(t *testing.T) {
	t.Parallel()

	p := review.Proposal{DocPath: "docs/a.md", Section: "Usage", Reason: "r", Lines: review.LineRange{Start: 3, End: 4}, Original: "## Usage\nold\n", Content: "## Usage\nnew\n"}
	changed := []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 10}}}}
	_, writes, _ := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, changed, nil)
	live := writes[0].Review.Body
	id := gate.ProposalID("docs/a.md", "Usage")
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "Usage", CommentID: 1, State: gate.ProposalOpen, Content: "## Usage\nold\n"}}}

	_, retired, _ := gate.Reconcile(prev, testPR(), review.NoImpact{Reason: "x"}, nil, []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: live}})

	want := strings.Replace(strings.Replace(live, "proposal", "superseded", 1), "```suggestion\n", "```\n", 1)
	if got := retired[0].Body; got != want || strings.Contains(got, "suggestion\n") {
		t.Errorf("retired body = %q, want %q", got, want)
	}
}

// renderedSummary returns the summary comment for state, as a refused tick of
// its Apply all box redraws it.
func renderedSummary(t *testing.T, state gate.PRState) string {
	t.Helper()

	state.InstallationID, state.Owner, state.Repo, state.Number, state.SummaryCommentID = 1, "acme", "widgets", 3, summaryID
	gh := &fakeGitHub{}
	for range summaryID {
		gh.addComment(gate.CommentKindIssue, "stale")
	}
	svc := gate.NewService(gh, &fakeCommentGitHub{}, &fakeStore{stored: state}, gate.Runners{}, nil, nil)
	if err := svc.HandleComment(t.Context(), summaryTick("Apply all")); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	return gh.comments[summaryID-1].Body
}

func TestSummaryRendering(t *testing.T) {
	t.Parallel()

	prop := func(status gate.ProposalStatus) gate.ProposalState {
		return gate.ProposalState{DocPath: "docs/a.md", Section: "A", CommentURL: "http://c", State: status, AppliedSHA: "abcdef1234567"}
	}
	tests := []struct {
		name  string
		state gate.PRState
		want  []string
		not   []string
	}{
		{
			name:  "open proposal, nothing ticked",
			state: gate.PRState{HeadSHA: "h", Proposals: []gate.ProposalState{prop(gate.ProposalOpen)}},
			want:  []string{"| open |", "- [ ] Apply all", "- [ ] Skip this commit", "- [ ] Skip this PR", "/pollux-agent apply", "/pollux-agent skip <reason>", "/pollux-agent skip-pr <reason>"},
			not:   []string{"[x]", "Skipped by", "Waiting for"},
		},
		{
			name:  "all applied replaces apply all with a status line",
			state: gate.PRState{Proposals: []gate.ProposalState{prop(gate.ProposalApplied), prop(gate.ProposalOutdated)}},
			want:  []string{"applied (abcdef1)", "✅ All proposals applied."},
			not:   []string{"abcdef12", "Apply all"},
		},
		{
			name:  "applied plus open leaves apply all unticked",
			state: gate.PRState{Proposals: []gate.ProposalState{prop(gate.ProposalApplied), prop(gate.ProposalOpen)}},
			want:  []string{"- [ ] Apply all"},
			not:   []string{"All proposals applied", "[x] Apply all"},
		},
		{
			name:  "none applied leaves apply all unticked",
			state: gate.PRState{Proposals: []gate.ProposalState{prop(gate.ProposalOutdated)}},
			want:  []string{"- [ ] Apply all"},
		},
		{
			name:  "fork has no apply all",
			state: gate.PRState{Fork: true, Proposals: []gate.ProposalState{prop(gate.ProposalApplied)}},
			want:  []string{"Apply all is not available", "fork"},
			not:   []string{"Apply all\n"},
		},
		{
			name:  "pending commit skip",
			state: gate.PRState{PendingSkip: &gate.SkipAsk{User: "ann", Scope: gate.SkipCommit}},
			want:  []string{"- [x] Skip this commit", "- [ ] Skip this PR", "Waiting for @ann to post the reason as a new comment on this PR."},
		},
		{
			name:  "pending pr skip",
			state: gate.PRState{PendingSkip: &gate.SkipAsk{User: "ann", Scope: gate.SkipPR}},
			want:  []string{"- [ ] Skip this commit", "- [x] Skip this PR", "Waiting for @ann"},
		},
		{
			name:  "commit skip at this head",
			state: gate.PRState{HeadSHA: "h1", Skip: &gate.Skip{User: "bob", Scope: gate.SkipCommit, Reason: "typo", HeadSHA: "h1"}},
			want:  []string{"- [x] Skip this commit", "- [ ] Skip this PR", "Skipped by @bob for this commit: typo"},
		},
		{
			name:  "commit skip at an older head is inactive",
			state: gate.PRState{HeadSHA: "h2", Skip: &gate.Skip{User: "bob", Scope: gate.SkipCommit, Reason: "typo", HeadSHA: "h1"}},
			want:  []string{"- [ ] Skip this commit"},
			not:   []string{"Skipped by", "[x]"},
		},
		{
			name:  "commit skip at this head marks open proposals skipped",
			state: gate.PRState{HeadSHA: "h1", Skip: &gate.Skip{User: "bob", Scope: gate.SkipCommit, Reason: "typo", HeadSHA: "h1"}, Proposals: []gate.ProposalState{prop(gate.ProposalOpen), prop(gate.ProposalApplied)}},
			want:  []string{"| skipped |", "applied (abcdef1)"},
			not:   []string{"| open |"},
		},
		{
			name:  "commit skip heading does not claim open proposals",
			state: gate.PRState{HeadSHA: "h1", Skip: &gate.Skip{User: "bob", Scope: gate.SkipCommit, Reason: "typo", HeadSHA: "h1"}, Proposals: []gate.ProposalState{prop(gate.ProposalOpen)}},
			want:  []string{"proposed 1 doc update; the check is skipped.", "- [ ] Apply all"},
			not:   []string{"proposes"},
		},
		{
			name:  "skip heading with everything applied claims nothing open",
			state: gate.PRState{HeadSHA: "h1", Skip: &gate.Skip{User: "bob", Scope: gate.SkipCommit, Reason: "typo", HeadSHA: "h1"}, Proposals: []gate.ProposalState{prop(gate.ProposalApplied)}},
			want:  []string{"**pollux-agent**: the check is skipped."},
			not:   []string{"proposed 0", "proposes"},
		},
		{
			name:  "lapsed commit skip reads open again",
			state: gate.PRState{HeadSHA: "h2", Skip: &gate.Skip{User: "bob", Scope: gate.SkipCommit, Reason: "typo", HeadSHA: "h1"}, Proposals: []gate.ProposalState{prop(gate.ProposalOpen)}},
			want:  []string{"| open |"},
			not:   []string{"| skipped |"},
		},
		{
			name:  "pr skip survives a new head",
			state: gate.PRState{HeadSHA: "h2", Skip: &gate.Skip{User: "bob", Scope: gate.SkipPR, Reason: "wip", HeadSHA: "h1"}},
			want:  []string{"- [ ] Skip this commit", "- [x] Skip this PR", "Skipped by @bob for this PR: wip"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := renderedSummary(t, tc.state)
			if !strings.HasPrefix(got, "<!-- pollux-agent:summary -->") {
				t.Errorf("summary does not start with the marker:\n%s", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("summary missing %q:\n%s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("summary contains %q:\n%s", n, got)
				}
			}
		})
	}
}

const injectedReason = "Docs drift. @acme/security see ![](https://evil.example/px.png) [x](https://evil.example) <!-- <details>"

// renderedProposal publishes p as a proposal and returns its review comment
// body, the summary comment body and the check run summary.
func renderedProposal(t *testing.T, p review.Proposal) (comment, summary, check string) {
	t.Helper()

	gh := &fakeGitHub{}
	proposalService(t, gh, &fakeStore{}, review.Proposals{p})
	if len(gh.reviewComments) != 1 || len(gh.comments) < 2 || len(gh.updates) != 1 {
		t.Fatalf("review comments = %d, comments = %d, check updates = %d, want 1, 2 and 1", len(gh.reviewComments), len(gh.comments), len(gh.updates))
	}
	return gh.reviewComments[0].Body, gh.comments[0].Body, gh.updates[0].run.Summary
}

func TestProseModelTextIsInert(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	p.Reason = injectedReason
	comment, _, check := renderedProposal(t, p)

	for name, text := range map[string]string{
		"comment":   strings.TrimPrefix(comment, strings.SplitN(comment, "\n", 2)[0]),
		"check run": check,
	} {
		for _, bad := range []string{"@acme", "![", "](", "<!--", "<details>", "://"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s = %q, want no %q", name, text, bad)
			}
		}
		if !strings.Contains(text, "@\u200bacme/security see") || !strings.Contains(text, "Docs drift.") {
			t.Errorf("%s = %q, want the reason's words intact", name, text)
		}
	}
	if got := strings.Count(comment, "- [ ] Apply this change"); got != 1 {
		t.Errorf("comment has %d Apply boxes, want 1:\n%s", got, comment)
	}
	if !strings.Contains(comment, "```diff") {
		t.Errorf("comment = %q, want the diff", comment)
	}
}

func TestProseForgedApplyBoxIsNotACheckboxInCheckRun(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	p.Reason = "- [ ] Apply this change"
	_, _, check := renderedProposal(t, p)

	if strings.Contains(check, "- [ ]") {
		t.Errorf("check run summary = %q, want no checkbox", check)
	}
}

func TestProseDropsBidiAndControlCharacters(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A\u202eB\x00C")
	p.Reason = "ok\u202e evil\x00 fam\u200d\u200fily \u2066x"
	comment, summary, check := renderedProposal(t, p)

	for name, text := range map[string]string{"summary": summary, "check run": check} {
		for _, bad := range []string{"\u202e", "\x00", "\u200f", "\u2066"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s = %q, want no %q", name, text, bad)
			}
		}
	}
	if !strings.Contains(check, "ok evil fam\u200dily x") {
		t.Errorf("check run = %q, want the words kept and the ZWJ intact", check)
	}
	if !strings.Contains(comment, "\n\nok evil fam\u200dily x\n\n") {
		t.Errorf("comment = %q, want the reason line cleaned", comment)
	}
	if !strings.Contains(summary, "`ABC`") {
		t.Errorf("summary = %q, want the section as `ABC`", summary)
	}
}

func TestProseBlockStartRule(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	p.Reason = "___"
	comment, _, check := renderedProposal(t, p)

	if !strings.Contains(comment, "\n\n\\___\n\n") || !strings.Contains(check, ": \\___") {
		t.Errorf("comment = %q, check = %q, want the reason as \\___", comment, check)
	}
}

func TestProseLongReasonIsCapped(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	p.Reason = strings.Repeat("word ", 20000) + "`<img src=x>`"
	comment, _, check := renderedProposal(t, p)

	for name, text := range map[string]string{"comment": comment, "check run": check} {
		if strings.Contains(text, "<img") {
			t.Errorf("%s contains <img", name)
		}
		if len(text) > 5000 {
			t.Errorf("%s is %d bytes, want the reason capped", name, len(text))
		}
	}
	if !strings.Contains(check, "…") {
		t.Errorf("check run = %q, want an ellipsis", check)
	}
}

func TestSummaryManyLongReasonsStayWithinLimit(t *testing.T) {
	t.Parallel()

	var ps review.Proposals
	for i := range 200 {
		p := proposal(fmt.Sprintf("docs/%d.md", i), "A")
		p.Reason = strings.Repeat("x", 900) + " `<img src=x>` " + strings.Repeat("<", 40)
		ps = append(ps, p)
	}
	gh := &fakeGitHub{}
	proposalService(t, gh, &fakeStore{}, ps)
	if len(gh.updates) != 1 {
		t.Fatalf("check run updates = %d, want 1", len(gh.updates))
	}
	got := gh.updates[0].run.Summary
	if len(got) > 65535 {
		t.Errorf("summary is %d bytes, want at most 65535", len(got))
	}
	if !regexp.MustCompile(`\n- … and \d+ more$`).MatchString(got) {
		t.Errorf("summary ends %q, want the more line", got[max(0, len(got)-80):])
	}
	if strings.Contains(strings.ReplaceAll(got, "`<img src=x>`", ""), "<img") {
		t.Error("summary has a raw <img outside a code span")
	}
}

func TestProseNoImpactReasonIsOneInertLine(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	proposalService(t, gh, &fakeStore{}, review.NoImpact{Reason: "Nothing to do.\n\n" + injectedReason + "\n- [ ] Apply this change\r\n"})
	if len(gh.updates) != 1 {
		t.Fatalf("check run updates = %d, want 1", len(gh.updates))
	}
	got := gh.updates[0].run.Summary
	if strings.Contains(got, "\n") {
		t.Errorf("summary = %q, want one line", got)
	}
	for _, bad := range []string{"@acme", "![", "](", "<!--", "<details>", "- [ ]", "://"} {
		if strings.Contains(got, bad) {
			t.Errorf("summary = %q, want no %q", got, bad)
		}
	}
	if !strings.HasPrefix(got, "Nothing to do. Docs drift.") || !strings.Contains(got, "Apply this change") {
		t.Errorf("summary = %q, want the words intact", got)
	}
}

func TestProseOrdinaryReasonRenders(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, reason, want string }{
		{name: "inline code span", reason: "Update `make run` in setup.", want: "Update `make run` in setup."},
		{name: "double backtick span", reason: "Use ``a ` b`` here.", want: "Use ``a ` b`` here."},
		{name: "span contents untouched", reason: "Run `a <b> @c [d]`.", want: "Run `a <b> @c [d]`."},
		{name: "unbalanced backtick", reason: "Odd ` tick and ``` fence", want: "Odd \\` tick and \\`\\`\\` fence"},
		{name: "whitespace collapses", reason: "a\n\tb   c", want: "a b c"},
		{name: "leading heading", reason: "# Title", want: "\\# Title"},
		{name: "leading quote", reason: "> quote", want: "\\> quote"},
		{name: "leading numbered list", reason: "1. first", want: "1\\. first"},
		{name: "backslash before bracket", reason: `\[x](u)`, want: `\\\[x\]\(u)`},
		{name: "html", reason: "use <b>bold</b>", want: "use &lt;b>bold&lt;/b>"},
		{name: "bare urls", reason: "see https://evil.example and WWW.evil.example", want: "see https:\u200b//evil.example and WWW\u200b.evil.example"},
		{name: "entity mention", reason: "ping &#64;acme", want: "ping &amp;#\u200b64;acme"},
		{name: "issue references", reason: "fixes #12, other/repo#3, gh-4 and GH-x", want: "fixes #\u200b12, other/repo#\u200b3, gh\u200b-4 and GH-x"},
		{name: "leading issue reference", reason: "#12 done", want: "\\#\u200b12 done"},
		{name: "dropped bidi mark before heading", reason: "\u202e # x", want: "\\# x"},
		{name: "dropped controls before list", reason: "\x00 \x00 - item", want: "\\- item"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := proposal("docs/a.md", "A")
			p.Reason = tc.reason
			comment, _, check := renderedProposal(t, p)
			if !strings.Contains(comment, "\n\n"+tc.want+"\n\n") {
				t.Errorf("comment = %q, want the reason line %q", comment, tc.want)
			}
			if !strings.Contains(check, ": "+tc.want) {
				t.Errorf("check summary = %q, want %q", check, tc.want)
			}
		})
	}
}

func TestSpanModelIdentifiersAreOneCodeSpan(t *testing.T) {
	t.Parallel()

	const hostile = "``a`` | @team <!-- [l](u)"

	checkbox := proposal("docs/a.md", hostile)
	checkbox.IndexEntry = hostile
	comment, summary, check := renderedProposal(t, checkbox)

	wantSpan := "``` ``a`` | @team <!-- [l](u) ```"
	if want := "`docs/a.md`, section " + wantSpan; !strings.Contains(comment, want) {
		t.Errorf("comment = %q, want %q", comment, want)
	}
	if want := "Index entry: " + wantSpan + "\n"; !strings.Contains(comment, want) {
		t.Errorf("comment = %q, want %q", comment, want)
	}

	var row string
	for l := range strings.SplitSeq(summary, "\n") {
		if strings.HasPrefix(l, "| `docs/a.md`") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("summary = %q, want a table row", summary)
	}
	if want := "``` ``a`` \\| @team <!-- [l](u) ```"; !strings.Contains(row, want) {
		t.Errorf("row = %q, want %q", row, want)
	}
	if cells := strings.Count(strings.ReplaceAll(row, `\|`, ""), "|"); cells != 5 {
		t.Errorf("row = %q has %d pipes, want 5 (4 cells)", row, cells)
	}
	if !strings.Contains(check, "`docs/a.md`") {
		t.Errorf("check summary = %q, want the doc path in a code span", check)
	}
}

func TestSpanPipeAfterBackslashStaysInOneCell(t *testing.T) {
	t.Parallel()

	state := gate.PRState{Proposals: []gate.ProposalState{{DocPath: "docs/a.md", Section: `a\|[x](https://e.example)|b`, State: gate.ProposalOpen}}}
	row := ""
	for l := range strings.SplitSeq(renderedSummary(t, state), "\n") {
		if strings.HasPrefix(l, "| `docs/a.md`") {
			row = l
		}
	}
	if row == "" {
		t.Fatal("summary has no table row")
	}
	// A scanner that pairs a backslash with any next character, the stricter
	// reading of GFM's cell rule.
	cells, pipes := 0, 0
	for i := 0; i < len(row); i++ {
		switch row[i] {
		case '\\':
			i++
		case '|':
			pipes++
		}
	}
	cells = pipes - 1
	if cells != 4 {
		t.Errorf("row = %q has %d cells, want 4", row, cells)
	}
	if !strings.Contains(row, "[x](https://e.example)") || strings.Count(row, "`") != 4 {
		t.Errorf("row = %q, want the link text inside one code span", row)
	}
}

func TestSpanDocPathIsLiteralInCheckSummary(t *testing.T) {
	t.Parallel()

	p := proposal("docs/*a_[b]<c>.md", "A")
	comment, summary, check := renderedProposal(t, p)

	for name, text := range map[string]string{"comment": comment, "summary": summary, "check run": check} {
		if !strings.Contains(text, "`docs/*a_[b]<c>.md`") {
			t.Errorf("%s = %q, want the doc path in a code span", name, text)
		}
	}
}

func TestProposalCommentSitsOnItsAnchorLine(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 3, End: 9}}}}
	p := review.Proposal{DocPath: "docs/a.md", Section: "A", Reason: "r", Anchor: review.Anchor{File: "a.go", Line: 5}, Content: "## A\nnew\n"}

	_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, changed, nil)

	if len(writes) == 0 {
		t.Fatal("Reconcile wrote nothing, want the proposal comment")
	}
	if got := writes[0].Review; got.Path != "a.go" || got.Line != 5 || got.File {
		t.Errorf("comment = %+v, want a line comment at a.go:5", got)
	}
}

func TestProposalCommentOutsideHunksIsFileLevel(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 3, End: 9}}}}
	p := review.Proposal{DocPath: "docs/a.md", Section: "A", Reason: "r", Anchor: review.Anchor{File: "a.go", Line: 279}, Content: "## A\nnew\n"}

	_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, changed, nil)

	if len(writes) == 0 {
		t.Fatal("Reconcile wrote nothing, want the proposal comment")
	}
	if got := writes[0].Review; got.Path != "a.go" || got.Line != 0 || !got.File {
		t.Errorf("comment = %+v, want a file-level comment on a.go", got)
	}
}
