package llmrunner_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestStart_MixedChangeWithUnimpactedCandidateStillGetsNewDoc(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	verdict, _ := startUncovered(t, changed,
		triageResponse(false), newDocResponse(true),
		submitResponse(newDocProposal("other.go")), verifyResponse(true))
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 1 {
		t.Fatalf("Verdict = %#v, want one new-doc proposal", verdict)
	}
	if p := proposals[0]; p.DocPath != "docs/other.md" || p.Section != "" {
		t.Errorf("proposal = %+v, want new doc docs/other.md", p)
	}
}
