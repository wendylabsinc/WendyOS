.DEFAULT_GOAL := help

SWIFT_FORMAT_TARGETS := WendyAgentCore/Sources WendyAgentCore/Tests WendyAgentMac/Sources WendyE2ETests/Sources WendyE2ETests/Tests
SWIFT_LINT_TARGETS := WendyAgentCore/Sources WendyAgentCore/Tests WendyAgentMac/Sources

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
else
SHELL := /bin/bash
endif

WINDOWS_E2E_OUTPUT_DIR := "$$env:SystemRoot\Temp\wendy"
UNIX_E2E_OUTPUT_DIR := "/tmp/wendy"
RUN_E2E_TEST_WINDOWS = @if (-not $$PSVersionTable) { throw 'Windows E2E targets require PowerShell.' }; ./Scripts/E2ETest.ps1 --default-run-id "$(1)" $(2)
RUN_E2E_RUN_WINDOWS = @if (-not $$PSVersionTable) { throw 'Windows E2E targets require PowerShell.' }; ./Scripts/E2ERun.ps1 --default-run-id "$(1)" $(2)

OPEN_E2E_REPORT_UNIX = report_path="$(1)"; if [ -f "$$report_path" ]; then if [ "$$(uname -s)" = "Darwin" ]; then open "$$report_path" || echo "HTML report: $$report_path"; else echo "HTML report: $$report_path"; fi; else echo "HTML report not found: $$report_path"; fi
E2E_RUN_ID_HELPERS_UNIX = e2e_cli_id() { case "$$(uname -s)" in Darwin) major="$$(sw_vers -productVersion | cut -d. -f1)"; printf "macos-%s" "$$major" ;; Linux) if [ -r /etc/os-release ]; then . /etc/os-release; version="$${VERSION_ID//./-}"; printf "%s-%s" "$${ID:-linux}" "$$version"; else printf "linux"; fi ;; *) uname -s | tr '[:upper:]' '[:lower:]' | sed -E 's/[^[:alnum:]._-]+/-/g; s/-+/-/g; s/^-//; s/-$$//' ;; esac; }; next_e2e_run_id() { output_dir="$$1"; run_name="$$2"; eval_id="local$$(date -u +%y%m%d)"; base="swift-e2e-tests.$$eval_id.$$run_name"; max=0; shopt -s nullglob; for path in "$$output_dir/$$base".[0-9][0-9][0-9][0-9]; do leaf="$$(basename "$$path")"; suffix="$$(printf '%s' "$$leaf" | awk -F. '{ print $$NF }')"; if [[ "$$suffix" =~ ^[0-9]{4}$$ ]]; then suffix_value="$$(expr "$$suffix" + 0)"; if [ "$$suffix_value" -gt "$$max" ]; then max="$$suffix_value"; fi; fi; done; shopt -u nullglob; printf "%s.%04d" "$$base" $$((max + 1)); }
RUN_E2E_TEST_UNIX = @output_dir=$(UNIX_E2E_OUTPUT_DIR); \
	$(E2E_RUN_ID_HELPERS_UNIX); \
	run_name="$$(e2e_cli_id)-to-$(1)"; \
	run_id="$${WENDY_E2E_RUN_ID:-$$(next_e2e_run_id "$$output_dir" "$$run_name")}"; \
	WENDY_E2E_RUN_ID="$$run_id" WENDY_E2E_RUN_NAME="$$run_name" bash ./Scripts/E2ETest.sh $(2)
RUN_E2E_RUN_UNIX = @output_dir=$(UNIX_E2E_OUTPUT_DIR); \
	$(E2E_RUN_ID_HELPERS_UNIX); \
	run_name="$$(e2e_cli_id)-to-$(1)"; \
	run_id="$${WENDY_E2E_RUN_ID:-$$(next_e2e_run_id "$$output_dir" "$$run_name")}"; \
	run_base="$${run_id%.*}"; \
	aggregate_name="$${run_base%.*}"; \
	aggregate_dir="$$output_dir/$$aggregate_name"; \
	status=0; \
	WENDY_E2E_RUN_ID="$$run_id" WENDY_E2E_RUN_NAME="$$run_name" bash ./Scripts/E2ETest.sh $(2) || status=$$?; \
	bash ./Scripts/E2EAggregate.sh --output-dir "$$output_dir" "$$output_dir/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	bash ./Scripts/E2EReview.sh --run-dir "$$aggregate_dir" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	bash ./Scripts/E2EReport.sh --run-dir "$$aggregate_dir" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	$(call OPEN_E2E_REPORT_UNIX,$$aggregate_dir/index.html); \
	exit $$status

