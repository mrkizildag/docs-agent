package actions_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocsJQ(t *testing.T) {
	t.Parallel()

	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not on PATH")
	}
	filter, err := filepath.Abs("../../../../action/docs.jq")
	if err != nil {
		t.Fatalf("locate docs.jq: %v", err)
	}

	tests := []struct {
		name    string
		key     string
		input   string
		want    string
		wantErr bool
	}{
		{name: "bare array is the review list", key: "review", input: `["a.md","b.md"]`, want: "- \"a.md\"\n- \"b.md\"\n"},
		{name: "bare array has no uncovered", key: "uncovered", input: `["a.md"]`, want: "(none)\n"},
		{name: "object review list", key: "review", input: `{"review":["a.md"],"uncovered":["x.go"]}`, want: "- \"a.md\"\n"},
		{name: "object uncovered list", key: "uncovered", input: `{"review":["a.md"],"uncovered":["x.go"]}`, want: "- \"x.go\"\n"},
		{name: "empty object", key: "review", input: `{}`, want: "(none)\n"},
		{name: "review not a list", key: "review", input: `{"review":"x"}`, wantErr: true},
		{name: "string input", key: "review", input: `"s"`, wantErr: true},
		{name: "non-string element", key: "uncovered", input: `{"uncovered":[1]}`, wantErr: true},
		{name: "quote and newline in a path stay on one line", key: "review", input: `["a\"b\nc.md"]`, want: `- "a\"b\nc.md"` + "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), jq, "-r", "--arg", "key", tc.key, "-f", filter) //nolint:gosec // jq from PATH and table-literal args
			cmd.Stdin = strings.NewReader(tc.input)
			out, err := cmd.Output()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("jq %s on %s = %q, want non-zero exit", tc.key, tc.input, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("jq %s on %s: %v", tc.key, tc.input, err)
			}
			if string(out) != tc.want {
				t.Errorf("jq %s on %s = %q, want %q", tc.key, tc.input, out, tc.want)
			}
		})
	}
}
