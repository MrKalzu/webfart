.PHONY: all build test clean verify

all: build test

build:
	@mkdir -p bin
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/forum ./cmd/forum
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/forumctl ./cmd/forumctl
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/merkleverify ./cmd/merkleverify

test:
	go test -v ./...

clean:
	rm -rf bin/ threads/ data/
