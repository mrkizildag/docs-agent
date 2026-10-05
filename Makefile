.PHONY: check fmt lint test vuln run generate

# golangci-lint's default cache is per user, so worktrees of this repo share it and report each other's files.
export GOLANGCI_LINT_CACHE ?= $(CURDIR)/.cache/golangci-lint

check: lint test vuln

generate:
	cd backend && go run ./cmd/genschema ../action/proposal.schema.json ../action/result.schema.json

fmt:
	cd backend && golangci-lint fmt ./...

lint:
	cd backend && golangci-lint run ./...

test:
	cd backend && go test -race ./...

vuln:
	cd backend && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

run:
	cd backend && go run ./cmd/server
