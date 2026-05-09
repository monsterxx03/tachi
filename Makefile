.PHONY: build build-linux test

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

build:
	go build -ldflags="-X main.Version=$(VERSION)" -o tachi .

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="-X main.Version=$(VERSION)" -o tachi-linux-amd64 .

test:
	go test ./...
