package gate_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func proposal(doc, section string) review.Proposal {
	return review.Proposal{
		DocPath: doc, Section: section, Reason: "why " + section,
		Anchor:  review.Anchor{File: "a.go", Line: 4},
		Content: "## " + section + "\nnew\n", Original: "## " + section + "\nold\n",
	}
}

func proposalIDs(state gate.PRState) map[string]gate.ProposalStatus {
	got := map[string]gate.ProposalStatus{}
	for _, p := range state.Proposals {
		got[p.DocPath+"#"+p.Section] = p.State
	}
	return got
}

// countWrites counts the comment creates and edits writes asks for, the summary
// write among them: a create while the state has no summary comment, else an edit.
func countWrites(writes gate.CommentWrites, state gate.PRState) (creates, edits int) {
	creates, edits = len(writes.Creates), len(writes.Edits)
	if writes.Summary {
		if state.SummaryCommentID == 0 {
			creates++
		} else {
			edits++
		}
	}
	return creates, edits
}

func TestReconcile(t *testing.T) {
	t.Parallel()

	pr := gate.PullRequest{InstallationID: 1, Owner: "o", Repo: "r", Number: 3, HeadSHA: "0123456789"}
	a, b := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")
	idA, idB := gate.ProposalID("docs/a.md", "A"), gate.ProposalID("docs/b.md", "B")
	prev := gate.PRState{SummaryCommentID: 90, Proposals: []gate.ProposalState{
		{ID: idA, DocPath: "docs/a.md", Section: "A", CommentID: 1, CommentURL: "u1", State: gate.ProposalOpen},
		{ID: idB, DocPath: "docs/b.md", Section: "B", CommentID: 2, CommentURL: "u2", State: gate.ProposalOpen},
	}}
	old := []gate.Comment{
		{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + idA + " -->\n\nold A body"},
		{ID: 2, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + idB + " -->\n\nold B body"},
		{ID: 90, Mine: true, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->"},
	}

	tests := []struct {
		name         string
		prev         gate.PRState
		verdict      review.Verdict
		existing     []gate.Comment
		wantCreates  int
		wantEdits    int
		wantStates   map[string]gate.ProposalStatus
		wantSummary  bool
		wantSumID    int64
		wantOutdated []string
	}{
		{
			name: "first run", verdict: review.Proposals{a, b},
			wantCreates: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true,
		},
		{
			name: "same proposals edit only", prev: prev, verdict: review.Proposals{a, b}, existing: old,
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "some gone are outdated", prev: prev, verdict: review.Proposals{a}, existing: old,
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "outdated"}, wantSummary: true, wantSumID: 90,
			wantOutdated: []string{"old B body"},
		},
		{
			name: "no impact outdates all", prev: prev, verdict: review.NoImpact{Reason: "x"}, existing: old,
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "outdated", "docs/b.md#B": "outdated"}, wantSummary: true, wantSumID: 90,
			wantOutdated: []string{"old A body", "old B body"},
		},
		{
			name: "no impact without prior state writes nothing", verdict: review.NoImpact{Reason: "x"}, wantStates: map[string]gate.ProposalStatus{},
		},
		{
			name: "duplicate ids keep first", verdict: review.Proposals{a, a},
			wantCreates: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open"}, wantSummary: true,
		},
		{
			name:    "outdated returns and reopens",
			prev:    gate.PRState{SummaryCommentID: 90, Proposals: []gate.ProposalState{{ID: idA, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOutdated}}},
			verdict: review.Proposals{a}, existing: []gate.Comment{old[0], old[2]},
			wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "deleted open proposal is recreated", prev: prev, verdict: review.Proposals{a, b}, existing: []gate.Comment{old[0], old[2]},
			wantCreates: 1, wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "deleted outdated proposal is not written", prev: prev, verdict: review.Proposals{a}, existing: []gate.Comment{old[0], old[2]},
			wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "outdated"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "deleted summary is recreated", prev: prev, verdict: review.Proposals{a, b}, existing: old[:2],
			wantCreates: 1, wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true,
		},
		{
			name: "crash recovery reuses marked comments", verdict: review.Proposals{a, b},
			existing:  append(slices.Clone(old), gate.Comment{ID: 91, Mine: true, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->\nrows"}),
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true, wantSumID: 90,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state, writes := gate.Reconcile(tc.prev, pr, tc.verdict, nil, tc.existing)
			creates, edits := countWrites(writes, state)
			if creates != tc.wantCreates || edits != tc.wantEdits {
				t.Errorf("writes = %d creates, %d edits, want %d, %d", creates, edits, tc.wantCreates, tc.wantEdits)
			}
			if diff := cmp.Diff(tc.wantStates, proposalIDs(state)); diff != "" {
				t.Errorf("states (-want +got):\n%s", diff)
			}
			hasSummary := writes.Summary
			if hasSummary != tc.wantSummary || state.SummaryCommentID != tc.wantSumID {
				t.Errorf("summary write = %v id %d, want %v id %d", hasSummary, state.SummaryCommentID, tc.wantSummary, tc.wantSumID)
			}
			for _, want := range tc.wantOutdated {
				found := false
				for _, w := range writes.Edits {
					found = found || (strings.Contains(w.Body, want) && strings.Contains(w.Body, "Outdated: no longer needed as of 0123456") && strings.Contains(w.Body, "<details>"))
				}
				if !found {
					t.Errorf("no outdated write keeping %q in %+v", want, writes)
				}
			}
		})
	}
}

