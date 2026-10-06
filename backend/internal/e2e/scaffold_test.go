package e2e_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gitfixture"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm/llmtest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

const (
	scaffoldIndex = "---\ntitle: Docs index\nsummary: Map of the docs.\ncovers: []\n---\n# Docs\n\n## Index\n\n" +
		"- [Architecture](architecture.md): how cmd/app is put together.\n- [Setup](guides/setup.md): build and test with make test.\n"
	scaffoldArchitecture = "---\ntitle: Architecture\nsummary: The parts of the app.\ncovers:\n  - \"cmd/**\"\n---\n# Architecture\n\ncmd/app is the entry point.\n"
	scaffoldSetup        = "---\ntitle: Setup\nsummary: Build and test.\ncovers:\n  - \"Makefile\"\n---\n# Setup\n\nRun `make test`.\n"

	scaffoldBranch = "pollux-agent/docs-scaffold"
	// scaffoldPRURL is where gatetest.GitHub puts the first pull request it opens.
	scaffoldPRURL = "https://gh/pull/9"
)

var errNotAnalyzed = errors.New("a PR without docs/ must not be analyzed")

// scaffoldGitHub is a GitHub whose default branch is at tip and which has no
// docs/ anywhere; its check runs are numbered from 101.
func scaffoldGitHub(tip string) *gatetest.GitHub {
	return &gatetest.GitHub{
		NoDocs:         true,
		DefaultTip:     tip,
		NextCheckRunID: 101,
		Fail:           map[string]error{"MergeBase": errNotAnalyzed, "ListChangedFiles": errNotAnalyzed},
	}
}

// failOnce makes the first call of method fail with err.
func failOnce(method string, err error) func(gatetest.Call) error {
	return func(c gatetest.Call) error {
		if c.Method == method && c.N == 1 {
			return err
		}
		return nil
	}
}

// useRepo points the server runner's clones of acme/widgets at a local
// repository with a Makefile and cmd/app, and returns its head SHA.
func useRepo(t *testing.T, extra map[string]string) string {
	t.Helper()

	files := map[string]string{"Makefile": "test:\n\tgo test ./...\n", "cmd/app/main.go": "package main\n\nfunc main() {}\n"}
	for name, content := range extra {
		files[name] = content
	}
	repoDir, tip := gitfixture.NewRepo(t, files)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+repoDir+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/acme/widgets.git")
	return tip
}

func scaffoldRunners(model llm.Model) gate.Runners {
	return gate.Runners{Server: llmrunner.New(model, noToken, "triage", "draft")}
}

func respond(resp llm.Response) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) { return resp, nil }
}

func providerError(llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("provider: 529 overloaded")
}

func submitDocs(t *testing.T, id, index string) func(llm.Request) (llm.Response, error) {
	t.Helper()

	args, err := json.Marshal(map[string]string{"index": index, "architecture": scaffoldArchitecture, "setup": scaffoldSetup})
	if err != nil {
		t.Fatalf("marshal submit_docs args: %v", err)
	}
	return respond(llm.Response{ToolCalls: []llm.ToolCall{{ID: id, Name: "submit_docs", Args: args}}})
}

func scriptedModel(steps ...func(llm.Request) (llm.Response, error)) *llmtest.ScriptedModel {
	return &llmtest.ScriptedModel{Script: steps}
}

// retryingScaffoldModel lists the repo, submits docs the index of which lacks
// its links, then submits valid ones.
func retryingScaffoldModel(t *testing.T) *llmtest.ScriptedModel {
	t.Helper()

	return scriptedModel(
		respond(llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: "list_dir", Args: json.RawMessage(`{"path":"."}`)}}}),
		submitDocs(t, "1", "---\ntitle: Docs\nsummary: S\ncovers: []\n---\n# Docs\n\n## Index\n"),
		submitDocs(t, "2", scaffoldIndex),
	)
}

func lastToolResult(req llm.Request) llm.ToolResult {
	last := req.Messages[len(req.Messages)-1]
	if len(last.ToolResults) == 0 {
		return llm.ToolResult{}
	}
	return last.ToolResults[0]
}

