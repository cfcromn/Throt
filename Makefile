.PHONY: setup fmt lint vet test race build check ci snapshot run bench

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

# bench runs the real-kernel benchmark in a container capped at 2 CPUs. The
# test binary is built first and exec'd, so no "go" process (itself a build
# workload) sits above it and takes a heavy slot.
bench:
	docker run --rm --cpus=2 -v "$$PWD":/src -w /src golang:1.26.10-alpine sh -c \
		'go test -c -o /tmp/bench.test ./internal/bench && THROT_BENCH=1 exec /tmp/bench.test -test.run Bench -test.v -test.count=1 -test.timeout 30m'

run:
	go run ./cmd/throtd

snapshot:
	goreleaser release --snapshot --clean
