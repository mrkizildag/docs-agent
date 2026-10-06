package gate_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestProposalCommentBodies(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{changed: []review.ChangedFile{{Path: "docs/s.md", Hunks: []review.LineRange{{Start: 1, End: 10}}, Patch: "@@ -1 +1,10 @@"}}}
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
	for _, want := range []string{"flag renamed", "`docs/a.md`", `"Usage"`, "```diff\n-## Usage\n-old\n+## Usage\n+new\n```", "- [ ] Apply this change"} {
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
	for i, want := range []string{"| `docs/a.md` | Usage |", "| `docs/b.md` | (new doc) |", "| `docs/s.md` | Run |"} {
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
			_, writes := gate.Reconcile(gate.PRState{}, pr, review.Proposals{p}, nil, nil)

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

func TestOutdatedProposalCommentHasNoApplyBox(t *testing.T) {
	t.Parallel()

	id := gate.ProposalID("docs/a.md", "A")
	old := "<!-- pollux-agent:proposal:" + id + " -->\n\nflag renamed\n\n- [ ] Apply this change\n"
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen}}}
	existing := []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: old}}

	_, writes := gate.Reconcile(prev, testPR(), review.NoImpact{Reason: "x"}, nil, existing)

	body := writes[0].Body
	if !strings.Contains(body, "flag renamed") || !strings.Contains(body, "Outdated") {
		t.Errorf("outdated body = %q, want the old text under an outdated notice", body)
	}
	if strings.Contains(body, "[ ]") || strings.Contains(body, "[x]") {
		t.Errorf("outdated body = %q, want no checkbox", body)
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
	svc := newService(gh, &fakeCommentGitHub{}, &fakeStore{stored: state}, gate.Runners{}, nil, nil)
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
			want:  []string{"- [x] Skip this commit", "- [ ] Skip this PR", "Waiting for @ann to reply with a reason."},
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