func scaffoldState(t *testing.T, store *sqlite.Store) gate.ScaffoldState {
	t.Helper()

	state, err := store.LoadScaffold(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("LoadScaffold() error = %v", err)
	}
	return state
}

func fileMap(files []gate.FileChange) map[string]string {
	m := map[string]string{}
	for _, f := range files {
		m[f.Path] = f.Content
	}
	return m
}

// waitLinked waits until check run id has been updated with a summary naming url.
func waitLinked(t *testing.T, gh *gatetest.GitHub, id int64, url string) gate.CheckRun {
	t.Helper()

	var linked gate.CheckRun
	waitFor(t, "check run to link the scaffold PR", func() bool {
		for _, cr := range gh.CheckRuns() {
			if cr.ID != id {
				continue
			}
			for _, u := range cr.Updates {
				if strings.Contains(u.Summary, url) {
					linked = u
					return true
				}
			}
		}
		return false
	})
	return linked
}

func TestWebhookToScaffoldPullRequest(t *testing.T) {
	tip := useRepo(t, map[string]string{
		"internal/doc.go":  "package internal\n",
		"README.md":        "# widgets\n",
		"assets/logo.txt":  "logo\n",
		"scripts/build.sh": "#!/bin/sh\n",
	})
	gh := scaffoldGitHub(tip)
	model := retryingScaffoldModel(t)
	sys := start(t, sqlitetest.Open(t), gh, scaffoldRunners(model))

	sys.deliverAs("d1", "pull_request", pullRequestBody(t, 1, tip, pushOpts{}))
	first := waitNth(t, "the first check run", gh.CheckRuns, 1)
	if c := first.Created; c.Conclusion != gate.ConclusionNeutral || c.Title != "No docs/ folder" || c.Name != "pollux-agent" {
		t.Errorf("first check run = %+v, want neutral \"No docs/ folder\" named pollux-agent", c)
	}
	linked := waitLinked(t, gh, first.ID, scaffoldPRURL)
	if first.ID != 101 || linked.Title != "No docs/ folder" || linked.Conclusion != gate.ConclusionNeutral {
		t.Errorf("updated check run %d = %+v, want check run 101 neutral \"No docs/ folder\" linking %s", first.ID, linked, scaffoldPRURL)
	}

	sys.deliverAs("d2", "pull_request", pullRequestBody(t, 2, tip, pushOpts{}))
	second := waitNth(t, "the second check run", gh.CheckRuns, 2).Created
	if !strings.Contains(second.Summary, scaffoldPRURL) || second.Conclusion != gate.ConclusionNeutral || second.Title != "No docs/ folder" {
		t.Errorf("second PR check run = %+v, want neutral \"No docs/ folder\" linking %s", second, scaffoldPRURL)
	}
	if err := sys.stop(); err != nil {
		t.Fatalf("worker.Run() error = %v", err)
	}

	if n := gh.CallCount("CreateBranch"); n != 1 {
		t.Errorf("CreateBranch calls = %d, want 1", n)
	}
	prs := gh.PullRequests()
	if len(prs) != 1 {
		t.Fatalf("pull requests created = %d, want 1", len(prs))
	}
	if want := (gate.NewPullRequest{Title: prs[0].Title, Body: prs[0].Body, Head: scaffoldBranch, Base: "main"}); prs[0] != want {
		t.Errorf("pull request = %+v, want head %s into main", prs[0], scaffoldBranch)
	}
	commits := gh.Committed()
	if len(commits) != 1 || commits[0].Branch != scaffoldBranch || commits[0].Parent != tip {
		t.Fatalf("commits = %+v, want one on %s from %s", commits, scaffoldBranch, tip)
	}
	wantFiles := map[string]string{"docs/README.md": scaffoldIndex, "docs/architecture.md": scaffoldArchitecture, "docs/guides/setup.md": scaffoldSetup}
	if diff := gocmp.Diff(wantFiles, fileMap(commits[0].Files)); diff != "" {
		t.Errorf("committed files (-want +got):\n%s", diff)
	}

	if len(model.Calls) != 3 {
		t.Fatalf("model saw %d requests, want 3 (list_dir, rejected submit_docs, accepted submit_docs)", len(model.Calls))
	}
	if !slices.ContainsFunc(model.Calls[0].Tools, func(tool llm.Tool) bool { return tool.Name == "submit_docs" }) {
		t.Errorf("first model request offers no submit_docs tool: %+v", model.Calls[0].Tools)
	}
	listing := lastToolResult(model.Calls[1])
	for _, want := range []string{"Makefile", "cmd/", "internal/"} {
		if !strings.Contains(listing.Content, want) {
			t.Errorf("list_dir result = %q, want it to contain %q", listing.Content, want)
		}
	}
	if rejected := lastToolResult(model.Calls[2]); !rejected.IsError || !strings.Contains(rejected.Content, "link") {
		t.Errorf("rejected submit_docs result = %+v, want an error about the index links", rejected)
	}
}

