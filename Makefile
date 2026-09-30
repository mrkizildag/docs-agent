.PHONY: check fmt lint test vuln run

check: lint test vuln

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
