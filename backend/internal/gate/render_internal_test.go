package gate

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestRenderCheckboxFork(t *testing.T) {
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
			body := renderCheckbox("id1", p, tc.fork)
			if got := strings.Contains(body, "- [ ] "+applyLabel); got != tc.wantCheckbox {
				t.Errorf("renderCheckbox(fork=%v) has apply checkbox = %v, want %v:\n%s", tc.fork, got, tc.wantCheckbox, body)
			}
			if got := strings.Contains(body, "fork"); got == tc.wantCheckbox {
				t.Errorf("renderCheckbox(fork=%v) mentions fork = %v, want %v", tc.fork, got, !tc.wantCheckbox)
			}
		})
	}
}

func TestTickApply(t *testing.T) {
	t.Parallel()

	body := renderCheckbox("id1", review.Proposal{DocPath: "docs/a.md", Section: "Usage", Content: "x\n"}, false)
	got, ok := tickApply(body)
	if !ok || !strings.Contains(got, "- [x] "+applyLabel) || strings.Contains(got, "- [ ] "+applyLabel) {
		t.Errorf("tickApply(unticked) = ok %v:\n%s", ok, got)
	}
	if again, ok := tickApply(got); ok || again != got {
		t.Errorf("tickApply(ticked) = ok %v, changed %v, want false, false", ok, again != got)
	}
	forkBody := renderCheckbox("id1", review.Proposal{DocPath: "docs/a.md", Section: "Usage", Content: "x\n"}, true)
	if _, ok := tickApply(forkBody); ok {
		t.Error("tickApply(fork body) ok = true, want false")
	}
}

func TestRenderSummary(t *testing.T) {
	t.Parallel()

	prop := func(status ProposalStatus) ProposalState {
		return ProposalState{DocPath: "docs/a.md", Section: "A", CommentURL: "http://c", State: status, AppliedSHA: "abcdef1234567"}
	}
	tests := []struct {
		name  string
		state PRState
		want  []string
		not   []string
	}{
		{
			name:  "open proposal, nothing ticked",
			state: PRState{HeadSHA: "h", Proposals: []ProposalState{prop(ProposalOpen)}},
			want:  []string{"| open |", "- [ ] " + applyAllLabel, "- [ ] " + skipCommitLabel, "- [ ] " + skipPRLabel, "/pollux-agent apply", "/pollux-agent skip <reason>", "/pollux-agent skip-pr <reason>"},
			not:   []string{"[x]", "Skipped by", "Waiting for"},
		},
		{
			name:  "all applied replaces apply all with a status line",
			state: PRState{Proposals: []ProposalState{prop(ProposalApplied), prop(ProposalOutdated)}},
			want:  []string{"applied (abcdef1)", "✅ All proposals applied."},
			not:   []string{"abcdef12", applyAllLabel},
		},
		{
			name:  "applied plus open leaves apply all unticked",
			state: PRState{Proposals: []ProposalState{prop(ProposalApplied), prop(ProposalOpen)}},
			want:  []string{"- [ ] " + applyAllLabel},
			not:   []string{"All proposals applied", "[x] " + applyAllLabel},
		},
		{
			name:  "none applied leaves apply all unticked",
			state: PRState{Proposals: []ProposalState{prop(ProposalOutdated)}},
			want:  []string{"- [ ] " + applyAllLabel},
		},
		{
			name:  "fork has no apply all",
			state: PRState{Fork: true, Proposals: []ProposalState{prop(ProposalApplied)}},
			want:  []string{"Apply all is not available", "fork"},
			not:   []string{applyAllLabel + "\n"},
		},
		{
			name:  "pending commit skip",
			state: PRState{PendingSkip: &SkipAsk{User: "ann", Scope: SkipCommit}},
			want:  []string{"- [x] " + skipCommitLabel, "- [ ] " + skipPRLabel, "Waiting for @ann to reply with a reason."},
		},
		{
			name:  "pending pr skip",
			state: PRState{PendingSkip: &SkipAsk{User: "ann", Scope: SkipPR}},
			want:  []string{"- [ ] " + skipCommitLabel, "- [x] " + skipPRLabel, "Waiting for @ann"},
		},
		{
			name:  "commit skip at this head",
			state: PRState{HeadSHA: "h1", Skip: &Skip{User: "bob", Scope: SkipCommit, Reason: "typo", HeadSHA: "h1"}},
			want:  []string{"- [x] " + skipCommitLabel, "- [ ] " + skipPRLabel, "Skipped by @bob for this commit: typo"},
		},
		{
			name:  "commit skip at an older head is inactive",
			state: PRState{HeadSHA: "h2", Skip: &Skip{User: "bob", Scope: SkipCommit, Reason: "typo", HeadSHA: "h1"}},
			want:  []string{"- [ ] " + skipCommitLabel},
			not:   []string{"Skipped by", "[x]"},
		},
		{
			name:  "pr skip survives a new head",
			state: PRState{HeadSHA: "h2", Skip: &Skip{User: "bob", Scope: SkipPR, Reason: "wip", HeadSHA: "h1"}},
			want:  []string{"- [ ] " + skipCommitLabel, "- [x] " + skipPRLabel, "Skipped by @bob for this PR: wip"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderSummary(tc.state)
			if !strings.HasPrefix(got, summaryMarker) {
				t.Errorf("renderSummary() does not start with the marker:\n%s", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("renderSummary() missing %q:\n%s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("renderSummary() contains %q:\n%s", n, got)
				}
			}
		})
	}
}
