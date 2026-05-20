ifeq ($(OS),Windows_NT)
DETECTED_HOST_OS := windows
DETECTED_HOST_ENV := windows
else
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
DETECTED_HOST_OS := macos
DETECTED_HOST_ENV := unix
else ifeq ($(UNAME_S),Linux)
DETECTED_HOST_OS := linux
DETECTED_HOST_ENV := unix
else
DETECTED_HOST_OS := unknown
DETECTED_HOST_ENV := unix
endif
endif

HOST_OS ?= $(DETECTED_HOST_OS)
HOST_ENV ?= $(DETECTED_HOST_ENV)

ifeq ($(HOST_ENV),windows)
override SHELL := powershell.exe
.SHELLFLAGS := -NoProfile -ExecutionPolicy Bypass -Command
endif

VERSION ?= dev
MSI_VERSION ?= 0.0.1
LDFLAGS := -s -w -X github.com/wendylabsinc/wendy/internal/shared/version.Version=$(VERSION)
ifeq ($(HOST_OS),macos)
# Go/cgo adds -lobjc for each Objective-C package. The native CLI links
# multiple CoreBluetooth-backed packages, and recent Apple linkers warn about
# the duplicate even though it is harmless.
CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
export CGO_LDFLAGS
endif

WXS := installer/windows/wendy.wxs

GO_BUILD_CROSS_UNIX = CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) go build -ldflags "$(LDFLAGS)" -o $(3) $(4)
GO_BUILD_CROSS_WINDOWS = @if (-not $$PSVersionTable) { throw 'Windows make recipes require PowerShell.' }; $$env:CGO_ENABLED = '0'; $$env:GOOS = '$(1)'; $$env:GOARCH = '$(2)'; go build -ldflags "$(LDFLAGS)" -o $(3) $(4); if ($$LASTEXITCODE -ne 0) { exit $$LASTEXITCODE }

# Build both CLI and agent (native)
.PHONY: build
build: build-$(HOST_ENV)

.PHONY: build-unix
build-unix: build-cli-unix build-agent-unix

.PHONY: build-windows
build-windows: build-cli-windows build-agent-windows

# Build the wendy CLI
.PHONY: build-cli
build-cli: build-cli-$(HOST_ENV)

.PHONY: build-cli-unix
build-cli-unix:
	mkdir -p bin
	go build -o bin/wendy ./cmd/wendy

.PHONY: build-cli-windows
build-cli-windows:
	@if (-not $$PSVersionTable) { throw 'Windows build-cli requires PowerShell.' }; New-Item -ItemType Directory -Force -Path bin | Out-Null; go build -o bin/wendy.exe ./cmd/wendy; if ($$LASTEXITCODE -ne 0) { exit $$LASTEXITCODE }

# Build the wendy-agent
.PHONY: build-agent
build-agent: build-agent-$(HOST_ENV)

.PHONY: build-agent-unix
build-agent-unix:
	mkdir -p bin
	go build -o bin/wendy-agent ./cmd/wendy-agent

.PHONY: build-agent-windows
build-agent-windows:
	@throw 'build-agent is not supported on Windows yet.'

# Cross-compile all targets
.PHONY: build-all
build-all: build-all-$(HOST_ENV)

.PHONY: build-all-unix
build-all-unix: build-agent-linux-amd64-unix build-agent-linux-arm64-unix \
           build-cli-linux-amd64-unix build-cli-linux-arm64-unix \
           build-cli-darwin-amd64-unix build-cli-darwin-arm64-unix \
           build-cli-windows-amd64-unix build-cli-windows-arm64-unix

.PHONY: build-all-windows
build-all-windows: build-agent-linux-amd64-windows build-agent-linux-arm64-windows \
           build-cli-linux-amd64-windows build-cli-linux-arm64-windows \
           build-cli-darwin-amd64-windows build-cli-darwin-arm64-windows \
           build-cli-windows-amd64-windows build-cli-windows-arm64-windows

.PHONY: build-agent-linux-amd64
build-agent-linux-amd64: build-agent-linux-amd64-$(HOST_ENV)
.PHONY: build-agent-linux-amd64-unix
build-agent-linux-amd64-unix:
	$(call GO_BUILD_CROSS_UNIX,linux,amd64,bin/wendy-agent-linux-amd64,./cmd/wendy-agent)
