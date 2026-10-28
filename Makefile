.PHONY: setup fmt lint vet test race build check ci snapshot run

setup:
	go mod download

fmt:
	gofmt -w .

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; skipping"; fi

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

build:
	go build ./...

check: vet test build

ci:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race ./...
	go build ./...

run:
	go run ./cmd/throtd

snapshot:
	goreleaser release --snapshot --clean
