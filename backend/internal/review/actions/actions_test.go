package actions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
	"github.com/mrkizildag/docs-agent/backend/internal/review/actions"
)

type fakeAPI struct {
	dispatched actions.DispatchInputs
	runID      int64
	artifact   []byte
	err        error
	changed    []review.ChangedFile
	changedErr error
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

func TestStart(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 99}
	runner := actions.New(api, 10*time.Minute)

	before := time.Now()
	started, err := runner.Start(t.Context(), review.Request{InstallationID: 1, Owner: "o", Repo: "r", Number: 7, HeadSHA: "abc"})
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

			api := &fakeAPI{artifact: tc.raw, changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}}
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