func TestWebhookAdoptsTheBotsScaffoldPullRequestAfterStateLoss(t *testing.T) {
	adoptBotScaffoldPullRequest(t, "bot-commit")
}

func TestWebhookAdoptsAClosedBotScaffoldPullRequestWithoutItsBranch(t *testing.T) {
	adoptBotScaffoldPullRequest(t, "")
}

// adoptBotScaffoldPullRequest runs a fresh store against a repo whose scaffold
// branch is at branchTip (absent when empty) with a bot pull request from it.
func adoptBotScaffoldPullRequest(t *testing.T, branchTip string) {
	t.Helper()

	tip := useRepo(t, nil)
	gh := scaffoldGitHub(tip)
	if branchTip != "" {
		gh.Branches = map[string]gate.Commit{scaffoldBranch: {SHA: branchTip}}
	}
	const adoptedURL = "https://github.com/acme/widgets/pull/7"
	gh.ExistingPR = &gate.ScaffoldPR{Number: 7, URL: adoptedURL, ByBot: true}
	sys := start(t, sqlitetest.Open(t), gh, scaffoldRunners(retryingScaffoldModel(t)))

	sys.deliverAs("d1", "pull_request", pullRequestBody(t, 1, tip, pushOpts{}))
	first := waitNth(t, "the check run", gh.CheckRuns, 1)
	waitLinked(t, gh, first.ID, adoptedURL)
	if err := sys.stop(); err != nil {
		t.Fatalf("worker.Run() error = %v", err)
	}

	if n := gh.CallCount("CreateBranch"); branchTip == "" && n != 0 {
		t.Errorf("branch creations = %d, want none", n)
	}
	if resets, prs, commits := gh.Resets(), gh.PullRequests(), gh.Committed(); len(resets) != 0 || len(prs) != 0 || len(commits) != 0 {
		t.Errorf("resets = %d, new pull requests = %d, commits = %d, want none", len(resets), len(prs), len(commits))
	}
}

