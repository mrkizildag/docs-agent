package llmrunner_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

const mainGoChanged = "package main\n\nfunc main() { println() }\n"

func docWithCovers(covers, body string) string {
	return "---\ntitle: X\nsummary: Describes X.\ncovers:" + covers + "\n---\n# X\n\n" + body + "\n"
}

// startBaseToHead runs the runner over the PR whose base is baseSHA and whose
// head is headSHA, with the model answering from script, and returns the
// verdict, the model's calls, and Start's error.
func startBaseToHead(t *testing.T, repoDir, baseSHA, headSHA string, changed []review.ChangedFile, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, []llm.Request, error) {
	t.Helper()

	model := &fakeModel{script: script}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)

	req := testRequest(headSHA)
	req.BaseSHA = baseSHA
	req.ChangedFiles = changed
	started, err := runner.Start(t.Context(), req)
	if err != nil {
		return nil, model.calls, fmt.Errorf("start: %w", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	return result.Verdict, model.calls, nil
}

// mustStartBaseToHead is startBaseToHead for runs that must succeed.
func mustStartBaseToHead(t *testing.T, repoDir, baseSHA, headSHA string, changed []review.ChangedFile, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, []llm.Request) {
	t.Helper()

	verdict, calls, err := startBaseToHead(t, repoDir, baseSHA, headSHA, changed, script...)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	return verdict, calls
}

func mainGoChange() review.ChangedFile {
	return review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"}
}

func TestStart_DocThatDropsItsCoversInThePRIsStillTriagedFromHead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		covers string
	}{
		{name: "emptied", covers: " []"},
		{name: "narrowed to a glob that misses the changed file", covers: "\n  - cmd/**"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoDir, baseSHA := newGitRepo(t)
			commitDoc(t, repoDir, "docs/x.md", docWithCovers(tc.covers, "head-only body."))
			headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

			_, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()}, triageResponse(false))
			if len(calls) != 1 {
				t.Fatalf("model saw %d calls, want 1 triage call for docs/x.md", len(calls))
			}
			prompt := calls[0].Messages[0].Text
			for _, want := range []string{"docs/x.md", "head-only body."} {
				if !strings.Contains(prompt, want) {
					t.Errorf("triage prompt = %q, want it to contain %q", prompt, want)
				}
			}
			if strings.Contains(prompt, "old behavior.") {
				t.Errorf("triage prompt = %q, want the head text of docs/x.md, not the base text", prompt)
			}
		})
	}
}

func TestStart_DocAddedByThePRIsNotACandidate(t *testing.T) {
	t.Parallel()

	t.Run("covered base doc is triaged and the added one is not", func(t *testing.T) {
		t.Parallel()

		repoDir, baseSHA := newGitRepo(t)
		commitDoc(t, repoDir, "docs/x.md", docWithCovers(" []", "x body."))
		commitDoc(t, repoDir, "docs/new.md", docWithCovers("\n  - main.go", "new body."))
		headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

		_, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()}, triageResponse(false))
		if len(calls) != 1 {
			t.Fatalf("model saw %d calls, want 1 triage call for docs/x.md only", len(calls))
		}
		prompt := calls[0].Messages[0].Text
		if !strings.Contains(prompt, "x body.") || strings.Contains(prompt, "new body.") {
			t.Errorf("triage prompt = %q, want docs/x.md and not docs/new.md", prompt)
		}
	})

	t.Run("uncovered base doc does not become a candidate", func(t *testing.T) {
		t.Parallel()

		repoDir, _ := newGitRepo(t)
		if err := os.Remove(filepath.Join(repoDir, "docs", "x.md")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		baseSHA := commitDoc(t, repoDir, "docs/other.md", docWithCovers("\n  - other.go", "other."))
		commitDoc(t, repoDir, "docs/new.md", docWithCovers("\n  - main.go", "new."))
		headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

		verdict, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()})
		if _, ok := verdict.(review.NoImpact); !ok {
			t.Errorf("verdict = %#v, want NoImpact", verdict)
		}
		if len(calls) != 0 {
			t.Errorf("model saw %d calls, want 0", len(calls))
		}
	})
}

