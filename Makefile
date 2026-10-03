.PHONY: check fmt lint test vuln run generate

check: lint test vuln

generate:
	cd backend && go run ./cmd/genschema ../action/proposal.schema.json

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
