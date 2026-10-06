package actions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
)

type fakeAPI struct {
	dispatched actions.DispatchInputs
	runID      int64
	artifact   []byte
	err        error
	changed    []review.ChangedFile
	changedErr error
	files      map[string][]byte
	fileErrs   map[string]error
	fileReads  map[string]int
	docsAt     map[string]fstest.MapFS
}

func (f *fakeAPI) DocsAtRef(_ context.Context, _ int64, _, _, ref string) (fs.FS, error) {
	return f.docsAt[ref], f.err
}

func (f *fakeAPI) Dispatch(_ context.Context, _ int64, _, _ string, in actions.DispatchInputs) (int64, error) {
	f.dispatched = in
	return f.runID, f.err
}

func (f *fakeAPI) ResultArtifact(_ context.Context, _ int64, _, _ string, _ int64) ([]byte, error) {
	return f.artifact, f.err
}

func (f *fakeAPI) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return f.changed, f.changedErr
}

func (f *fakeAPI) FileAtRef(_ context.Context, _ int64, _, _, path, ref string) ([]byte, bool, error) {
	if f.fileReads == nil {
		f.fileReads = map[string]int{}
	}
	f.fileReads[path+"@"+ref]++
	src, ok := f.files[path]
	return src, ok, f.fileErrs[path]
}

func TestStart(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 99}
	runner := actions.New(api, 10*time.Minute)

	before := time.Now()
	started, err := runner.Start(t.Context(), review.Request{InstallationID: 1, Owner: "o", Repo: "r", Number: 7, BaseSHA: "base", HeadSHA: "abc"})
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}

	pending, ok := started.(review.Pending)
	if !ok {
		t.Fatalf("Start() = %T, want review.Pending", started)
	}
	if pending.RunID != 99 || pending.Nonce == "" || pending.Nonce != api.dispatched.Nonce {
		t.Errorf("Start() = %+v, want RunID 99 and the dispatched nonce", pending)
	}
	if pending.Deadline.Before(before.Add(10 * time.Minute)) {
		t.Errorf("Start() deadline = %v, want at least 10m from %v", pending.Deadline, before)
	}
	if api.dispatched.HeadSHA != "abc" || api.dispatched.PRNumber != 7 {
		t.Errorf("Dispatch inputs = %+v, want head abc, PR 7", api.dispatched)
	}
}

func coverDoc(covers string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\ntitle: T\nsummary: S\ncovers: " + covers + "\n---\n# T\n")}
}

func startRequest(files ...review.ChangedFile) review.Request {
	return review.Request{InstallationID: 1, Owner: "o", Repo: "r", Number: 7, BaseSHA: "base", HeadSHA: "head", ChangedFiles: files}
}

func TestStartDispatchesBaseCandidates(t *testing.T) {
	t.Parallel()

	main := review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 2}}}

	t.Run("a doc whose head copy dropped its covers is still a candidate", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{
			"base": {"docs/a.md": coverDoc("[main.go]")},
			"head": {"docs/a.md": coverDoc("[]")},
		}}
		if _, err := actions.New(api, time.Minute).Start(t.Context(), startRequest(main)); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if diff := cmp.Diff([]string{"docs/a.md"}, api.dispatched.Docs); diff != "" {
			t.Errorf("dispatched Docs (-want +got):\n%s", diff)
		}
	})

	t.Run("a doc the PR adds is not a candidate", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{
			"base": {"docs/a.md": coverDoc("[main.go]")},
			"head": {"docs/a.md": coverDoc("[main.go]"), "docs/new.md": coverDoc("[main.go]")},
		}}
		if _, err := actions.New(api, time.Minute).Start(t.Context(), startRequest(main)); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if diff := cmp.Diff([]string{"docs/a.md"}, api.dispatched.Docs); diff != "" {
			t.Errorf("dispatched Docs (-want +got):\n%s", diff)
		}
	})

	t.Run("no covering doc still dispatches with an empty list", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[other.go]")}}}
		started, err := actions.New(api, time.Minute).Start(t.Context(), startRequest(main))
		if err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if _, ok := started.(review.Pending); !ok || len(api.dispatched.Docs) != 0 {
			t.Errorf("Start() = %T with Docs %v, want Pending with no docs", started, api.dispatched.Docs)
		}
	})

	t.Run("a renamed candidate is dispatched at its new path", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[main.go]")}}}
		req := startRequest(main, review.ChangedFile{Path: "docs/b.md", PreviousPath: "docs/a.md"})
		started, err := actions.New(api, time.Minute).Start(t.Context(), req)
		if err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if _, ok := started.(review.Pending); !ok {
			t.Fatalf("Start() = %T, want Pending", started)
		}
		if diff := cmp.Diff([]string{"docs/b.md"}, api.dispatched.Docs); diff != "" {
			t.Errorf("dispatched Docs (-want +got):\n%s", diff)
		}
	})

	t.Run("more candidates than the cap fail without dispatching", func(t *testing.T) {
		t.Parallel()
		base := fstest.MapFS{}
		for i := range basedocs.MaxCandidates + 1 {
			base[fmt.Sprintf("docs/d%02d.md", i)] = coverDoc("[main.go]")
		}
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": base}}
		_, err := actions.New(api, time.Minute).Start(t.Context(), startRequest(main))
		var failed *review.FailedError
		if !errors.As(err, &failed) || failed.Cause != review.CauseTooManyCandidates {
			t.Fatalf("Start() = %v, want FailedError with cause %q", err, review.CauseTooManyCandidates)
		}
		if api.dispatched.Nonce != "" {
			t.Errorf("Dispatch was called with %+v, want no dispatch", api.dispatched)
		}
	})
}

