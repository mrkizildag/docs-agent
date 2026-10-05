package llmrunner

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing/fstest"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
)

const maxGitOutputLen = 500

var fullSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// clonePR fetches headSHA and baseSHA from remoteURL at depth 1 into a new
// temp directory and checks headSHA out detached, authenticating with token if
// it's non-empty. The base commit is only fetched, never checked out. It
// returns the directory even on error once one was created, so the caller can
// always remove it.
func clonePR(ctx context.Context, remoteURL, headSHA, baseSHA, token string) (string, error) {
	if !fullSHA.MatchString(headSHA) {
		return "", fmt.Errorf("clone %s: head sha %q is not a full hex object id", remoteURL, headSHA)
	}
	if !fullSHA.MatchString(baseSHA) {
		return "", fmt.Errorf("clone %s: base sha %q is not a full hex object id", remoteURL, baseSHA)
	}

	dir, err := os.MkdirTemp("", "pollux-agent-clone-")
	if err != nil {
		return "", fmt.Errorf("clone %s: create temp dir: %w", remoteURL, err)
	}

	steps := [][]string{
		{"init"},
		{"fetch", "--depth=1", "--no-tags", remoteURL, headSHA, baseSHA},
		{"checkout", "--detach", headSHA},
	}
	for _, args := range steps {
		if _, err := runGit(ctx, dir, remoteURL, token, args...); err != nil {
			return dir, fmt.Errorf("clone %s at %s (base %s): %w", remoteURL, headSHA, baseSHA, err)
		}
	}

	return dir, nil
}

// baseDocs reads docs/ at baseSHA, which clonePR fetched into dir, straight
// from git objects and returns it as an in-memory fs.FS rooted at the repo
// root. Nothing is checked out, so the PR's .gitattributes can't rewrite the
// base docs, and the agent's root over the clone can't reach them. Only regular
// .md files of at most docs.MaxDocBytes are included.
func baseDocs(ctx context.Context, dir, remoteURL, token, baseSHA string) (fs.FS, error) {
	listing, err := runGit(ctx, dir, remoteURL, token, "ls-tree", "-r", "-z", "--long", baseSHA, "--", "docs")
	if err != nil {
		return nil, fmt.Errorf("list docs at %s: %w", baseSHA, err)
	}

	var paths, shas []string
	for _, entry := range strings.Split(listing, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasSuffix(path, ".md") {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
			continue
		}
		if size, err := strconv.Atoi(fields[3]); err != nil || size > docs.MaxDocBytes {
			continue
		}
		paths = append(paths, path)
		shas = append(shas, fields[2])
	}

	files := fstest.MapFS{}
	if len(shas) == 0 {
		return files, nil
	}

	out, err := runGitStdin(ctx, dir, remoteURL, token, strings.Join(shas, "\n")+"\n", "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("read docs at %s: %w", baseSHA, err)
	}
	r := bufio.NewReader(strings.NewReader(out))
	for i, path := range paths {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read docs at %s: header of %s: %w", baseSHA, path, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != shas[i] || fields[1] != "blob" {
			return nil, fmt.Errorf("read docs at %s: unexpected cat-file header %q for %s", baseSHA, strings.TrimSpace(header), path)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size > docs.MaxDocBytes {
			return nil, fmt.Errorf("read docs at %s: bad size in cat-file header %q for %s", baseSHA, strings.TrimSpace(header), path)
		}
		content := make([]byte, size+1) // the blob plus its trailing newline
		if _, err := io.ReadFull(r, content); err != nil {
			return nil, fmt.Errorf("read docs at %s: content of %s: %w", baseSHA, path, err)
		}
		files[path] = &fstest.MapFile{Data: content[:size], Mode: 0o444}
	}
	return files, nil
}

// runGit runs git in dir and returns its stdout.
func runGit(ctx context.Context, dir, remoteURL, token string, args ...string) (string, error) {
	return runGitStdin(ctx, dir, remoteURL, token, "", args...)
}

// runGitStdin is runGit with stdin fed to git.
func runGitStdin(ctx context.Context, dir, remoteURL, token, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are fixed git subcommands plus validated SHAs and the runner's remote, not request text
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = gitEnv(dir, remoteURL, token)
	// git fetch forks git-remote-http, which inherits the output pipe; killing
	// only git leaves it holding the pipe open, so kill the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		return "", fmt.Errorf("git %v: %w", args, ctxErr)
	}
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, oneLine(stderr.String(), maxGitOutputLen))
	}
	return string(out), nil
}

// gitEnv is the whole environment git runs in: it handles attacker-controlled
// repository content, so it gets none of the server's secrets, no system or
// user config, and only https and local-path remotes (plus http when the
// remote itself is http). Without a token the caller's
// GIT_CONFIG_COUNT/KEY_n/VALUE_n pass through so tests can redirect the remote
// with url.<path>.insteadOf.
func gitEnv(home, remoteURL, token string) []string {
	protocols := "https:file"
	if strings.HasPrefix(remoteURL, "http://") {
		protocols += ":http"
	}
	env := []string{
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=" + protocols,
	}
	if token != "" {
		env = append(env, gitAuthEnv(token)...)
	}
	for _, kv := range os.Environ() { //nolint:forbidigo // the one place the git subprocess's environment is allowed through, field by field
		switch {
		case strings.HasPrefix(kv, "PATH="):
			env = append(env, kv)
		case token == "" && (strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") || strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_")):
			env = append(env, kv)
		}
	}
	return env
}

// gitAuthEnv carries the clone's bearer token as an HTTP header through git's
// config-from-env mechanism, never in argv, a URL, or an on-disk config file.
func gitAuthEnv(token string) []string {
	if token == "" {
		return nil
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}
