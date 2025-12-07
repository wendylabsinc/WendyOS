# Add platform detection at the top of the Makefile
OS := $(shell uname -s)
ARCH := $(shell uname -m)

# Determine platform-specific path
ifeq ($(OS),Darwin)
  PLATFORM := $(ARCH)-apple-macosx
else ifeq ($(OS),Linux)
  PLATFORM := $(ARCH)-unknown-linux-gnu
else
  PLATFORM := unknown
endif

.PHONY: all clean wendy wendy-agent help format setup-hooks build proto deps build-network-daemon build-app-bundle force-protos

help: ## Show this help message
	@echo 'Usage:'
	@echo '  make <product>'
	@echo ''
	@echo 'Targets:'
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

all: build-cli build-helper build-network-daemon build-agent ## Build all executables

install-deps: ## Install dependencies needed for building
	brew install protoc-gen-grpc-swift

# Collect all proto source files
PROTO_SOURCES := $(shell find Proto -name "*.proto" 2>/dev/null)

# Sentinel file to track when protos were last generated
PROTO_SENTINEL := .proto-generated

# Only regenerate protos if proto files have changed
_protos: $(PROTO_SENTINEL)

$(PROTO_SENTINEL): $(PROTO_SOURCES)
	@echo "Proto files changed, regenerating..."
	./Scripts/Generate-Proto.sh
	@touch $(PROTO_SENTINEL)

# Force proto regeneration (useful for debugging)
force-protos: ## Force regeneration of proto files
	@rm -f $(PROTO_SENTINEL)
	@$(MAKE) _protos

build-cli: _protos ## Build the wendy CLI executable
	swiftly run swift build --product wendy
	echo "y" | cp -f .build/$(PLATFORM)/debug/wendy ~/bin/wendy-dev
	chmod +x ~/bin/wendy-dev

build-helper: _protos ## Build the wendy helper daemon executable
	swift build --product wendy-helper
	echo "y" | cp -f .build/$(PLATFORM)/debug/wendy-helper ~/bin/wendy-helper
	chmod +x ~/bin/wendy-helper

build-network-daemon: _protos ## Build the wendy network daemon executable
	swiftly run swift build --product wendy-network-daemon
	echo "y" | cp -f .build/$(PLATFORM)/debug/wendy-network-daemon ~/bin/wendy-network-daemon
	chmod +x ~/bin/wendy-network-daemon

build-app-bundle: build-cli build-helper build-network-daemon ## Build and codesign the WendyCLI.app bundle
	@echo "Building WendyCLI.app bundle..."
	mkdir -p WendyCLI.app/Contents/MacOS
	mkdir -p WendyCLI.app/Contents/Resources
	mkdir -p WendyCLI.app/Contents/Library/LaunchDaemons
	
	# Copy binaries
	cp .build/$(PLATFORM)/debug/wendy WendyCLI.app/Contents/MacOS/
	cp .build/$(PLATFORM)/debug/wendy-helper WendyCLI.app/Contents/Resources/
	cp .build/$(PLATFORM)/debug/wendy-network-daemon WendyCLI.app/Contents/MacOS/
	
	# Copy resources
	cp Sources/wendy/Resources/Info.plist WendyCLI.app/Contents/
	cp Sources/wendy/Resources/com.wendy.helper.plist WendyCLI.app/Contents/Resources/
	cp Sources/wendy/Resources/com.wendy.wendy-network-daemon.plist WendyCLI.app/Contents/Library/LaunchDaemons/
	
	# Codesign the bundle
	codesign --force --options runtime --entitlements Sources/wendy-network-daemon/wendy-network-daemon.entitlements --sign "$(CODESIGN_IDENTITY)" WendyCLI.app/Contents/MacOS/wendy-network-daemon
	codesign --force --options runtime --sign "$(CODESIGN_IDENTITY)" WendyCLI.app/Contents/Resources/wendy-helper
	codesign --force --options runtime --sign "$(CODESIGN_IDENTITY)" WendyCLI.app/Contents/MacOS/wendy
	codesign --force --options runtime --sign "$(CODESIGN_IDENTITY)" WendyCLI.app
	
	@echo "✅ WendyCLI.app bundle built and codesigned"

build-cli-linux: _protos ## build the wendy CLI for linux with musl
	swiftly run swift build +6.2.1 --swift-sdk aarch64-swift-linux-musl --product wendy -c release

build-agent: _protos ## Build the wendy agent executable
	swiftly run swift build +6.2.1 --swift-sdk aarch64-swift-linux-musl --product wendy-agent -c debug 
	cp .build/aarch64-swift-linux-musl/debug/wendy-agent .
	chmod +x wendy-agent
	@echo "Binary size: $$(du -h wendy-agent | cut -f1)"

build-agent-release: _protos ## Build the wendy agent executable in release mode
	swiftly run swift build +6.2.1 --swift-sdk aarch64-swift-linux-musl \
	--product wendy-agent \
	-c release \
	-Xswiftc -whole-module-optimization \
	-Xlinker --gc-sections \
	-Xlinker --strip-all

	cp .build/aarch64-swift-linux-musl/release/wendy-agent .
	strip wendy-agent 2>/dev/null || true

	chmod +x wendy-agent
	@echo "Binary size: $$(du -h wendy-agent | cut -f1)"

test: ## Run the tests
	swift test

cov-html: ## Run the tests and generate coverage report
	swift test --enable-code-coverage
	xcrun llvm-cov show \
		.build/debug/wendy-agentPackageTests.xctest/Contents/MacOS/wendy-agentPackageTests \
		-instr-profile=.build/debug/codecov/default.profdata \
		Sources/ \
		--format=html \
		-o coverage_report
	open coverage_report/index.html

cov-report: ## Run the tests and generate coverage report
	swift test --enable-code-coverage
	xcrun llvm-cov export \
		.build/debug/wendy-agentPackageTests.xctest/Contents/MacOS/wendy-agentPackageTests \
		-instr-profile=.build/debug/codecov/default.profdata \
		--include-directory=Sources/ \
		-format=json > coverage.json
	xcrun llvm-cov export \
		.build/debug/wendy-agentPackageTests.xctest/Contents/MacOS/wendy-agentPackageTests \
		-instr-profile=.build/debug/codecov/default.profdata \
		--include-directory=Sources/ \
		-format=lcov > coverage.lcov
format: ## Format Swift code using swift-format
	swiftly run swift format --recursive --in-place Sources/ Tests/

setup-hooks: ## Install git hooks
	./Scripts/install-hooks.sh

clean: ## Clean build artifacts and remove executables
	swiftly run swift package clean
	rm -f wendy WendyAgent $(PROTO_SENTINEL)

# Default build target
build:
	@echo "Building wendy-agent..."
	@mkdir -p bin
	@cd cmd/wendy-agent && go build -o ../../bin/wendy-agent

# Generate protocol buffer code
proto:
	@echo "Generating protobuf code..."
	@mkdir -p internal/proto
	@protoc \
		--go_out=. --go_opt=module=github.com/wendylabsinc/wendy-agent-go \
		--go-grpc_out=. --go-grpc_opt=module=github.com/wendylabsinc/wendy-agent-go \
		api/proto/wendy/agent/services/v1/*.proto
	@# Move generated files to internal/proto
	@mkdir -p internal/proto/wendy/agent/services/v1
	@mv api/proto/wendy/agent/services/v1/*.pb.go internal/proto/wendy/agent/services/v1/

# Install dependencies
deps:
	@echo "Installing dependencies..."
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Default target
all: proto build 
