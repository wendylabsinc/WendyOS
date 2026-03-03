.PHONY: build build-cli build-agent test vet proto clean fmt lint install \
       build-all \
       build-agent-linux-amd64 build-agent-linux-arm64 \
       build-cli-linux-amd64 build-cli-linux-arm64 \
       build-cli-darwin-amd64 build-cli-darwin-arm64 \
       build-cli-windows-amd64 build-cli-windows-arm64

VERSION ?= dev
LDFLAGS := -s -w -X github.com/wendylabsinc/wendy/internal/shared/version.Version=$(VERSION)

# Build both CLI and agent (native)
build: build-cli build-agent

# Build the wendy CLI
build-cli:
	go build -o bin/wendy ./cmd/wendy

# Build the wendy-agent
build-agent:
	go build -o bin/wendy-agent ./cmd/wendy-agent

# Cross-compile all targets
build-all: build-agent-linux-amd64 build-agent-linux-arm64 \
           build-cli-linux-amd64 build-cli-linux-arm64 \
           build-cli-darwin-amd64 build-cli-darwin-arm64 \
           build-cli-windows-amd64 build-cli-windows-arm64

build-agent-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-agent-linux-amd64 ./cmd/wendy-agent

build-agent-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-agent-linux-arm64 ./cmd/wendy-agent

build-cli-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-linux-amd64 ./cmd/wendy

build-cli-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-linux-arm64 ./cmd/wendy

build-cli-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-darwin-amd64 ./cmd/wendy

build-cli-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-darwin-arm64 ./cmd/wendy

build-cli-windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-windows-amd64.exe ./cmd/wendy

build-cli-windows-arm64:
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-windows-arm64.exe ./cmd/wendy

# Run all tests
test:
	go test ./... -v -count=1 -timeout 120s

# Run tests with race detector
test-race:
	go test ./... -v -count=1 -race -timeout 120s

# Run go vet
vet:
	go vet ./...

# Generate protobuf code
proto:
	bash scripts/generate-proto.sh

# Clean build artifacts
clean:
	rm -rf bin/

# Install both binaries
install:
	go install ./cmd/wendy
	go install ./cmd/wendy-agent

# Format code
fmt:
	gofmt -w -s .

# Lint (requires golangci-lint)
lint:
	golangci-lint run ./...
