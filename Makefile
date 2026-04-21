.PHONY: build test

build:
	go build -o tachi .

test:
	go test ./...