func TestReconcileKeepsRowOrder(t *testing.T) {
	t.Parallel()

	prev := gate.PRState{Proposals: []gate.ProposalState{
		{ID: gate.ProposalID("docs/a.md", "A"), DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen},
	}}
	state, _ := gate.Reconcile(prev, testPR(), review.Proposals{proposal("docs/z.md", "Z"), proposal("docs/a.md", "A")}, nil, nil)
	if len(state.Proposals) != 2 || state.Proposals[0].DocPath != "docs/a.md" || state.Proposals[1].DocPath != "docs/z.md" {
		t.Errorf("rows = %+v, want existing first, new appended", state.Proposals)
	}
}

func TestReconcileVariants(t *testing.T) {
	t.Parallel()

	suggest := review.Proposal{
		DocPath: "docs/a.md", Section: "Mid", Reason: "r", Anchor: review.Anchor{File: "a.go", Line: 4},
		Original: "## Mid\nmid body\n\n", Content: "## Mid\nnew ```go\nx\n```\n", Lines: review.LineRange{Start: 9, End: 11},
	}
	single := suggest
	single.Lines = review.LineRange{Start: 9, End: 9}
	newDoc := review.Proposal{DocPath: "docs/n.md", Anchor: review.Anchor{File: "a.go", Line: 4}, Reason: "r", Content: "# N\n", IndexEntry: "- n"}
	diffFile := func(path string, hunks ...review.LineRange) []review.ChangedFile {
		return []review.ChangedFile{{Path: path, Hunks: hunks}}
	}

	tests := []struct {
		name        string
		p           review.Proposal
		changed     []review.ChangedFile
		wantPath    string
		wantStart   int
		wantLine    int
		wantSuggest bool
	}{
		{"in hunk", suggest, diffFile("docs/a.md", review.LineRange{Start: 1, End: 20}), "docs/a.md", 9, 11, true},
		{"single line has no start", single, diffFile("docs/a.md", review.LineRange{Start: 9, End: 9}), "docs/a.md", 0, 9, true},
		{"partly outside hunk", suggest, diffFile("docs/a.md", review.LineRange{Start: 10, End: 20}), "a.go", 0, 4, false},
		{"doc not in diff", suggest, diffFile("other.md", review.LineRange{Start: 1, End: 20}), "a.go", 0, 4, false},
		{"new doc", newDoc, diffFile("docs/n.md", review.LineRange{Start: 1, End: 20}), "a.go", 0, 4, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{tc.p}, tc.changed, nil)
			rc := writes.Creates[0].Comment
			if rc.Path != tc.wantPath || rc.StartLine != tc.wantStart || rc.Line != tc.wantLine || rc.CommitSHA != "abc123" {
				t.Errorf("comment = %+v, want %s %d-%d on abc123", rc, tc.wantPath, tc.wantStart, tc.wantLine)
			}
			if got := strings.Contains(rc.Body, "suggestion\n"); got != tc.wantSuggest {
				t.Errorf("suggestion block = %v, want %v:\n%s", got, tc.wantSuggest, rc.Body)
			}
			if got := strings.Contains(rc.Body, "- [ ] Apply this change"); got == tc.wantSuggest {
				t.Errorf("checkbox = %v, want %v", got, !tc.wantSuggest)
			}
		})
	}

	_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{suggest}, diffFile("docs/a.md", review.LineRange{Start: 1, End: 20}), nil)
	body := writes.Creates[0].Comment.Body
	want := "````suggestion\n## Mid\nnew ```go\nx\n```\n\n````\n"
	if !strings.HasSuffix(body, want) {
		t.Errorf("suggestion body = %q, want suffix %q (fence grown, trailing blank line kept)", body, want)
	}
}

