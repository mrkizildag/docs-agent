//go:build eval

package llmrunner_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.yaml.in/yaml/v3"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const (
	verdictProposals = "proposals"
	verdictNoImpact  = "no_impact"
	// verdictEither is a borderline case: "no impact" passes, and so do
	// proposals that only touch the listed docs and sections.
	verdictEither = "either"
)

// evalCommitEnv is the fixed identity for every commit the harness makes.
func evalCommitEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=eval", "GIT_AUTHOR_EMAIL=eval@example.com",
		"GIT_COMMITTER_NAME=eval", "GIT_COMMITTER_EMAIL=eval@example.com",
	}
}

var fullHexSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

type evalDoc struct {
	Path     string   `yaml:"path"`
	New      bool     `yaml:"new"`
	Sections []string `yaml:"sections"`
	MustSay  []string `yaml:"must_say"`
}

type evalExpect struct {
	Verdict string    `yaml:"verdict"`
	Docs    []evalDoc `yaml:"docs"`
	Allow   []string  `yaml:"allow"`
}

type evalCase struct {
	ID     string     `yaml:"id"`
	Source string     `yaml:"source"`
	Base   string     `yaml:"base"`
	Head   string     `yaml:"head"`
	Expect evalExpect `yaml:"expect"`
	Notes  string     `yaml:"notes"`
}

// parseEvalCase decodes and validates one case file; name is the file name.
func parseEvalCase(name string, data []byte) (evalCase, error) {
	var c evalCase
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return evalCase{}, fmt.Errorf("decode case %s: %w", name, err)
	}

	if stem := strings.TrimSuffix(name, filepath.Ext(name)); c.ID != stem {
		return evalCase{}, fmt.Errorf("case %s: id %q must equal the file name stem %q", name, c.ID, stem)
	}
	for field, sha := range map[string]string{"base": c.Base, "head": c.Head} {
		if !fullHexSHA.MatchString(sha) {
			return evalCase{}, fmt.Errorf("case %s: %s %q is not a full 40-hex sha", name, field, sha)
		}
	}

	switch c.Expect.Verdict {
	case verdictNoImpact:
		if len(c.Expect.Docs) > 0 {
			return evalCase{}, fmt.Errorf("case %s: a no_impact case lists no docs", name)
		}
	case verdictProposals:
		if len(c.Expect.Docs) == 0 {
			return evalCase{}, fmt.Errorf("case %s: a proposals case lists at least one doc", name)
		}
	case verdictEither:
		if len(c.Expect.Docs) == 0 {
			return evalCase{}, fmt.Errorf("case %s: an either case lists at least one doc", name)
		}
		// An either case is scored on targets only: facts and allow would be silently ignored.
		if len(c.Expect.Allow) > 0 || slices.ContainsFunc(c.Expect.Docs, func(d evalDoc) bool { return len(d.MustSay) > 0 || d.New }) {
			return evalCase{}, fmt.Errorf("case %s: an either case lists docs and sections only (no allow, must_say, or new)", name)
		}
	default:
		return evalCase{}, fmt.Errorf("case %s: verdict %q must be %s, %s, or %s", name, c.Expect.Verdict, verdictProposals, verdictNoImpact, verdictEither)
	}

	globs := slices.Clone(c.Expect.Allow)
	for _, d := range c.Expect.Docs {
		globs = append(globs, d.Path)
	}
	for _, g := range globs {
		if _, err := path.Match(g, ""); err != nil {
			return evalCase{}, fmt.Errorf("case %s: glob %q: %w", name, g, err)
		}
	}
	return c, nil
}

// loadEvalCases reads every case in dir, or only those in ids when it is non-empty.
func loadEvalCases(dir string, ids []string) ([]evalCase, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list cases in %s: %w", dir, err)
	}

	var cases []evalCase
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // f comes from globbing the cases directory
		if err != nil {
			return nil, fmt.Errorf("read case %s: %w", f, err)
		}
		c, err := parseEvalCase(filepath.Base(f), data)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 || slices.Contains(ids, c.ID) {
			cases = append(cases, c)
		}
	}
	for _, id := range ids {
		if !slices.ContainsFunc(cases, func(c evalCase) bool { return c.ID == id }) {
			return nil, fmt.Errorf("EVAL_CASE: no case %q in %s", id, dir)
		}
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no cases in %s", dir)
	}
	return cases, nil
}

func runEvalGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are literals or shas validated as 40-hex on case load
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func evalRepoRoot(ctx context.Context) (string, error) {
	out, err := runEvalGit(ctx, ".", nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// evalInput is a case ready to run: a repository the runner clones from and
// the request describing the change in it.
type evalInput struct {
	// Dir is the repository's path on disk; Remote is its URL.
	Dir     string
	Remote  string
	Request review.Request
}

// buildEvalInput fetches the case's commits from the repository at root into a
// temp repo in dir and builds a synthetic head: head's tree with docs/ reset
// to base's, so the runner must propose the docs the PR changed. The commit
// date and identity are fixed so its SHA is the same on every run.
func buildEvalInput(ctx context.Context, root, dir string, c evalCase) (evalInput, error) {
	git := func(env []string, args ...string) (string, error) { return runEvalGit(ctx, dir, env, args...) }

	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "uploadpack.allowAnySHA1InWant", "true"},
		{"-c", "uploadpack.allowAnySHA1InWant=true", "fetch", "-q", "--no-tags", root, c.Base, c.Head},
	} {
		if _, err := git(nil, args...); err != nil {
			return evalInput{}, fmt.Errorf("case %s: %w", c.ID, err)
		}
	}

	indexEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "eval-index")}
	steps := [][]string{
		{"read-tree", c.Head},
		{"rm", "--cached", "-r", "-q", "--ignore-unmatch", "docs"},
	}
	if _, err := git(nil, "cat-file", "-e", c.Base+":docs"); err == nil {
		steps = append(steps, []string{"read-tree", "--prefix=docs/", c.Base + ":docs"})
	}
	for _, args := range steps {
		if _, err := git(indexEnv, args...); err != nil {
			return evalInput{}, fmt.Errorf("case %s: build synthetic head: %w", c.ID, err)
		}
	}
	tree, err := git(indexEnv, "write-tree")
	if err != nil {
		return evalInput{}, fmt.Errorf("case %s: build synthetic head: %w", c.ID, err)
	}

	const stamp = "2000-01-01T00:00:00Z"
	commitEnv := append(evalCommitEnv(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	out, err := git(commitEnv, "-c", "commit.gpgsign=false", "commit-tree", "-p", c.Base, "-m", "eval head", strings.TrimSpace(tree))
	if err != nil {
		return evalInput{}, fmt.Errorf("case %s: build synthetic head: %w", c.ID, err)
	}
	head := strings.TrimSpace(out)
	if _, err := git(nil, "update-ref", "refs/eval/head", head); err != nil {
		return evalInput{}, fmt.Errorf("case %s: %w", c.ID, err)
	}

	files, err := evalChangedFiles(ctx, dir, c.Base, head)
	if err != nil {
		return evalInput{}, fmt.Errorf("case %s: %w", c.ID, err)
	}
	return evalInput{
		Dir:    dir,
		Remote: "file://" + dir,
		Request: review.Request{
			InstallationID: 1, Owner: "eval", Repo: c.ID, Number: 1,
			BaseSHA: c.Base, HeadSHA: head, ChangedFiles: files,
		},
	}, nil
}

// evalChangedFiles lists the files between base and head the way GitHub's PR
// files API does. numstat and the patch share git's path ordering, so they
// are paired by position.
func evalChangedFiles(ctx context.Context, dir, base, head string) ([]review.ChangedFile, error) {
	numstat, err := runEvalGit(ctx, dir, nil, "diff", "-M", "--numstat", "-z", base, head)
	if err != nil {
		return nil, err
	}
	diff, err := runEvalGit(ctx, dir, nil, "diff", "-M", "-U3", "--no-color", "--no-ext-diff", base, head)
	if err != nil {
		return nil, err
	}

	stats, err := parseNumstat(numstat)
	if err != nil {
		return nil, err
	}
	patches := splitPatches(diff)
	if len(stats) != len(patches) {
		return nil, fmt.Errorf("diff %s..%s: numstat lists %d files, patch has %d", base, head, len(stats), len(patches))
	}

	files := make([]review.ChangedFile, len(stats))
	for i, s := range stats {
		hunks, err := parseHunks(patches[i].text)
		if err != nil {
			return nil, fmt.Errorf("parse patch of %s: %w", s.path, err)
		}
		files[i] = review.ChangedFile{Path: s.path, PreviousPath: s.previous, Removed: patches[i].removed, Hunks: hunks, Patch: patches[i].text, Changes: s.changes}
	}
	return files, nil
}

type numstatEntry struct {
	path     string
	previous string
	changes  int
}

func parseNumstat(out string) ([]numstatEntry, error) {
	toks := strings.Split(out, "\x00")
	var entries []numstatEntry
	for i := 0; i < len(toks); i++ {
		if toks[i] == "" {
			continue
		}
		parts := strings.SplitN(toks[i], "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed numstat entry %q", toks[i])
		}
		var e numstatEntry
		if parts[0] != "-" {
			added, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, fmt.Errorf("numstat entry %q: %w", toks[i], err)
			}
			deleted, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("numstat entry %q: %w", toks[i], err)
			}
			e.changes = added + deleted
		}
		if parts[2] != "" {
			e.path = parts[2]
		} else {
			if i+2 >= len(toks) {
				return nil, fmt.Errorf("numstat rename entry %q lacks its paths", toks[i])
			}
			e.previous, e.path = toks[i+1], toks[i+2]
			i += 2
		}
		entries = append(entries, e)
	}
	return entries, nil
}

