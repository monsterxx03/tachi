.PHONY: build build-linux test

build:
	go build -o tachi .

build-linux:
	GOOS=linux GOARCH=amd64 go build -o tachi-linux-amd64 .

test:
	go test ./...
