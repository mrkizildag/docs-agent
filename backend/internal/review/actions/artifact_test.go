package actions_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
)

type zipAPI struct {
	fakeAPI
	archive []byte
}

func (z *zipAPI) RunArtifact(context.Context, int64, string, string, int64, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(z.archive)), nil
}

func zipOf(t *testing.T, name string, content []byte) []byte {
	t.Helper()

	archive, err := zipBytes(name, content)
	if err != nil {
		t.Fatalf("zip %s: %v", name, err)
	}
	return archive
}

// zipBytes is a zip holding one file.
func zipBytes(name string, content []byte) ([]byte, error) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	f, err := zw.Create(name)
	if err != nil {
		return nil, fmt.Errorf("create zip entry: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		return nil, fmt.Errorf("write zip entry: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}
	return archive.Bytes(), nil
}

func TestCollectArtifactZip(t *testing.T) {
	t.Parallel()

	const capBytes = 10 << 20
	valid := artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}}})

	tests := []struct {
		name        string
		archive     []byte
		wantErr     string
		wantInvalid bool
	}{
		{name: "result.json in the zip", archive: zipOf(t, actions.ResultFileName, valid)},
		{name: "zip over the cap", archive: make([]byte, capBytes+1), wantErr: "download artifact: larger than"},
		{name: "not a zip", archive: []byte("nope"), wantErr: "open artifact zip"},
		{name: "result.json over the cap", archive: zipOf(t, actions.ResultFileName, make([]byte, capBytes+1)), wantErr: "read result.json in artifact: larger than"},
		{name: "zip without result.json", archive: zipOf(t, "other.json", []byte("{}")), wantErr: "open result.json in artifact"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := actions.New(&zipAPI{archive: tc.archive}, time.Minute)
			_, err := runner.Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})

			var invalid *review.InvalidResultError
			if errors.As(err, &invalid) {
				t.Fatalf("Collect() = %v, want a transient error, not an invalid result", err)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Collect() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Collect() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
