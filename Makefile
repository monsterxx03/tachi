.PHONY: build build-debug build-linux test test-cover test-cover-html lint lint-fix itest itest-run itest-acp

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Release build: -s -w strips symbol table + DWARF (~30% smaller binary),
# -trimpath keeps build reproducible. For Delve debugging use `make build-debug`.
LDFLAGS := -s -w -X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o tachi .

build-debug:
	go build -ldflags="-X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)" -o tachi .

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o tachi-linux-amd64 .

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o tachi-linux-arm64 .

test:
	go test ./...

test-cover:
	@echo "=== Running tests with coverage ==="
	@go test -coverprofile=coverage.out ./...
	@echo ""
	@echo "=== Per-package coverage ==="
	@go tool cover -func=coverage.out | sort -k 3 -r
	@echo ""
	@echo "=== Total coverage: $$(go tool cover -func=coverage.out | tail -1 | awk '{print $$NF}') ==="
	@rm -f coverage.out

test-cover-html:
	go test -coverprofile=coverage.out ./... && \
	go tool cover -html=coverage.out && \
	rm -f coverage.out

lint:
	@echo "=== Running golangci-lint ==="
	golangci-lint run ./...

lint-fix:
	@echo "=== Running go fix ==="
	go fix ./...
	@echo "=== Running golangci-lint (with --fix) ==="
	golangci-lint run --fix ./...

# Integration tests (docs/2026-07-31-tui-integration-test.md): isolated from
# unit tests via the integration build tag; `go test ./...` stays unchanged.
# M0 = -p pipe mode (real binary). mockllm's own unit + contract tests run
# with the regular `test` target (no build tag).
#
# Parallelism: every spec owns an isolated --home (t.TempDir()) + its own
# mockllm server (random port) + a real tachi subprocess, so specs are safe
# to run concurrently.
#   - `itest` runs the three packages in parallel (go test -p); each suite's
#     specs still run serially inside its package.
#   - `itest-run` / `itest-acp` additionally parallelize the suite's specs
#     via ginkgo -p (ITEST_PROCS processes, default 4; override with
#     `make ITEST_PROCS=8 itest-acp`).
GINKGO := go run github.com/onsi/ginkgo/v2/ginkgo
ITEST_PROCS ?= 4

itest:
	go test -tags=integration ./itest/...

itest-run:
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/run

itest-acp:
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/acp