// Two PRs at once, a redelivered webhook, and a restart open one scaffold PR.
func TestScaffoldOnceUnderConcurrencyRedeliveryAndRestart(t *testing.T) {
	tip := useRepo(t, nil)
	store := sqlitetest.Open(t)
	gh := scaffoldGitHub(tip)
	runners := scaffoldRunners(scriptedModel(submitDocs(t, "1", scaffoldIndex)))
	sys := start(t, store, gh, runners)

	deliveries := []struct {
		id   string
		body []byte
	}{
		{"d1", pullRequestBody(t, 1, tip, pushOpts{})},
		{"d2", pullRequestBody(t, 2, tip, pushOpts{})},
		{"d1", pullRequestBody(t, 1, tip, pushOpts{})},
	}
	done := make(chan struct{})
	for _, d := range deliveries {
		go func() {
			sys.postAs(d.id, "pull_request", d.body)
			done <- struct{}{}
		}()
	}
	for range deliveries {
		<-done
	}
	waitFor(t, "a scaffold PR", func() bool { return len(gh.PullRequests()) > 0 })
	if err := sys.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sys2 := start(t, store, gh, runners)
	sys2.deliverAs("d3", "pull_request", pullRequestBody(t, 3, tip, pushOpts{}))
	sys2.deliverAs("d1", "pull_request", pullRequestBody(t, 1, tip, pushOpts{}))
	waitFor(t, "PR 3's check run", func() bool { return len(gh.CheckRuns()) >= 3 })
	if err := sys2.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if prs, commits := gh.PullRequests(), gh.Committed(); len(prs) != 1 || len(commits) != 1 {
		t.Errorf("pull requests = %d, commits = %d, want 1 and 1", len(prs), len(commits))
	}
	runs := gh.CheckRuns()
	for _, cr := range runs {
		linked := strings.Contains(cr.Created.Summary, "/pull/9") || slices.ContainsFunc(cr.Updates, func(u gate.CheckRun) bool { return strings.Contains(u.Summary, "/pull/9") })
		if !linked {
			t.Errorf("check run %d never links the scaffold PR: %q", cr.ID, cr.Created.Summary)
		}
	}
	if len(runs) != 3 {
		t.Errorf("check runs created = %d, want 3 (PRs 1, 2, 3; redelivered d1 deduplicated)", len(runs))
	}
}

// A server LLM error opens no PR and the next PR event retries.
func TestScaffoldLLMErrorRetriesOnNextEvent(t *testing.T) {
	tip := useRepo(t, nil)
	store := sqlitetest.Open(t)
	gh := scaffoldGitHub(tip)
	sys := start(t, store, gh, scaffoldRunners(scriptedModel(providerError, submitDocs(t, "1", scaffoldIndex))))

	sys.deliverAs("d1", "pull_request", pullRequestBody(t, 1, tip, pushOpts{}))
	first := waitNth(t, "the first check run", gh.CheckRuns, 1).Created
	if first.Title != "No docs/ folder" || first.Conclusion != gate.ConclusionNeutral {
		t.Errorf("first check run = %+v", first)
	}
	waitFor(t, "the failed attempt", func() bool {
		s := scaffoldState(t, store)
		return s.Attempt == 1 && s.Phase == gate.ScaffoldIdle
	})
	if n := len(gh.PullRequests()); n != 0 {
		t.Errorf("pull requests after LLM error = %d, want 0", n)
	}

	sys.deliverAs("d2", "pull_request", pullRequestBody(t, 2, tip, pushOpts{}))
	waitNth(t, "the second check run", gh.CheckRuns, 2)
	waitFor(t, "the retried scaffold PR", func() bool { return len(gh.PullRequests()) == 1 })
}

// A GitHub error creating the PR opens none; the next event opens exactly one.
func TestScaffoldCreatePRErrorRetriesOnNextEvent(t *testing.T) {
	tip := useRepo(t, nil)
	store := sqlitetest.Open(t)
	gh := scaffoldGitHub(tip)
	gh.Before = failOnce("CreatePullRequest", errors.New("github: 502"))
	sys := start(t, store, gh, scaffoldRunners(scriptedModel(submitDocs(t, "1", scaffoldIndex), submitDocs(t, "2", scaffoldIndex))))

	sys.deliverAs("d1", "pull_request", pullRequestBody(t, 1, tip, pushOpts{}))
	waitNth(t, "the first check run", gh.CheckRuns, 1)
	waitFor(t, "the failed attempt", func() bool { return scaffoldState(t, store).Attempt == 1 })
	sys.deliverAs("d2", "pull_request", pullRequestBody(t, 2, tip, pushOpts{}))
	waitNth(t, "the second check run", gh.CheckRuns, 2)
	waitFor(t, "the scaffold PR", func() bool { return len(gh.PullRequests()) == 1 })
	if err := sys.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if n := len(gh.Committed()); n != 1 {
		t.Errorf("commits = %d, want 1", n)
	}
}

