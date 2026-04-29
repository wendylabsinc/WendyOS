.PHONY: build build-cli build-agent test vet proto clean fmt lint install \
       build-all \
       build-agent-linux-amd64 build-agent-linux-arm64 \
       build-cli-linux-amd64 build-cli-linux-arm64 \
       build-cli-darwin-amd64 build-cli-darwin-arm64 \
       build-cli-windows-amd64 build-cli-windows-arm64 \
       msi-windows-amd64 msi-windows-arm64 \
       licenses licenses-update

VERSION ?= dev
MSI_VERSION ?= 0.0.1
LDFLAGS := -s -w -X github.com/wendylabsinc/wendy/internal/shared/version.Version=$(VERSION)
ifeq ($(shell uname -s),Darwin)
# Go/cgo adds -lobjc for each Objective-C package. The native CLI links
# multiple CoreBluetooth-backed packages, and recent Apple linkers warn about
# the duplicate even though it is harmless.
CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
export CGO_LDFLAGS
endif

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

# Build Windows MSI installers (requires WiX v4+: dotnet tool install --global wix)
WXS := installer/windows/wendy.wxs

msi-windows-amd64: build-cli-windows-amd64
	wix build -d Version=$(MSI_VERSION) -d ExePath=bin/wendy-windows-amd64.exe \
	     -arch x64 -o bin/wendy-windows-amd64.msi $(WXS)

msi-windows-arm64: build-cli-windows-arm64
	wix build -d Version=$(MSI_VERSION) -d ExePath=bin/wendy-windows-arm64.exe \
	     -arch arm64 -o bin/wendy-windows-arm64.msi $(WXS)

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

# Check that dependency licenses match the committed baseline (licenses.csv)
licenses:
	bash scripts/check-licenses.sh

# Regenerate licenses.csv from current dependencies
licenses-update:
	bash scripts/check-licenses.sh --update