type filePatch struct {
	text    string
	removed bool
}

// splitPatches cuts a unified diff into one filePatch per file, keeping the
// text from the first hunk header on.
func splitPatches(diff string) []filePatch {
	var patches []filePatch
	var body []string
	inHunks := false
	flush := func() {
		if len(patches) > 0 {
			patches[len(patches)-1].text = strings.Join(body, "\n")
		}
		body, inHunks = nil, false
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			patches = append(patches, filePatch{})
		case !inHunks && strings.HasPrefix(line, "deleted file mode "):
			patches[len(patches)-1].removed = true
		case !inHunks && strings.HasPrefix(line, "@@"):
			inHunks = true
			body = append(body, line)
		case inHunks:
			body = append(body, line)
		}
	}
	flush()
	return patches
}

// parseHunks returns the head-side line range of each hunk header in patch,
// following internal/github's rule: an omitted count is 1 and a count of 0
// covers no lines.
func parseHunks(patch string) ([]review.LineRange, error) {
	var hunks []review.LineRange
	for line := range strings.SplitSeq(patch, "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		m := hunkHeader.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("malformed hunk header %q", line)
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("hunk header %q: %w", line, err)
		}
		count := 1
		if m[2] != "" {
			if count, err = strconv.Atoi(m[2]); err != nil {
				return nil, fmt.Errorf("hunk header %q: %w", line, err)
			}
		}
		if count == 0 {
			continue
		}
		hunks = append(hunks, review.LineRange{Start: start, End: start + count - 1})
	}
	return hunks, nil
}

func TestEvalCases(t *testing.T) {
	ctx := t.Context()
	root, err := evalRepoRoot(ctx)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	cases, err := loadEvalCases(filepath.Join(root, "eval", "cases"), nil)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}

	const maxFiles = 50
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			in, err := buildEvalInput(ctx, root, t.TempDir(), c)
			if err != nil {
				t.Fatalf("buildEvalInput(%s) error = %v", c.ID, err)
			}
			patchBytes := 0
			for _, f := range in.Request.ChangedFiles {
				patchBytes += len(f.Patch)
			}
			t.Logf("%s: %d changed files, %d patch bytes", c.ID, len(in.Request.ChangedFiles), patchBytes)
			if n := len(in.Request.ChangedFiles); n > maxFiles {
				t.Errorf("%s: %d non-docs changed files, want at most %d", c.ID, n, maxFiles)
			}
		})
	}
}

