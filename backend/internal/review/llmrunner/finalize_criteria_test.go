package llmrunner_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func TestStart_SectionEditOfDocWithBrokenFrontmatterAtHeadCarriesOriginal(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	headSHA := commitDoc(t, repoDir, "docs/x.md", "---\ntitle: [unclosed\n---\n# Top\n\n## X\nold behavior.\n")

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true), submitResponse(proposalFor("docs/x.md", 2)), verifyResponse(true),
	}}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	req := testRequest(headSHA)
	req.BaseSHA = baseSHA

	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	proposals, ok := result.Verdict.(review.Proposals)
	if !ok || len(proposals) != 1 {
		t.Fatalf("Verdict = %#v, want one accepted proposal", result.Verdict)
	}
	if !strings.Contains(proposals[0].Original, "old behavior.") || proposals[0].Lines == (review.LineRange{}) {
		t.Errorf("Original, Lines = %q, %+v; want the section's current text", proposals[0].Original, proposals[0].Lines)
	}
}

func TestStart_NewDocAtAnOversizedFileIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	big := "---\ntitle: Other\n---\n" + strings.Repeat("x", 2<<20)
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "other.md"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	headSHA := commitDoc(t, repoDir, "docs/keep.txt", "keep\n")

	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(newDocProposal("other.go")), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_NewDocUnderASymlinkedDirectoryIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(repoDir, "docs", "link")); err != nil {
		t.Fatalf("symlink docs/link: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "docs/keep.txt", "keep\n")

	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/link/new.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_NewDocUnderAnInCloneSymlinkIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "docs", "real"), 0o700); err != nil {
		t.Fatalf("mkdir docs/real: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(repoDir, "docs", "link")); err != nil {
		t.Fatalf("symlink docs/link: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "docs/real/keep.txt", "keep\n")

	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/link/new.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_HeadReadErrorFailsTheRun(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	proposal := newDocProposal("other.go")
	// A path component over the file system's name limit makes lstat fail with
	// an error that is neither "missing" nor fixable by the model.
	proposal["doc_path"] = "docs/" + strings.Repeat("a", 300) + ".md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true), newDocResponse(true), submitResponse(proposal),
	}}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	req := testRequest(headSHA)
	req.ChangedFiles = changed

	if started, err := runner.Start(t.Context(), req); err == nil {
		t.Fatalf("Start() = %#v, nil; want the run to fail on the head read", started)
	}
	if len(model.calls) != 3 {
		t.Errorf("model calls = %d, want 3 (no retry after the head read failed)", len(model.calls))
	}
}

func TestStart_NewDocUnderAnExistingFileIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/x.md/new.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}