func TestStartRestoresDeletedCandidate(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[main.go]")}}}
	req := startRequest(
		review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 4, End: 6}}},
		review.ChangedFile{Path: "docs/a.md", Removed: true},
	)

	started, err := actions.New(api, time.Minute).Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	proposals, ok := result.Verdict.(review.Proposals)
	if !ok || len(proposals) != 1 || proposals[0].DocPath != "docs/a.md" {
		t.Fatalf("verdict = %#v, want one restore of docs/a.md", result.Verdict)
	}
	if result.Model != "" {
		t.Errorf("Model = %q, want empty", result.Model)
	}
	if api.dispatched.Nonce != "" {
		t.Errorf("Dispatch was called with %+v, want no dispatch", api.dispatched)
	}
}

func TestStartDocsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	if _, err := actions.New(&fakeAPI{err: wantErr}, time.Minute).Start(t.Context(), review.Request{}); !errors.Is(err, wantErr) {
		t.Fatalf("Start() = %v, want wrapping %v", err, wantErr)
	}
}

func TestStartDispatchError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	runner := actions.New(&fakeAPI{err: wantErr}, time.Minute)

	if _, err := runner.Start(t.Context(), review.Request{}); !errors.Is(err, wantErr) {
		t.Fatalf("Start() = %v, want wrapping %v", err, wantErr)
	}
}

func artifact(t *testing.T, head, nonce string, claude map[string]any) []byte {
	t.Helper()

	b, err := json.Marshal(map[string]any{"head_sha": head, "nonce": nonce, "claude": claude})
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	return b
}

func validProposal() map[string]any {
	return map[string]any{
		"doc_path": "docs/a.md", "section": "Usage", "anchor": map[string]any{"file": "main.go", "line": 3},
		"reason": "flag renamed", "content": "new text",
	}
}

