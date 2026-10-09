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
		{name: "base_sha of an object", key: "base_sha", input: `{"base_sha":"abc"}`, want: "abc\n"},
		{name: "base_sha absent", key: "base_sha", input: `{}`, want: "\n"},
		{name: "base_sha of a bare array", key: "base_sha", input: `["a.md"]`, want: "\n"},
		{name: "base_sha not a string", key: "base_sha", input: `{"base_sha":1}`, wantErr: true},
		{name: "base_sha not a string fails any key", key: "review", input: `{"base_sha":false}`, wantErr: true},
		{name: "files absent", key: "has_files", input: `{}`, want: "false\n"},
		{name: "files null", key: "has_files", input: `{"files":null}`, want: "false\n"},
		{name: "empty files", key: "has_files", input: `{"files":[]}`, want: "true\n"},
		{name: "hunks flatten files and ranges", key: "hunks", input: `{"files":[{"path":"a.go","ranges":[{"start":1,"end":2},{"start":5,"end":5}]},{"path":"old.go","ranges":[]}]}`,
			want: `{"file":"a.go","start":1,"end":2}` + "\n" + `{"file":"a.go","start":5,"end":5}` + "\n"},
		{name: "hunks of no files", key: "hunks", input: `{}`, want: ""},
		{name: "files not an array", key: "has_files", input: `{"files":{}}`, wantErr: true},
		{name: "file without path", key: "has_files", input: `{"files":[{"ranges":[]}]}`, wantErr: true},
		{name: "file path not a string", key: "has_files", input: `{"files":[{"path":1,"ranges":[]}]}`, wantErr: true},
		{name: "file without ranges", key: "has_files", input: `{"files":[{"path":"a.go"}]}`, wantErr: true},
		{name: "range start not a number", key: "has_files", input: `{"files":[{"path":"a.go","ranges":[{"start":"1","end":2}]}]}`, wantErr: true},
		{name: "range without end", key: "has_files", input: `{"files":[{"path":"a.go","ranges":[{"start":1}]}]}`, wantErr: true},
		{name: "bad review list fails the first key read", key: "has_files", input: `{"review":"x"}`, wantErr: true},
		{name: "bad uncovered list fails the first key read", key: "has_files", input: `{"uncovered":[1]}`, wantErr: true},
		{name: "quote and newline in a path stay on one line", key: "review", input: `["a\"b\nc.md"]`, want: `- "a\"b\nc.md"` + "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "bash", "-c", `jq -c -r --arg key "$KEY" -f "$FILTER"`)
			cmd.Env = []string{"PATH=" + filepath.Dir(jq) + ":/usr/bin:/bin", "KEY=" + tc.key, "FILTER=" + filter}
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

func TestTraceJQ(t *testing.T) {
	t.Parallel()

	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not on PATH")
	}
	filter, err := filepath.Abs("../../../../action/trace.jq")
	if err != nil {
		t.Fatalf("locate trace.jq: %v", err)
	}

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"::error::x"},{"type":"tool_use","name":"Read","input":{"file_path":"a/b.md"}},{"type":"tool_use","name":"Grep","input":{"pattern":"p\nq","path":"src"}},{"type":"tool_use","name":"Glob","input":{"pattern":"**/*.go"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"FILE BODY"}]}}`,
		`not json`,
		`{"type":"result","num_turns":4,"duration_ms":12345,"total_cost_usd":0.12,"usage":{"input_tokens":10,"output_tokens":20}}`,
	}, "\n") + "\n"
	want := "» Read a/b.md\n» Grep p q src\n» Glob **/*.go\n» done: turns=4 duration=12s cost=$0.12 tokens in=10 cache_read=0 cache_write=0 out=20\n"

	cmd := exec.CommandContext(t.Context(), "bash", "-c", `jq -rR -f "$FILTER"`)
	cmd.Env = []string{"PATH=" + filepath.Dir(jq) + ":/usr/bin:/bin", "FILTER=" + filter}
	cmd.Stdin = strings.NewReader(stream)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq trace.jq: %v", err)
	}
	if string(out) != want {
		t.Errorf("trace = %q, want %q", out, want)
	}
}