func TestEvalCasesParse(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("a", 40)
	header := "id: x\nbase: " + sha + "\nhead: " + sha + "\n"
	tests := []struct {
		name    string
		file    string
		body    string
		wantErr bool
	}{
		{name: "proposals", file: "x.yaml", body: header + "expect:\n  verdict: proposals\n  docs:\n    - path: docs/*.md\n"},
		{name: "no impact", file: "x.yaml", body: header + "expect:\n  verdict: no_impact\n"},
		{name: "id differs from file", file: "y.yaml", body: header + "expect:\n  verdict: no_impact\n", wantErr: true},
		{name: "short sha", file: "x.yaml", body: "id: x\nbase: abc\nhead: " + sha + "\nexpect:\n  verdict: no_impact\n", wantErr: true},
		{name: "unknown verdict", file: "x.yaml", body: header + "expect:\n  verdict: maybe\n", wantErr: true},
		{name: "no impact with docs", file: "x.yaml", body: header + "expect:\n  verdict: no_impact\n  docs:\n    - path: docs/a.md\n", wantErr: true},
		{name: "proposals without docs", file: "x.yaml", body: header + "expect:\n  verdict: proposals\n", wantErr: true},
		{name: "either", file: "x.yaml", body: header + "expect:\n  verdict: either\n  docs:\n    - path: docs/a.md\n"},
		{name: "either without docs", file: "x.yaml", body: header + "expect:\n  verdict: either\n", wantErr: true},
		{name: "either with facts", file: "x.yaml", body: header + "expect:\n  verdict: either\n  docs:\n    - path: docs/a.md\n      must_say: [\"x\"]\n", wantErr: true},
		{name: "either with allow", file: "x.yaml", body: header + "expect:\n  verdict: either\n  docs:\n    - path: docs/a.md\n  allow: [docs/b.md]\n", wantErr: true},
		{name: "bad glob", file: "x.yaml", body: header + "expect:\n  verdict: proposals\n  docs:\n    - path: \"docs/[\"\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseEvalCase(tc.file, []byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Errorf("parseEvalCase() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestEvalCasesPatches(t *testing.T) {
	t.Parallel()

	diff := "diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n x\n+y\n z\n" +
		"diff --git a/b.go b/b.go\ndeleted file mode 100644\nindex 1..0\n--- a/b.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-q\n" +
		"diff --git a/c.bin b/c.bin\nBinary files a/c.bin and b/c.bin differ\n"
	got := splitPatches(diff)
	want := []filePatch{
		{text: "@@ -1,2 +1,3 @@\n x\n+y\n z"},
		{text: "@@ -1 +0,0 @@\n-q", removed: true},
		{},
	}
	if d := cmp.Diff(want, got, cmp.AllowUnexported(filePatch{})); d != "" {
		t.Errorf("splitPatches() mismatch (-want +got):\n%s", d)
	}

	hunks, err := parseHunks(got[0].text + "\n@@ -9 +10,0 @@\n@@ -20 +30 @@")
	if err != nil {
		t.Fatalf("parseHunks() error = %v", err)
	}
	wantHunks := []review.LineRange{{Start: 1, End: 3}, {Start: 30, End: 30}}
	if d := cmp.Diff(wantHunks, hunks); d != "" {
		t.Errorf("parseHunks() mismatch (-want +got):\n%s", d)
	}

	entries, err := parseNumstat("3\t1\ta.go\x00-\t-\t\x00old.bin\x00new.bin\x00")
	if err != nil {
		t.Fatalf("parseNumstat() error = %v", err)
	}
	wantEntries := []numstatEntry{{path: "a.go", changes: 4}, {path: "new.bin", previous: "old.bin"}}
	if d := cmp.Diff(wantEntries, entries, cmp.AllowUnexported(numstatEntry{})); d != "" {
		t.Errorf("parseNumstat() mismatch (-want +got):\n%s", d)
	}
}