// A transient failure linking a waiting check run must not leave that check
// without the scaffold PR link forever.
func TestScaffoldLinkFailureIsHealedByNextEvent(t *testing.T) {
	tip := useRepo(t, nil)
	gh := scaffoldGitHub(tip)
	gh.Before = failOnce("UpdateCheckRun", errors.New("github: 502"))
	sys := start(t, sqlitetest.Open(t), gh, scaffoldRunners(scriptedModel(submitDocs(t, "1", scaffoldIndex))))

	sys.deliverAs("d1", "pull_request", pullRequestBody(t, 1, tip, pushOpts{}))
	first := waitNth(t, "the first check run", gh.CheckRuns, 1)
	waitFor(t, "the scaffold PR", func() bool { return len(gh.PullRequests()) == 1 })

	sys.deliverAs("d2", "pull_request", pullRequestBody(t, 2, tip, pushOpts{}))
	waitNth(t, "the second check run", gh.CheckRuns, 2)
	waitLinked(t, gh, first.ID, "/pull/9")
}

// Three failed attempts, each retried by the next PR event; after the third,
// the next PR's check says the scaffold could not be written and no further
// attempt runs the model.
func TestScaffoldGivesUpAfterThreeFailedAttempts(t *testing.T) {
	tip := useRepo(t, nil)
	store := sqlitetest.Open(t)
	gh := scaffoldGitHub(tip)
	model := scriptedModel(providerError, providerError, providerError)
	sys := start(t, store, gh, scaffoldRunners(model))

	for i := 1; i <= 3; i++ {
		sys.deliverAs(fmt.Sprintf("d%d", i), "pull_request", pullRequestBody(t, i, tip, pushOpts{}))
		run := waitNth(t, "the check run of the new PR", gh.CheckRuns, i)
		if c := run.Created; c.Title != "No docs/ folder" || c.Conclusion != gate.ConclusionNeutral {
			t.Errorf("PR %d check run = %+v, want neutral \"No docs/ folder\"", i, c)
		}
		waitFor(t, "failed attempt", func() bool { return scaffoldState(t, store).Failures == i })
		want := "tries again"
		if i == 3 {
			want = "after 3 attempts"
		}
		told := waitNth(t, "the waiter to be told", func() []gate.CheckRun { return gh.CheckRuns()[i-1].Updates }, 1)
		if told.Title != "No docs/ folder" || !strings.Contains(told.Summary, want) {
			t.Errorf("after failure %d, waiter update = %+v, want title \"No docs/ folder\" mentioning %q", i, told, want)
		}
	}
	for _, cr := range gh.CheckRuns() {
		for _, u := range cr.Updates {
			if u.Title != "No docs/ folder" || !strings.Contains(u.Summary, "tries again") && !strings.Contains(u.Summary, "after 3 attempts") {
				t.Errorf("check run %d update = %+v, want the scaffold failure told", cr.ID, u)
			}
		}
	}
	if s := scaffoldState(t, store); s.Phase != gate.ScaffoldGaveUp {
		t.Fatalf("state after 3 failures = %+v, want gave_up", s)
	}

	sys.deliverAs("d4", "pull_request", pullRequestBody(t, 4, tip, pushOpts{}))
	c := waitNth(t, "the check run of PR 4", gh.CheckRuns, 4).Created
	if c.Title != "No docs/ folder" || !strings.Contains(c.Summary, "could not write") {
		t.Errorf("PR 4 check run = %+v, want it to say the scaffold could not be written", c)
	}
	// No event marks an attempt that must not happen, so give one time to start.
	time.Sleep(500 * time.Millisecond)
	if err := sys.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if n := len(model.Calls); n != 3 {
		t.Errorf("model calls = %d after giving up, want 3 (no further attempt)", n)
	}
	if prs, commits := gh.PullRequests(), gh.Committed(); len(prs) != 0 || len(commits) != 0 {
		t.Errorf("pull requests = %d, commits = %d, want 0 and 0", len(prs), len(commits))
	}
}