.PHONY: help
help: help-$(HOST_ENV) ## Show this help message

.PHONY: help-windows
help-windows:
	@if (-not $$PSVersionTable) { throw 'Windows make recipes require PowerShell.' }; Write-Output 'Usage:'; Write-Output '  make <target>'; Write-Output ''; Write-Output 'Targets:'; Select-String -Path '$(firstword $(MAKEFILE_LIST))' -Pattern '^[a-zA-Z0-9_-]+:.*?\#\# ' | ForEach-Object { $$parts = $$_.Line -split ':.*?\#\# ', 2; '  {0,-24} {1}' -f $$parts[0], $$parts[1] }

.PHONY: help-unix
help-unix:
	@echo 'Usage:'; echo '  make <target>'; echo ''; echo 'Targets:'; grep -E '^[a-zA-Z0-9_-]+:.*?\#\# .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?\#\# "}; {printf "  %-24s %s\n", $$1, $$2}'

.PHONY: format
format: format-$(HOST_ENV) ## Format Swift code using swift-format

.PHONY: format-windows
format-windows:
	swift format --recursive --in-place $(SWIFT_FORMAT_TARGETS)

.PHONY: format-unix
format-unix:
	swift format --recursive --in-place $(SWIFT_FORMAT_TARGETS)

.PHONY: lint
lint: lint-$(HOST_ENV) ## Lint Swift code using swift-format

.PHONY: lint-windows
lint-windows:
	swift format lint --recursive --strict $(SWIFT_LINT_TARGETS)

.PHONY: lint-unix
lint-unix:
	bash ./Scripts/Lint.sh

.PHONY: test
test: test-$(HOST_ENV) ## Run Swift package tests

.PHONY: test-windows
test-windows:
	@if (-not $$PSVersionTable) { throw 'Windows test target requires PowerShell.' }; New-Item -ItemType Directory -Force -Path Build/SwiftPM | Out-Null; Push-Location WendyAgentCore; try { swift test --scratch-path ../Build/SwiftPM; if ($$LASTEXITCODE -ne 0) { exit $$LASTEXITCODE } } finally { Pop-Location }

.PHONY: test-unix
test-unix:
	bash ./Scripts/Test.sh

e2e-test e2e-run: DEVICE ?= wendyos-raspberry-pi-5.local
.PHONY: e2e-test
e2e-test: e2e-test-$(HOST_ENV) ## Run Swift E2E tests locally and write raw artifacts

