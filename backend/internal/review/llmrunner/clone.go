package llmrunner

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"time"
)

var fullSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// cloneHead clones headSHA from remoteURL at depth 1 into a new temp
// directory and checks it out detached, authenticating with token if it's
// non-empty. It returns the directory even on error once one was created, so
// the caller can always remove it.
func cloneHead(ctx context.Context, remoteURL, headSHA, token string) (string, error) {
	if !fullSHA.MatchString(headSHA) {
		return "", fmt.Errorf("clone %s: head sha %q is not a full hex object id", remoteURL, headSHA)
	}

	dir, err := os.MkdirTemp("", "docs-agent-clone-")
	if err != nil {
		return "", fmt.Errorf("clone %s: create temp dir: %w", remoteURL, err)
	}

	steps := [][]string{
		{"init"},
		{"fetch", "--depth=1", "--no-tags", remoteURL, headSHA},
		{"checkout", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if err := runGit(ctx, dir, token, args...); err != nil {
			return dir, fmt.Errorf("clone %s at %s: %w", remoteURL, headSHA, err)
		}
	}

	return dir, nil
}

func runGit(ctx context.Context, dir, token string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are fixed git subcommands plus a validated SHA and the runner's remote, not request text
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitAuthEnv(token)...)
	// git fetch forks git-remote-http, which inherits the output pipe; killing
	// only git leaves it holding the pipe open, so kill the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.CombinedOutput()
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		return fmt.Errorf("git %v: %w", args, ctxErr)
	}
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
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
