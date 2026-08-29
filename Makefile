.PHONY: build build-debug build-linux test test-cover test-cover-html lint lint-fix web-build web-check itest itest-mockllm itest-run itest-tui itest-acp

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Release build: -s -w strips symbol table + DWARF (~30% smaller binary),
# -trimpath keeps build reproducible. For Delve debugging use `make build-debug`.
LDFLAGS := -s -w -X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)
NPM := npm --prefix web/frontend


build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o tachi .

# Build the frontend into web/frontend/dist so it's embedded into the Go binary.
web-build:
	$(NPM) install && $(NPM) run build

# The module embeds web/frontend/dist (web/frontend/embed.go), so any Go
# compile needs it to exist first. Check instead of unconditionally building:
# dist is only missing on a fresh checkout, and npm build is expensive.
web-check:
	@if [ ! -f web/frontend/dist/index.html ]; then \
		echo "web/frontend/dist is missing — building the frontend first"; \
		$(MAKE) web-build; \
	fi

build-debug: web-check
	go build -ldflags="-X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)" -o tachi .

build-linux: web-check
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o tachi-linux-amd64 .

build-linux-arm64: web-check
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o tachi-linux-arm64 .

test: web-check
	go test ./...

test-cover: web-check
	@echo "=== Running tests with coverage ==="
	@go test -coverprofile=coverage.out ./...
	@echo ""
	@echo "=== Per-package coverage ==="
	@go tool cover -func=coverage.out | sort -k 3 -r
	@echo ""
	@echo "=== Total coverage: $$(go tool cover -func=coverage.out | tail -1 | awk '{print $$NF}') ==="
	@rm -f coverage.out

test-cover-html: web-check
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
# mockllm server (random port) + (run/acp) a real tachi subprocess or (tui)
# an in-process tea.Program, so specs are safe to run concurrently.
#   - `itest` runs all four suites with `make -j4` — package parallelism
#     (mockllm contract tests) AND ginkgo -p spec parallelism inside
#     run/acp/tui; total wall time ≈ mockllm + the slowest suite.
#   - `itest-run` / `itest-acp` / `itest-tui` additionally parallelize the
#     suite's specs via ginkgo -p (ITEST_PROCS processes, default 4; override
#     with `make ITEST_PROCS=8 itest-acp`).
#   - Each suite depends on `itest-mockllm`: the contract tests lock the
#     mock's wire format against the real SDK clients, so a wire-format
#     regression fails fast before the suites run.
GINKGO := go run github.com/onsi/ginkgo/v2/ginkgo
ITEST_PROCS ?= 4

itest: web-check
	$(MAKE) -j4 itest-mockllm itest-run itest-tui itest-acp

# Contract + unit tests for the mock LLM server (no integration build tag
# needed — the contract tests run under plain `go test` too).
itest-mockllm:
	go test ./itest/mockllm

itest-run: itest-mockllm
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/run

itest-tui: itest-mockllm
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/tui

itest-acp: itest-mockllm
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/acp