.PHONY: e2e-test-windows
e2e-test-windows:
	$(call RUN_E2E_TEST_WINDOWS,windows-to-raspberry-pi-5,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-test-unix
e2e-test-unix:
	$(call RUN_E2E_TEST_UNIX,local,--output-dir $(UNIX_E2E_OUTPUT_DIR) --cli-root-dir $(UNIX_E2E_OUTPUT_DIR) --agent-root-dir $(UNIX_E2E_OUTPUT_DIR) --parallel)

.PHONY: e2e-run
e2e-run: e2e-run-$(HOST_ENV) ## Run Swift E2E tests locally, review results, render the report, and open it

.PHONY: e2e-run-windows
e2e-run-windows:
	$(call RUN_E2E_RUN_WINDOWS,windows-to-raspberry-pi-5,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-run-unix
e2e-run-unix:
	$(call RUN_E2E_RUN_UNIX,local,--output-dir $(UNIX_E2E_OUTPUT_DIR) --cli-root-dir $(UNIX_E2E_OUTPUT_DIR) --agent-root-dir $(UNIX_E2E_OUTPUT_DIR) --parallel)

e2e-test-jetson-orin-nano e2e-run-jetson-orin-nano: DEVICE ?= wendyos-jetson-orin-nano.local
.PHONY: e2e-test-jetson-orin-nano
e2e-test-jetson-orin-nano: e2e-test-jetson-orin-nano-$(HOST_ENV) ## Run Swift E2E tests against Jetson Orin Nano and write raw artifacts

.PHONY: e2e-test-jetson-orin-nano-windows
e2e-test-jetson-orin-nano-windows:
	$(call RUN_E2E_TEST_WINDOWS,jetson-orin-nano,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-test-jetson-orin-nano-unix
e2e-test-jetson-orin-nano-unix:
	$(call RUN_E2E_TEST_UNIX,jetson-orin-nano,--output-dir $(UNIX_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-run-jetson-orin-nano
e2e-run-jetson-orin-nano: e2e-run-jetson-orin-nano-$(HOST_ENV) ## Run Swift E2E tests against Jetson Orin Nano, review results, render the report, and open it

.PHONY: e2e-run-jetson-orin-nano-windows
e2e-run-jetson-orin-nano-windows:
	$(call RUN_E2E_RUN_WINDOWS,jetson-orin-nano,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-run-jetson-orin-nano-unix
e2e-run-jetson-orin-nano-unix:
	$(call RUN_E2E_RUN_UNIX,jetson-orin-nano,--output-dir $(UNIX_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

e2e-test-raspberry-pi-5 e2e-run-raspberry-pi-5: DEVICE ?= wendyos-raspberry-pi-5.local
.PHONY: e2e-test-raspberry-pi-5
e2e-test-raspberry-pi-5: e2e-test-raspberry-pi-5-$(HOST_ENV) ## Run Swift E2E tests against Raspberry Pi 5 and write raw artifacts

.PHONY: e2e-test-raspberry-pi-5-windows
e2e-test-raspberry-pi-5-windows:
	$(call RUN_E2E_TEST_WINDOWS,raspberry-pi-5,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-test-raspberry-pi-5-unix
e2e-test-raspberry-pi-5-unix:
	$(call RUN_E2E_TEST_UNIX,raspberry-pi-5,--output-dir $(UNIX_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-run-raspberry-pi-5
e2e-run-raspberry-pi-5: e2e-run-raspberry-pi-5-$(HOST_ENV) ## Run Swift E2E tests against Raspberry Pi 5, review results, render the report, and open it

.PHONY: e2e-run-raspberry-pi-5-windows
e2e-run-raspberry-pi-5-windows:
	$(call RUN_E2E_RUN_WINDOWS,raspberry-pi-5,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

.PHONY: e2e-run-raspberry-pi-5-unix
e2e-run-raspberry-pi-5-unix:
	$(call RUN_E2E_RUN_UNIX,raspberry-pi-5,--output-dir $(UNIX_E2E_OUTPUT_DIR) --agent-os wendyOS --agent-address "$(DEVICE)")

e2e-test-mac-mini e2e-run-mac-mini: DEVICE ?= mac-mini.local
.PHONY: e2e-test-mac-mini
e2e-test-mac-mini: e2e-test-mac-mini-$(HOST_ENV) ## Run Swift E2E tests against Mac mini and write raw artifacts

.PHONY: e2e-test-mac-mini-windows
e2e-test-mac-mini-windows:
	$(call RUN_E2E_TEST_WINDOWS,mac-mini,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os macOS --agent-address "$(DEVICE)")

.PHONY: e2e-test-mac-mini-unix
e2e-test-mac-mini-unix:
	$(call RUN_E2E_TEST_UNIX,mac-mini,--output-dir $(UNIX_E2E_OUTPUT_DIR) --agent-os macOS --agent-address "$(DEVICE)")

.PHONY: e2e-run-mac-mini
e2e-run-mac-mini: e2e-run-mac-mini-$(HOST_ENV) ## Run Swift E2E tests against Mac mini, review results, render the report, and open it

.PHONY: e2e-run-mac-mini-windows
e2e-run-mac-mini-windows:
	$(call RUN_E2E_RUN_WINDOWS,mac-mini,--output-dir $(WINDOWS_E2E_OUTPUT_DIR) --agent-os macOS --agent-address "$(DEVICE)")

.PHONY: e2e-run-mac-mini-unix
e2e-run-mac-mini-unix:
	$(call RUN_E2E_RUN_UNIX,mac-mini,--output-dir $(UNIX_E2E_OUTPUT_DIR) --agent-os macOS --agent-address "$(DEVICE)")

.PHONY: reference
reference: reference-$(HOST_ENV) ## Generate Swift E2E HTML reference documentation and open it

.PHONY: reference-windows
reference-windows:
	@if (-not $$PSVersionTable) { throw 'Windows reference target requires PowerShell.' }; Remove-Item -LiteralPath Build/Reference -Recurse -Force -ErrorAction SilentlyContinue; New-Item -ItemType Directory -Force -Path Build/Reference | Out-Null; Push-Location WendyE2ETests; try { swift run swift-e2e-testing reference --format html --output ../Build/Reference Tests/WendyE2ETests; if ($$LASTEXITCODE -ne 0) { exit $$LASTEXITCODE } } finally { Pop-Location }; Start-Process Build/Reference/index.html

.PHONY: reference-unix
reference-unix:
	rm -rf Build/Reference
	mkdir -p Build/Reference
	cd WendyE2ETests && swift run swift-e2e-testing reference --format html --output ../Build/Reference Tests/WendyE2ETests
	open Build/Reference/index.html

.PHONY: proto
proto: proto-$(HOST_ENV) ## Generate Swift gRPC code from Proto/

.PHONY: proto-windows
proto-windows:
	@throw 'proto is not supported on Windows yet; use a unix host for Scripts/GenerateProto.sh.'

.PHONY: proto-unix
proto-unix:
	./Scripts/GenerateProto.sh

.PHONY: clean
clean: clean-$(HOST_ENV) ## Remove Build/

.PHONY: clean-windows
clean-windows:
	Remove-Item -LiteralPath Build -Recurse -Force -ErrorAction SilentlyContinue

.PHONY: clean-unix
clean-unix:
	rm -rf Build/

.PHONY: agent-release
agent-release: agent-release-$(HOST_OS) ## Build, sign, notarize, and package the macOS app

.PHONY: agent-release-macos
agent-release-macos:
	VERSION='$(VERSION)' OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/Build.sh

.PHONY: agent-release-linux
agent-release-linux:
	@echo 'ERROR: agent-release is macOS-only.' >&2; exit 1

.PHONY: agent-release-windows
agent-release-windows:
	@throw 'agent-release is macOS-only.'

.PHONY: build-cli
build-cli: build-cli-$(HOST_ENV) ## Build the Go CLI in ../go

.PHONY: build-cli-windows
build-cli-windows:
	$(MAKE) -C ../go build-cli HOST_ENV=windows

.PHONY: build-cli-unix
build-cli-unix:
	$(MAKE) -C ../go build-cli HOST_ENV=unix

.PHONY: agent-build
agent-build: agent-build-$(HOST_OS) ## Build and package a dev-signed macOS app without notarization

.PHONY: agent-build-macos
agent-build-macos:
	OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/Build.sh --dev

.PHONY: agent-build-linux
agent-build-linux:
	@echo 'ERROR: agent-build is macOS-only.' >&2; exit 1

.PHONY: agent-build-windows
agent-build-windows:
	@throw 'agent-build is macOS-only.'

.PHONY: agent-start
agent-start: agent-start-$(HOST_OS) ## Stop, build, and open the WendyAgentMac app

.PHONY: agent-start-macos
agent-start-macos:
	@$(MAKE) agent-stop
	@$(MAKE) agent-build
	open Build/WendyAgentMac.app

.PHONY: agent-start-linux
agent-start-linux:
	@echo 'ERROR: agent-start is macOS-only.' >&2; exit 1

.PHONY: agent-start-windows
agent-start-windows:
	@throw 'agent-start is macOS-only.'

.PHONY: agent-reset-settings
agent-reset-settings: agent-reset-settings-$(HOST_OS) ## Reset WendyAgentMac preferences/defaults

.PHONY: agent-reset-settings-macos
agent-reset-settings-macos:
	./Scripts/ResetMacAppSettings.sh

.PHONY: agent-reset-settings-linux
agent-reset-settings-linux:
	@echo 'ERROR: agent-reset-settings is macOS-only.' >&2; exit 1

.PHONY: agent-reset-settings-windows
agent-reset-settings-windows:
	@throw 'agent-reset-settings is macOS-only.'

.PHONY: agent-reset-permissions
agent-reset-permissions: agent-reset-permissions-$(HOST_OS) ## Reset WendyAgentMac permissions

.PHONY: agent-reset-permissions-macos
agent-reset-permissions-macos:
	./Scripts/ResetMacAppTCC.sh

.PHONY: agent-reset-permissions-linux
agent-reset-permissions-linux:
	@echo 'ERROR: agent-reset-permissions is macOS-only.' >&2; exit 1

.PHONY: agent-reset-permissions-windows
agent-reset-permissions-windows:
	@throw 'agent-reset-permissions is macOS-only.'

.PHONY: agent-reset
agent-reset: agent-reset-$(HOST_OS) ## Reset WendyAgentMac settings and permissions

.PHONY: agent-reset-macos
agent-reset-macos: agent-reset-settings-macos agent-reset-permissions-macos

.PHONY: agent-reset-linux
agent-reset-linux:
	@echo 'ERROR: agent-reset is macOS-only.' >&2; exit 1

.PHONY: agent-reset-windows
agent-reset-windows:
	@throw 'agent-reset is macOS-only.'

.PHONY: agent-stop
agent-stop: agent-stop-$(HOST_OS) ## Quit WendyAgentMac and fail if it is still running after 10 seconds

.PHONY: agent-stop-macos
agent-stop-macos:
	./Scripts/Quit.sh

.PHONY: agent-stop-linux
agent-stop-linux:
	@echo 'ERROR: agent-stop is macOS-only.' >&2; exit 1

.PHONY: agent-stop-windows
agent-stop-windows:
	@throw 'agent-stop is macOS-only.'