func TestCollect(t *testing.T) {
	t.Parallel()

	const doc = "---\ntitle: A\nsummary: S.\ncovers:\n  - main.go\n---\n# A\n\n## Usage\nold usage\n"

	completion := review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", RunID: 99, Nonce: "n1"}
	proposal := validProposal()
	outside := map[string]any{
		"doc_path": "README.md", "section": "Usage", "anchor": map[string]any{"file": "main.go", "line": 3},
		"reason": "x", "content": "y",
	}

	farAnchor := map[string]any{
		"doc_path": "docs/a.md", "section": "Usage", "anchor": map[string]any{"file": "main.go", "line": 50},
		"reason": "x", "content": "y",
	}

	wrongType := validProposal()
	wrongType["anchor"] = map[string]any{"line": "three", "file": "main.go"}
	missingContent := validProposal()
	delete(missingContent, "content")

	tests := []struct {
		name        string
		raw         []byte
		want        review.Result
		wantInvalid bool
	}{
		{
			name: "no impact",
			raw: artifact(t, "abc", "n1", map[string]any{
				"modelUsage":        map[string]any{"claude-sonnet-4-5": map[string]any{}},
				"structured_output": map[string]any{"no_impact_reason": "internal refactor", "proposals": []any{}},
			}),
			want: review.Result{Runner: "actions", Model: "claude-sonnet-4-5", Verdict: review.NoImpact{Reason: "internal refactor"}},
		},
		{
			name: "proposals",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{proposal}},
			}),
			want: review.Result{Runner: "actions", Model: "claude-code", Verdict: review.Proposals{{
				DocPath: "docs/a.md", Section: "Usage", Anchor: review.Anchor{File: "main.go", Line: 3},
				Reason: "flag renamed", Content: "new text",
				Original: "## Usage\nold usage\n", Lines: review.LineRange{Start: 9, End: 10},
			}}},
		},
		{name: "head mismatch", raw: artifact(t, "other", "n1", map[string]any{"structured_output": map[string]any{}}), wantInvalid: true},
		{name: "nonce mismatch", raw: artifact(t, "abc", "stale", map[string]any{"structured_output": map[string]any{}}), wantInvalid: true},
		{name: "claude error", raw: artifact(t, "abc", "n1", map[string]any{"is_error": true, "result": "401"}), wantInvalid: true},
		{name: "no proposals and no reason", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"no_impact_reason": "", "proposals": []any{}}}), wantInvalid: true},
		{name: "no proposals and a blank reason", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"no_impact_reason": "  \n", "proposals": []any{}}}), wantInvalid: true},
		{name: "anchor line not an integer", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{wrongType}}}), wantInvalid: true},
		{name: "proposal missing content", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{missingContent}}}), wantInvalid: true},
		{name: "missing structured output", raw: artifact(t, "abc", "n1", map[string]any{}), wantInvalid: true},
		{name: "bad json", raw: []byte("{"), wantInvalid: true},
		{
			name: "anchor outside diff",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{farAnchor}},
			}),
			wantInvalid: true,
		},
		{
			name: "proposal outside docs",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{outside}},
			}),
			wantInvalid: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeAPI{
				artifact: tc.raw,
				changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
				files:    map[string][]byte{"docs/a.md": []byte(doc)},
			}
			runner := actions.New(api, time.Minute)
			got, err := runner.Collect(t.Context(), completion)

			var invalid *review.InvalidResultError
			if errors.As(err, &invalid) != tc.wantInvalid || (err != nil) != tc.wantInvalid {
				t.Fatalf("Collect() error = %v, want InvalidResultError = %v", err, tc.wantInvalid)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Collect() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCollectArtifactError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	runner := actions.New(&fakeAPI{err: wantErr}, time.Minute)

	_, err := runner.Collect(t.Context(), review.Completion{})
	var invalid *review.InvalidResultError
	if !errors.Is(err, wantErr) || errors.As(err, &invalid) {
		t.Fatalf("Collect() = %v, want transient error wrapping %v", err, wantErr)
	}
}

func TestCollectChangedFilesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	raw := artifact(t, "abc", "n1", map[string]any{
		"structured_output": map[string]any{"proposals": []any{validProposal()}},
	})
	runner := actions.New(&fakeAPI{artifact: raw, changedErr: wantErr}, time.Minute)

	_, err := runner.Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
	var invalid *review.InvalidResultError
	if !errors.Is(err, wantErr) || errors.As(err, &invalid) {
		t.Fatalf("Collect() = %v, want transient error wrapping %v", err, wantErr)
	}
}

func TestCollectErrorDoesNotEchoResult(t *testing.T) {
	t.Parallel()

	const injected = "SECRET-EXFIL"
	raw := artifact(t, "abc", "n1", map[string]any{
		"is_error": true, "result": injected, "subtype": "success",
		"terminal_reason": "api_error", "api_error_status": 401,
	})
	_, err := actions.New(&fakeAPI{artifact: raw}, time.Minute).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})

	var invalid *review.InvalidResultError
	if !errors.As(err, &invalid) {
		t.Fatalf("Collect() = %v, want *review.InvalidResultError", err)
	}
	if strings.Contains(err.Error(), injected) {
		t.Errorf("Collect() error %q contains the result text", err)
	}
	if want := "api_error_status 401 (terminal_reason api_error"; !strings.Contains(err.Error(), want) {
		t.Errorf("Collect() error %q, want it to contain %q", err, want)
	}
}