func TestReconcileEditKeepsVariantSafe(t *testing.T) {
	t.Parallel()

	p := review.Proposal{
		DocPath: "docs/a.md", Section: "Mid", Reason: "r", Anchor: review.Anchor{File: "a.go", Line: 4},
		Original: "## Mid\nbody\n", Content: "## Mid\nnew\n", Lines: review.LineRange{Start: 9, End: 10},
	}
	id := gate.ProposalID(p.DocPath, p.Section)
	inDiff := []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: p.DocPath, Section: p.Section, CommentID: 1, State: gate.ProposalOpen}}}
	onCode := gate.Comment{ID: 1, Mine: true, Kind: gate.CommentKindReview, Path: "a.go", Line: 4}
	onDoc := gate.Comment{ID: 1, Mine: true, Kind: gate.CommentKindReview, Path: "docs/a.md", StartLine: 9, Line: 10}
	moved := gate.Comment{ID: 1, Mine: true, Kind: gate.CommentKindReview, Path: "docs/a.md", StartLine: 5, Line: 6}

	tests := []struct {
		name        string
		existing    gate.Comment
		changed     []review.ChangedFile
		wantSuggest bool
	}{
		{"checkbox on code stays checkbox when section enters the diff", onCode, inDiff, false},
		{"suggestion on same lines stays suggestion", onDoc, inDiff, true},
		{"suggestion whose lines moved becomes checkbox", moved, inDiff, false},
		{"suggestion leaving the diff becomes checkbox", onDoc, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, writes := gate.Reconcile(prev, testPR(), review.Proposals{p}, tc.changed, []gate.Comment{tc.existing})
			if len(writes.Edits) == 0 || writes.Edits[0].ID != 1 {
				t.Fatalf("writes = %+v, want an edit of comment 1 first", writes)
			}
			if got := strings.Contains(writes.Edits[0].Body, "suggestion\n"); got != tc.wantSuggest {
				t.Errorf("suggestion body = %v, want %v:\n%s", got, tc.wantSuggest, writes.Edits[0].Body)
			}
		})
	}
}

func TestReconcileIgnoresForeignMarkers(t *testing.T) {
	t.Parallel()

	a := proposal("docs/a.md", "A")
	id := gate.ProposalID("docs/a.md", "A")
	marker := "<!-- pollux-agent:proposal:" + id + " -->"
	tests := []struct {
		name     string
		existing gate.Comment
	}{
		{"forged proposal marker", gate.Comment{ID: 7, Kind: gate.CommentKindReview, Body: marker + "\n\nfake"}},
		{"forged summary marker", gate.Comment{ID: 8, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->\nfake"}},
		{"own comment with marker not on first line", gate.Comment{ID: 9, Mine: true, Kind: gate.CommentKindReview, Body: "text\n" + marker}},
		{"own summary with marker not on first line", gate.Comment{ID: 10, Mine: true, Kind: gate.CommentKindIssue, Body: "text <!-- pollux-agent:summary -->"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{a}, nil, []gate.Comment{tc.existing})
			creates, edits := countWrites(writes, state)
			if creates != 2 || edits != 0 || state.SummaryCommentID != 0 || state.Proposals[0].CommentID != 0 {
				t.Errorf("writes = %d creates, %d edits, state = %+v, want 2 creates, nothing adopted", creates, edits, state)
			}
		})
	}
}

func TestReconcileRecreatesStateCommentNotMine(t *testing.T) {
	t.Parallel()

	a := proposal("docs/a.md", "A")
	id := gate.ProposalID("docs/a.md", "A")
	prev := gate.PRState{SummaryCommentID: 90, Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen}}}
	existing := []gate.Comment{
		{ID: 1, Kind: gate.CommentKindReview, Body: "x"},
		{ID: 90, Kind: gate.CommentKindIssue, Body: "y"},
	}

	state, writes := gate.Reconcile(prev, testPR(), review.Proposals{a}, nil, existing)
	creates, edits := countWrites(writes, state)
	if creates != 2 || edits != 0 || state.SummaryCommentID != 0 || state.Proposals[0].CommentID != 0 {
		t.Errorf("writes = %d creates, %d edits, state = %+v, want 2 creates, no edits of foreign comments", creates, edits, state)
	}
}

func TestReconcileRestoresOmittedHeading(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "Behavior")
	p.Original = "## Behavior\n\nold text\n"
	p.Content = "new text\n"

	_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, nil, nil)
	if len(writes.Creates) == 0 {
		t.Fatal("Reconcile() returned no creates, want a review comment create")
	}
	want := "-## Behavior\n-\n-old text\n+## Behavior\n+\n+new text\n"
	if body := writes.Creates[0].Comment.Body; !strings.Contains(body, want) {
		t.Errorf("review comment body = %q, want diff %q", body, want)
	}

	p.Content = "### Install\n\nnew text\n"
	_, writes = gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, nil, nil)
	want = "+## Behavior\n+\n+### Install\n+\n+new text\n"
	if body := writes.Creates[0].Comment.Body; !strings.Contains(body, want) {
		t.Errorf("subsection content: review comment body = %q, want diff %q", body, want)
	}
	p.Content = "## Behaviour\n\nnew text\n"
	_, writes = gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, nil, nil)
	if body := writes.Creates[0].Comment.Body; strings.Contains(body, "+## Behavior\n") || !strings.Contains(body, "+## Behaviour\n") {
		t.Errorf("renamed heading: review comment body = %q, want the model's heading kept and no second heading", body)
	}
}