.PHONY: build-agent-linux-amd64-windows
build-agent-linux-amd64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,linux,amd64,bin/wendy-agent-linux-amd64,./cmd/wendy-agent)

.PHONY: build-agent-linux-arm64
build-agent-linux-arm64: build-agent-linux-arm64-$(HOST_ENV)
.PHONY: build-agent-linux-arm64-unix
build-agent-linux-arm64-unix:
	$(call GO_BUILD_CROSS_UNIX,linux,arm64,bin/wendy-agent-linux-arm64,./cmd/wendy-agent)
.PHONY: build-agent-linux-arm64-windows
build-agent-linux-arm64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,linux,arm64,bin/wendy-agent-linux-arm64,./cmd/wendy-agent)

.PHONY: build-cli-linux-amd64
build-cli-linux-amd64: build-cli-linux-amd64-$(HOST_ENV)
.PHONY: build-cli-linux-amd64-unix
build-cli-linux-amd64-unix:
	$(call GO_BUILD_CROSS_UNIX,linux,amd64,bin/wendy-linux-amd64,./cmd/wendy)
.PHONY: build-cli-linux-amd64-windows
build-cli-linux-amd64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,linux,amd64,bin/wendy-linux-amd64,./cmd/wendy)

.PHONY: build-cli-linux-arm64
build-cli-linux-arm64: build-cli-linux-arm64-$(HOST_ENV)
.PHONY: build-cli-linux-arm64-unix
build-cli-linux-arm64-unix:
	$(call GO_BUILD_CROSS_UNIX,linux,arm64,bin/wendy-linux-arm64,./cmd/wendy)
.PHONY: build-cli-linux-arm64-windows
build-cli-linux-arm64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,linux,arm64,bin/wendy-linux-arm64,./cmd/wendy)

.PHONY: build-cli-darwin-amd64
build-cli-darwin-amd64: build-cli-darwin-amd64-$(HOST_ENV)
.PHONY: build-cli-darwin-amd64-unix
build-cli-darwin-amd64-unix:
	$(call GO_BUILD_CROSS_UNIX,darwin,amd64,bin/wendy-darwin-amd64,./cmd/wendy)
.PHONY: build-cli-darwin-amd64-windows
build-cli-darwin-amd64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,darwin,amd64,bin/wendy-darwin-amd64,./cmd/wendy)

.PHONY: build-cli-darwin-arm64
build-cli-darwin-arm64: build-cli-darwin-arm64-$(HOST_ENV)
.PHONY: build-cli-darwin-arm64-unix
build-cli-darwin-arm64-unix:
	$(call GO_BUILD_CROSS_UNIX,darwin,arm64,bin/wendy-darwin-arm64,./cmd/wendy)
.PHONY: build-cli-darwin-arm64-windows
build-cli-darwin-arm64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,darwin,arm64,bin/wendy-darwin-arm64,./cmd/wendy)

.PHONY: build-cli-windows-amd64
build-cli-windows-amd64: build-cli-windows-amd64-$(HOST_ENV)
.PHONY: build-cli-windows-amd64-unix
build-cli-windows-amd64-unix:
	$(call GO_BUILD_CROSS_UNIX,windows,amd64,bin/wendy-windows-amd64.exe,./cmd/wendy)
.PHONY: build-cli-windows-amd64-windows
build-cli-windows-amd64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,windows,amd64,bin/wendy-windows-amd64.exe,./cmd/wendy)

.PHONY: build-cli-windows-arm64
build-cli-windows-arm64: build-cli-windows-arm64-$(HOST_ENV)
.PHONY: build-cli-windows-arm64-unix
build-cli-windows-arm64-unix:
	$(call GO_BUILD_CROSS_UNIX,windows,arm64,bin/wendy-windows-arm64.exe,./cmd/wendy)
.PHONY: build-cli-windows-arm64-windows
build-cli-windows-arm64-windows:
	$(call GO_BUILD_CROSS_WINDOWS,windows,arm64,bin/wendy-windows-arm64.exe,./cmd/wendy)

# Build Windows MSI installers (requires WiX v4+: dotnet tool install --global wix)
.PHONY: msi-windows-amd64
msi-windows-amd64: msi-windows-amd64-$(HOST_ENV)
.PHONY: msi-windows-amd64-unix
msi-windows-amd64-unix: build-cli-windows-amd64-unix
	wix build -d Version=$(MSI_VERSION) -d ExePath=bin/wendy-windows-amd64.exe \
	     -arch x64 -o bin/wendy-windows-amd64.msi $(WXS)