func TestCollectCapsProposalErrorText(t *testing.T) {
	t.Parallel()

	long := validProposal()
	long["doc_path"] = strings.Repeat("x", 5000)
	raw := artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{long}}})
	api := &fakeAPI{artifact: raw, changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}}

	_, err := actions.New(api, time.Minute).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
	if err == nil || len(err.Error()) > 400 {
		t.Fatalf("Collect() error = %v (len %d), want a non-nil error under 400 bytes", err, len(fmt.Sprint(err)))
	}
}

const sectionsDoc = "---\ntitle: A\nsummary: S.\ncovers:\n  - main.go\n---\n# A\n\n## Usage\nold usage\n\n## Other\nbody\n\n## Dup\nx\n\n## Dup\ny\n"

func collectProposals(t *testing.T, api *fakeAPI, proposals ...map[string]any) (review.Result, error) {
	t.Helper()

	items := make([]any, len(proposals))
	for i, p := range proposals {
		items[i] = p
	}
	api.artifact = artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": items}})
	api.changed = []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}
	res, err := actions.New(api, time.Minute).Collect(t.Context(), review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", Nonce: "n1"})
	if err != nil {
		return res, fmt.Errorf("collect: %w", err)
	}
	return res, nil
}

func TestCollectFillsOriginalAndLines(t *testing.T) {
	t.Parallel()

	other := validProposal()
	other["section"] = "## Other"

	api := &fakeAPI{files: map[string][]byte{"docs/a.md": []byte(sectionsDoc)}}
	got, err := collectProposals(t, api, validProposal(), other)
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}

	proposals, ok := got.Verdict.(review.Proposals)
	if !ok || len(proposals) != 2 {
		t.Fatalf("Verdict = %#v, want 2 proposals", got.Verdict)
	}
	if p := proposals[0]; p.Original != "## Usage\nold usage\n\n" || p.Lines != (review.LineRange{Start: 9, End: 11}) {
		t.Errorf("usage Original, Lines = %q, %+v", p.Original, p.Lines)
	}
	if p := proposals[1]; p.Section != "Other" || p.Original != "## Other\nbody\n\n" || p.Lines != (review.LineRange{Start: 12, End: 14}) {
		t.Errorf("other Section, Original, Lines = %q, %q, %+v", p.Section, p.Original, p.Lines)
	}
	if diff := cmp.Diff(map[string]int{"docs/a.md@abc": 1}, api.fileReads); diff != "" {
		t.Errorf("file reads (-want +got):\n%s", diff)
	}
}

