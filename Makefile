.PHONY: build build-linux test

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

LDFLAGS := -X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)

build:
	go build -ldflags="$(LDFLAGS)" -o tachi .

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o tachi-linux-amd64 .

test:
	go test ./...
