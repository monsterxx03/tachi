.PHONY: build build-linux test test-cover test-cover-html lint lint-fix

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

LDFLAGS := -X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)

build:
	go build -ldflags="$(LDFLAGS)" -o tachi .

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o tachi-linux-amd64 .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o tachi-linux-arm64 .

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
	@echo "=== Running golangci-lint (with --fix) ==="
	golangci-lint run --fix ./...