func TestCollectRejectsSectionThatDoesNotResolveAtHead(t *testing.T) {
	t.Parallel()

	missingSection := validProposal()
	missingSection["section"] = "Nope"
	ambiguous := validProposal()
	ambiguous["section"] = "Dup"
	missingDoc := validProposal()
	missingDoc["doc_path"] = "docs/gone.md"
	brokenDoc := validProposal()
	brokenDoc["doc_path"] = "docs/broken.md"

	tests := []struct {
		name     string
		proposal map[string]any
		wantErr  string
	}{
		{name: "no such heading lists the headings", proposal: missingSection, wantErr: `headings are: "A", "Usage", "Other", "Dup", "Dup"`},
		{name: "duplicate heading", proposal: ambiguous, wantErr: "not exactly one"},
		{name: "doc missing at head", proposal: missingDoc, wantErr: "does not exist at head"},
		{name: "doc does not parse", proposal: brokenDoc, wantErr: "does not parse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeAPI{files: map[string][]byte{"docs/a.md": []byte(sectionsDoc), "docs/broken.md": []byte("no frontmatter")}}
			_, err := collectProposals(t, api, tc.proposal)

			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Collect() = %v, want *review.InvalidResultError containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCollectNormalizesSectionBeforeValidating(t *testing.T) {
	t.Parallel()

	hashOnly := validProposal()
	hashOnly["section"] = "#"
	hashOnly["index_entry"] = "a: Describes a."

	api := &fakeAPI{}
	got, err := collectProposals(t, api, hashOnly)
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}
	if proposals, ok := got.Verdict.(review.Proposals); !ok || len(proposals) != 1 || proposals[0].Section != "" {
		t.Fatalf("Verdict = %#v, want one new-doc proposal", got.Verdict)
	}
}

func TestCollectTooLargeDocLeavesOriginalEmpty(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{fileErrs: map[string]error{"docs/a.md": fmt.Errorf("read docs/a.md: %w", review.ErrFileTooLarge)}}
	got, err := collectProposals(t, api, validProposal())
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}
	if proposals, ok := got.Verdict.(review.Proposals); !ok || len(proposals) != 1 || proposals[0].Original != "" {
		t.Fatalf("Verdict = %#v, want one proposal with no Original", got.Verdict)
	}
}

func TestCollectReadErrorIsTransient(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	api := &fakeAPI{fileErrs: map[string]error{"docs/a.md": wantErr}}
	_, err := collectProposals(t, api, validProposal())

	var invalid *review.InvalidResultError
	if !errors.Is(err, wantErr) || errors.As(err, &invalid) {
		t.Fatalf("Collect() = %v, want transient error wrapping %v", err, wantErr)
	}
}

const scaffoldFrontmatter = "---\ntitle: T\nsummary: S\ncovers: []\n---\n"

func validScaffoldOutput() map[string]any {
	return map[string]any{
		"index":        scaffoldFrontmatter + "## Index\n[a](architecture.md) [s](guides/setup.md)\n",
		"architecture": scaffoldFrontmatter,
		"setup":        scaffoldFrontmatter,
	}
}

func TestStartScaffold(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 42}
	runner := actions.New(api, 10*time.Minute)

	started, err := runner.StartScaffold(t.Context(), review.ScaffoldRequest{InstallationID: 1, Owner: "o", Repo: "r", BaseSHA: "base"})
	if err != nil {
		t.Fatalf("StartScaffold() = %v, want nil", err)
	}
	pending, ok := started.(review.Pending)
	if !ok {
		t.Fatalf("StartScaffold() = %T, want review.Pending", started)
	}
	if pending.RunID != 42 || pending.Nonce == "" || !pending.Deadline.After(time.Now()) {
		t.Errorf("Pending = %+v, want run 42, a nonce and a future deadline", pending)
	}
	want := actions.DispatchInputs{HeadSHA: "base", PRNumber: 0, Nonce: pending.Nonce}
	if diff := cmp.Diff(want, api.dispatched); diff != "" {
		t.Errorf("dispatch inputs (-want +got):\n%s", diff)
	}
}

func TestCollectScaffold(t *testing.T) {
	t.Parallel()

	completion := review.Completion{Owner: "o", Repo: "r", HeadSHA: "base", RunID: 42, Nonce: "n1"}
	badIndex := validScaffoldOutput()
	badIndex["index"] = scaffoldFrontmatter

	tests := []struct {
		name        string
		art         []byte
		wantInvalid string
	}{
		{name: "valid", art: artifact(t, "base", "n1", map[string]any{"structured_output": validScaffoldOutput(), "modelUsage": map[string]any{"m1": map[string]any{}}})},
		{name: "mismatched nonce", art: artifact(t, "base", "other", map[string]any{"structured_output": validScaffoldOutput()}), wantInvalid: "nonce"},
		{name: "mismatched head", art: artifact(t, "other", "n1", map[string]any{"structured_output": validScaffoldOutput()}), wantInvalid: "head_sha"},
		{name: "invalid docs", art: artifact(t, "base", "n1", map[string]any{"structured_output": badIndex}), wantInvalid: "docs/README.md"},
		{name: "is_error", art: artifact(t, "base", "n1", map[string]any{"is_error": true, "structured_output": validScaffoldOutput()}), wantInvalid: "claude code failed"},
		{name: "missing output", art: artifact(t, "base", "n1", map[string]any{}), wantInvalid: "no structured_output"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := actions.New(&fakeAPI{artifact: tc.art}, time.Minute)
			got, err := runner.CollectScaffold(t.Context(), completion)
			if tc.wantInvalid == "" {
				if err != nil {
					t.Fatalf("CollectScaffold() = %v, want nil", err)
				}
				want := review.Scaffold{
					Runner:       "actions",
					Model:        "m1",
					Index:        scaffoldFrontmatter + "## Index\n[a](architecture.md) [s](guides/setup.md)\n",
					Architecture: scaffoldFrontmatter,
					Setup:        scaffoldFrontmatter,
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("Scaffold (-want +got):\n%s", diff)
				}
				return
			}

			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.wantInvalid) {
				t.Fatalf("CollectScaffold() = %v, want *InvalidResultError containing %q", err, tc.wantInvalid)
			}
		})
	}
}
