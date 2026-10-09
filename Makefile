.PHONY: check fmt lint test generated vuln run generate eval eval-check

# golangci-lint's default cache is per user, so worktrees of this repo share it and report each other's files.
export GOLANGCI_LINT_CACHE ?= $(CURDIR)/.cache/golangci-lint

check: lint test generated vuln

generate:
	cd backend && go run ./cmd/genaction ../action

fmt:
	cd backend && golangci-lint fmt ./...

lint:
	cd backend && golangci-lint run ./...

test:
	cd backend && go test -race ./...

# -count=1: the generated files live outside the Go module, so the test cache would not notice an edit.
generated:
	cd backend && go test -count=1 ./cmd/genaction

vuln:
	cd backend && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

run:
	cd backend && go run ./cmd/server

eval:
	cd backend && go test -tags eval -run '^TestEval$$' -count=1 -timeout 3h -v ./internal/review/llmrunner/

eval-check:
	cd backend && go test -tags eval -run '^(TestEvalCases|TestEvalActions|TestEvalPreviousReport|TestScore)' -count=1 -v ./internal/review/llmrunner/
