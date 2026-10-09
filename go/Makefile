ifeq ($(OS),Windows_NT)
HOST_ENV ?= windows
UNAME_S :=
else
HOST_ENV ?= unix
UNAME_S := $(shell uname -s 2>/dev/null)
endif

.PHONY: build build-cli build-agent test vet proto clean fmt lint install \
       build-all \
       build-agent-linux-amd64 build-agent-linux-arm64 \
       build-cli-linux-amd64 build-cli-linux-arm64 \
       build-cli-darwin-amd64 build-cli-darwin-arm64 \
       build-cli-windows-amd64 build-cli-windows-arm64 \
       msi-windows-amd64 msi-windows-arm64 \
       licenses licenses-update \
       sync-assets

VERSION ?= dev
MSI_VERSION ?= 0.0.1
LDFLAGS := -s -w -X github.com/wendylabsinc/wendy/go/internal/shared/version.Version=$(VERSION)
ifeq ($(UNAME_S),Darwin)
# Go/cgo adds -lobjc for each Objective-C package. The native CLI links
# multiple CoreBluetooth-backed packages, and recent Apple linkers warn about
# the duplicate even though it is harmless.
CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
export CGO_LDFLAGS
# If Swiftly is installed, its clang shim (~/.swiftly/bin/clang) is often
# ahead of /usr/bin on PATH and refuses to run whenever the repo's
# .swift-version doesn't match an installed Swift toolchain — which breaks
# cgo builds with an opaque "exit status 2" and no output. Force the real
# Apple clang so Go's cgo toolchain never goes through the shim.
CC ?= /usr/bin/clang
export CC
endif

# Build both CLI and agent (native)
build: build-cli build-agent

# Generate the embedded MCP UI from locked frontend dependencies.
.PHONY: mcp-app test-race
mcp-app:
	npm --prefix ../web-client/mcp-app ci --no-audit --no-fund
	npm --prefix ../web-client/mcp-app run build

# Build the wendy CLI
build-cli: mcp-app
ifeq ($(HOST_ENV),windows)
	powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "New-Item -ItemType Directory -Force -Path bin | Out-Null; go build -o bin/wendy.exe ./cmd/wendy; if ($$LASTEXITCODE -ne 0) { exit $$LASTEXITCODE }"
else
	mkdir -p bin
	go build -o bin/wendy ./cmd/wendy
endif

# Build the wendy-agent
build-agent:
	mkdir -p bin
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

# The Linux CLI links libusb via cgo (Thor USB recovery flashing), so it can only
# be built natively: Linux host, matching arch, libusb dev headers installed (see
# scripts/build-wendy.sh). Release binaries come from CI (build.yml
# build-cli-linux), which links libusb statically against musl. Elsewhere the
# target skips — and removes any stale binary so build-all can't hand out an
# outdated one.
define build-cli-linux-native
	@if [ "$(UNAME_S)" = "Linux" ] && [ "$$(go env GOHOSTARCH)" = "$(1)" ]; then \
		CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o bin/wendy-linux-$(1) ./cmd/wendy; \
	else \
		rm -f bin/wendy-linux-$(1); \
		echo "skipping bin/wendy-linux-$(1): the Linux CLI needs native cgo/libusb — build it on a Linux $(1) host or in CI"; \
	fi
endef

build-cli-linux-amd64: mcp-app
	$(call build-cli-linux-native,amd64)

build-cli-linux-arm64: mcp-app
	$(call build-cli-linux-native,arm64)

build-cli-darwin-amd64: mcp-app
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-darwin-amd64 ./cmd/wendy

build-cli-darwin-arm64: mcp-app
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-darwin-arm64 ./cmd/wendy

build-cli-windows-amd64: mcp-app
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/wendy-windows-amd64.exe ./cmd/wendy

build-cli-windows-arm64: mcp-app
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
test: mcp-app
	go test ./... -v -count=1 -timeout 120s

# Run tests with race detector
test-race: mcp-app
	go test ./... -v -count=1 -race -timeout 120s

# Run go vet
vet: mcp-app
	go vet ./...

# Generate protobuf code
proto:
	bash scripts/generate-proto.sh

# Clean build artifacts
clean:
	rm -rf bin/

# Install both binaries
install: mcp-app
	go install ./cmd/wendy
	go install ./cmd/wendy-agent

# Format code
fmt:
	gofmt -w -s .

# Lint (requires golangci-lint)
lint: mcp-app
	golangci-lint run ./...

# Check that dependency licenses match the committed baseline (licenses.csv)
licenses: mcp-app
	bash scripts/check-licenses.sh

# Regenerate licenses.csv from current dependencies
licenses-update: mcp-app
	bash scripts/check-licenses.sh --update

# Sync embedded docs and skills from sibling repos (requires them checked out at ../../docs and ../claude-skills)
# Note: go/ is a subdirectory; DOCS_SRC is two levels up, ASSETS is relative to go/
sync-assets:
	@DOCS_SRC=../../docs SKILLS_SRC=../claude-skills ASSETS=internal/cli/assets; \
	if [ ! -d "$$DOCS_SRC" ]; then echo "Docs repo not found at $$DOCS_SRC — skipping"; else \
	  rm -rf "$$ASSETS/docs" && mkdir -p "$$ASSETS/docs" && \
	  rsync -a --exclude='.git' --exclude='superpowers/' --exclude='operations/' \
	    --exclude='security/' --exclude='.github/' --exclude='.claude/' --exclude='*.json' \
	    "$$DOCS_SRC/" "$$ASSETS/docs/" && \
	  echo "Synced $$(find $$ASSETS/docs -name '*.md' | wc -l) doc files"; fi; \
	if [ ! -d "$$SKILLS_SRC" ]; then echo "Skills repo not found at $$SKILLS_SRC — skipping"; else \
	  rm -rf "$$ASSETS/skills" && mkdir -p "$$ASSETS/skills" && \
	  cp "$$SKILLS_SRC/.claude-plugin/marketplace.json" "$$ASSETS/skills/marketplace.json" && \
	  for skill in wendy wendy-lite wendy-contributing wendy-swift swift swift-concurrency \
	               swift-nio swift-library-design swift-valkey postgres-nio hummingbird \
	               deckset linear database-driver-design; do \
	    [ -d "$$SKILLS_SRC/$$skill" ] && rsync -a --exclude='.DS_Store' "$$SKILLS_SRC/$$skill/" "$$ASSETS/skills/$$skill/"; \
	  done && \
	  echo "Synced $$(find $$ASSETS/skills -type f | wc -l) skill files"; fi