.PHONY: msi-windows-amd64-windows
msi-windows-amd64-windows: build-cli-windows-amd64-windows
	wix build -d Version=$(MSI_VERSION) -d ExePath=bin/wendy-windows-amd64.exe -arch x64 -o bin/wendy-windows-amd64.msi $(WXS)

.PHONY: msi-windows-arm64
msi-windows-arm64: msi-windows-arm64-$(HOST_ENV)
.PHONY: msi-windows-arm64-unix
msi-windows-arm64-unix: build-cli-windows-arm64-unix
	wix build -d Version=$(MSI_VERSION) -d ExePath=bin/wendy-windows-arm64.exe \
	     -arch arm64 -o bin/wendy-windows-arm64.msi $(WXS)
.PHONY: msi-windows-arm64-windows
msi-windows-arm64-windows: build-cli-windows-arm64-windows
	wix build -d Version=$(MSI_VERSION) -d ExePath=bin/wendy-windows-arm64.exe -arch arm64 -o bin/wendy-windows-arm64.msi $(WXS)

# Run all tests
.PHONY: test
test: test-$(HOST_ENV)
.PHONY: test-unix
test-unix:
	go test ./... -v -count=1 -timeout 120s
.PHONY: test-windows
test-windows:
	go test ./... -v -count=1 -timeout 120s

# Run tests with race detector
.PHONY: test-race
test-race: test-race-$(HOST_ENV)
.PHONY: test-race-unix
test-race-unix:
	go test ./... -v -count=1 -race -timeout 120s
.PHONY: test-race-windows
test-race-windows:
	go test ./... -v -count=1 -race -timeout 120s

# Run go vet
.PHONY: vet
vet: vet-$(HOST_ENV)
.PHONY: vet-unix
vet-unix:
	go vet ./...
.PHONY: vet-windows
vet-windows:
	go vet ./...

# Generate protobuf code
.PHONY: proto
proto: proto-$(HOST_ENV)
.PHONY: proto-unix
proto-unix:
	bash scripts/generate-proto.sh
.PHONY: proto-windows
proto-windows:
	@throw 'proto is not supported on Windows yet; use a unix host for scripts/generate-proto.sh.'

# Clean build artifacts
.PHONY: clean
clean: clean-$(HOST_ENV)
.PHONY: clean-unix
clean-unix:
	rm -rf bin/
.PHONY: clean-windows
clean-windows:
	Remove-Item -LiteralPath bin -Recurse -Force -ErrorAction SilentlyContinue

# Install both binaries
.PHONY: install
install: install-$(HOST_ENV)
.PHONY: install-unix
install-unix:
	go install ./cmd/wendy
	go install ./cmd/wendy-agent
.PHONY: install-windows
install-windows:
	@throw 'install is not supported on Windows yet because wendy-agent is not available there.'

# Format code
.PHONY: fmt
fmt: fmt-$(HOST_ENV)
.PHONY: fmt-unix
fmt-unix:
	gofmt -w -s .
.PHONY: fmt-windows
fmt-windows:
	gofmt -w -s .

# Lint (requires golangci-lint)
.PHONY: lint
lint: lint-$(HOST_ENV)
.PHONY: lint-unix
lint-unix:
	golangci-lint run ./...
.PHONY: lint-windows
lint-windows:
	golangci-lint run ./...

# Check that dependency licenses match the committed baseline (licenses.csv)
.PHONY: licenses
licenses: licenses-$(HOST_ENV)
.PHONY: licenses-unix
licenses-unix:
	bash scripts/check-licenses.sh
.PHONY: licenses-windows
licenses-windows:
	@throw 'licenses is not supported on Windows yet; use a unix host for scripts/check-licenses.sh.'

# Regenerate licenses.csv from current dependencies
.PHONY: licenses-update
licenses-update: licenses-update-$(HOST_ENV)
.PHONY: licenses-update-unix
licenses-update-unix:
	bash scripts/check-licenses.sh --update
.PHONY: licenses-update-windows
licenses-update-windows:
	@throw 'licenses-update is not supported on Windows yet; use a unix host for scripts/check-licenses.sh.'
