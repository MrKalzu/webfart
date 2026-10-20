.PHONY: all build deploy deploy-aarch64 build-aarch64 test clean

CC_AARCH64 ?= aarch64-linux-gnu-gcc

all: build test

# Dev build: assets served from -static disk directory (live-reload friendly)
build:
	@mkdir -p bin
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/forum ./cmd/forum
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/forumctl ./cmd/forumctl
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/merkleverify ./cmd/merkleverify

# Deployment build: web assets embedded in the binary, no -static flag needed
deploy:
	@mkdir -p bin
	CGO_ENABLED=1 go build -tags deploy -ldflags="-s -w" -o bin/forum ./cmd/forum
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/forumctl ./cmd/forumctl
	CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/merkleverify ./cmd/merkleverify
	@echo "Deployment binaries built. Run: ./bin/forum -data ./threads"

# Cross-compile deployment build for aarch64 (ARM64)
deploy-aarch64:
	@mkdir -p bin/aarch64
	@if command -v $(CC_AARCH64) >/dev/null 2>&1; then \
		echo "Cross-compiling deploy build for aarch64 with $(CC_AARCH64)..."; \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_AARCH64) go build -tags deploy -ldflags="-s -w" -o bin/aarch64/forum ./cmd/forum && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_AARCH64) go build -ldflags="-s -w" -o bin/aarch64/forumctl ./cmd/forumctl && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_AARCH64) go build -ldflags="-s -w" -o bin/aarch64/merkleverify ./cmd/merkleverify && \
		echo "aarch64 deployment binaries built in bin/aarch64/"; \
	elif [ "$$(uname -m)" = "aarch64" ] || [ "$$(uname -m)" = "arm64" ]; then \
		echo "Building natively on aarch64..."; \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -tags deploy -ldflags="-s -w" -o bin/aarch64/forum ./cmd/forum && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/aarch64/forumctl ./cmd/forumctl && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/aarch64/merkleverify ./cmd/merkleverify && \
		echo "aarch64 deployment binaries built in bin/aarch64/"; \
	else \
		echo "Error: $(CC_AARCH64) not found."; \
		echo "To cross-compile for aarch64 on x86_64, install the cross-compiler:"; \
		echo "  sudo apt install gcc-aarch64-linux-gnu"; \
		echo "Or specify a custom cross-compiler:"; \
		echo "  make deploy-aarch64 CC_AARCH64=path/to/compiler"; \
		exit 1; \
	fi

# Cross-compile dev build for aarch64
build-aarch64:
	@mkdir -p bin/aarch64
	@if command -v $(CC_AARCH64) >/dev/null 2>&1; then \
		echo "Cross-compiling for aarch64 with $(CC_AARCH64)..."; \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_AARCH64) go build -ldflags="-s -w" -o bin/aarch64/forum ./cmd/forum && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_AARCH64) go build -ldflags="-s -w" -o bin/aarch64/forumctl ./cmd/forumctl && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=$(CC_AARCH64) go build -ldflags="-s -w" -o bin/aarch64/merkleverify ./cmd/merkleverify && \
		echo "aarch64 binaries built in bin/aarch64/"; \
	elif [ "$$(uname -m)" = "aarch64" ] || [ "$$(uname -m)" = "arm64" ]; then \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/aarch64/forum ./cmd/forum && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/aarch64/forumctl ./cmd/forumctl && \
		GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -ldflags="-s -w" -o bin/aarch64/merkleverify ./cmd/merkleverify; \
	else \
		echo "Error: $(CC_AARCH64) not found."; \
		echo "  sudo apt install gcc-aarch64-linux-gnu"; \
		exit 1; \
	fi

test:
	go test -v ./...

clean:
	rm -rf bin/ threads/ data/