func TestStart_RenamedCoveringDocIsTriagedAtItsNewPath(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	if err := os.Remove(filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("remove docs/x.md: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "docs/renamed.md", docWithCovers("\n  - main.go", "renamed body."))

	_, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{
		mainGoChange(),
		{Path: "docs/renamed.md", PreviousPath: "docs/x.md"},
	}, triageResponse(false))
	if len(calls) != 1 {
		t.Fatalf("model saw %d calls, want 1 triage call for docs/renamed.md", len(calls))
	}
	if prompt := calls[0].Messages[0].Text; !strings.Contains(prompt, "docs/renamed.md") {
		t.Errorf("triage prompt = %q, want it to name docs/renamed.md", prompt)
	}
}

func removeFiles(t *testing.T, dir string, paths ...string) string {
	t.Helper()

	for _, args := range [][]string{append([]string{"rm", "-q"}, paths...), {"commit", "-q", "-m", "rm"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // dir is a t.TempDir path, not external input
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestStart_DeletedCoveringDocIsRestoredWithoutModelCalls(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "README.md"), []byte("## Index\n\n- [X](x.md): about x.\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	baseSHA := commitDoc(t, repoDir, "main.go", "package main\n\nfunc main() {}\n")
	headSHA := removeFiles(t, repoDir, "docs/x.md")

	verdict, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{
		mainGoChange(),
		{Path: "docs/x.md", Removed: true},
	})
	if len(calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(calls))
	}
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 1 {
		t.Fatalf("verdict = %#v, want Proposals with one restore", verdict)
	}
	p := proposals[0]
	if p.DocPath != "docs/x.md" || p.Section != "" || !strings.Contains(p.Content, "old behavior.") || p.IndexEntry != "- [X](x.md): about x." {
		t.Errorf("restore proposal = %+v, want docs/x.md recreated from base with its README entry", p)
	}
	if p.Anchor != (review.Anchor{File: "main.go", Line: 1}) {
		t.Errorf("Anchor = %v, want main.go:1", p.Anchor)
	}
}

func TestStart_DeletedDocWhoseCoveredFileIsAlsoDeletedIsNotRestored(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	headSHA := removeFiles(t, repoDir, "docs/x.md", "main.go")

	verdict, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{
		{Path: "main.go", Removed: true},
		{Path: "docs/x.md", Removed: true},
	})
	if _, ok := verdict.(review.NoImpact); !ok {
		t.Errorf("verdict = %#v, want NoImpact", verdict)
	}
	if len(calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(calls))
	}
}

func TestStart_DocWithBrokenFrontmatterAtHeadIsStillTriaged(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	headSHA := commitDoc(t, repoDir, "docs/x.md", "# X\n\n## Mid\nno frontmatter anymore.\n")

	proposal := proposalFor("docs/x.md", 2)
	proposal["section"] = "Mid"
	verdict, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()},
		triageResponse(true), submitResponse(proposal), verifyResponse(true))
	if prompt := calls[0].Messages[0].Text; !strings.Contains(prompt, "no frontmatter anymore.") {
		t.Errorf("triage prompt = %q, want the raw head text of docs/x.md", prompt)
	}
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 1 {
		t.Fatalf("verdict = %#v, want one proposal", verdict)
	}
	if want := "## Mid\nno frontmatter anymore.\n"; proposals[0].Original != want {
		t.Errorf("Original = %q, want %q", proposals[0].Original, want)
	}
}

func TestStart_CandidateReplacedBySymlinkAtHeadFailsWithoutModelCalls(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	if err := os.Remove(filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("remove docs/x.md: %v", err)
	}
	if err := os.Symlink("../main.go", filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("symlink docs/x.md: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	_, calls, err := startBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()})
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseInternal {
		t.Fatalf("Start() error = %v, want *review.FailedError with CauseInternal", err)
	}
	if len(calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(calls))
	}
}

func TestStart_EditedCandidateIsQuotedAtHeadNotBase(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	baseSHA := commitDoc(t, repoDir, "docs/x.md", docWithCovers("\n  - main.go", "## Mid\nbase mid text\n"))
	commitDoc(t, repoDir, "docs/x.md", docWithCovers(" []", "## Mid\nhead mid text\n"))
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	proposal := proposalFor("docs/x.md", 2)
	proposal["section"] = "Mid"
	verdict, _ := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()},
		triageResponse(true), submitResponse(proposal), verifyResponse(true))
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 1 {
		t.Fatalf("Verdict = %#v, want one proposal", verdict)
	}
	if want := "## Mid\nhead mid text\n\n"; proposals[0].Original != want {
		t.Errorf("Original = %q, want head text %q", proposals[0].Original, want)
	}
}

func TestStart_HeadGitattributesDoNotRewriteBaseDocs(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	commitDoc(t, repoDir, ".gitattributes", "docs/** working-tree-encoding=UTF-16LE-BOM\n")
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	_, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()}, triageResponse(false))
	if len(calls) != 1 {
		t.Fatalf("model saw %d calls, want 1 triage call for docs/x.md", len(calls))
	}
}
