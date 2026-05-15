SHELL := /bin/bash

.DEFAULT_GOAL := help

SWIFT_FORMAT_TARGETS := WendyAgentCore/Sources WendyAgentCore/Tests WendyAgentMac/Sources WendyE2ETests/Sources WendyE2ETests/Tests
E2E_REPORT_PATH := $$(ls -t Build/e2e/*/report.html 2>/dev/null | head -n 1)
OPEN_E2E_REPORT := report_path="$(E2E_REPORT_PATH)"; if [ -z "$$report_path" ]; then echo "HTML report not found"; elif [ "$$(uname -s)" = "Darwin" ]; then open "$$report_path" || echo "HTML report: $$report_path"; else echo "HTML report: $$report_path"; fi

.PHONY: help format lint test e2e-test e2e-analyze e2e-report e2e-test-jetson-orin-nano e2e-test-raspberry-pi-5 e2e-test-mac-mini reference proto agent-release build-cli agent-build agent-start agent-reset-settings agent-reset-permissions agent-reset agent-stop clean

help: ## Show this help message
	@echo 'Usage:'
	@echo '  make <target>'
	@echo ''
	@echo 'Targets:'
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-24s %s\n", $$1, $$2}'

format: ## Format Swift code using swift-format
	swift format --recursive --in-place $(SWIFT_FORMAT_TARGETS)

lint: ## Lint Swift code using swift-format
	bash ./Scripts/Lint.sh

test: ## Run Swift package tests
	bash ./Scripts/Test.sh

e2e-test: ## Run Swift E2E tests, run AI analysis, and open the HTML report on macOS
	@run_id="$${WENDY_E2E_RUN_ID:-local}"; \
	status=0; \
	WENDY_E2E_RUN_ID="$$run_id" bash ./Scripts/E2ETest.sh --output-dir Build/e2e --cli-root-dir Build/e2e --agent-root-dir Build/e2e --parallel || status=$$?; \
	bash ./Scripts/E2EAnalyze.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	bash ./Scripts/E2EReport.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	$(OPEN_E2E_REPORT); \
	exit $$status

e2e-analyze: ## Run Swift E2E AI analysis for RUN_DIR=Build/e2e/<run-id>
	@if [ -z "$(RUN_DIR)" ]; then echo "RUN_DIR is required (for example, RUN_DIR=Build/e2e/local)" >&2; exit 64; fi
	bash ./Scripts/E2EAnalyze.sh --run-dir "$(RUN_DIR)"

e2e-report: ## Render the Swift E2E HTML report for RUN_DIR=Build/e2e/<run-id>
	@if [ -z "$(RUN_DIR)" ]; then echo "RUN_DIR is required (for example, RUN_DIR=Build/e2e/local)" >&2; exit 64; fi
	bash ./Scripts/E2EReport.sh --run-dir "$(RUN_DIR)"
	@$(OPEN_E2E_REPORT)

e2e-test-jetson-orin-nano: DEVICE ?= wendyos-jetson-orin-nano.local
e2e-test-jetson-orin-nano: ## Run Swift E2E tests against Jetson Orin Nano, run AI analysis, and open the HTML report on macOS
	@run_id="$${WENDY_E2E_RUN_ID:-jetson-orin-nano}"; \
	status=0; \
	WENDY_E2E_RUN_ID="$$run_id" WENDY_E2E_AGENT_OS=wendyOS bash ./Scripts/E2ETest.sh --output-dir Build/e2e --agent-address $(DEVICE) || status=$$?; \
	bash ./Scripts/E2EAnalyze.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	bash ./Scripts/E2EReport.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	$(OPEN_E2E_REPORT); \
	exit $$status

e2e-test-raspberry-pi-5: DEVICE ?= wendyos-raspberry-pi-5.local
e2e-test-raspberry-pi-5: ## Run Swift E2E tests against Raspberry Pi 5, run AI analysis, and open the HTML report on macOS
	@run_id="$${WENDY_E2E_RUN_ID:-raspberry-pi-5}"; \
	status=0; \
	WENDY_E2E_RUN_ID="$$run_id" WENDY_E2E_AGENT_OS=wendyOS bash ./Scripts/E2ETest.sh --output-dir Build/e2e --agent-address $(DEVICE) || status=$$?; \
	bash ./Scripts/E2EAnalyze.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	bash ./Scripts/E2EReport.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	$(OPEN_E2E_REPORT); \
	exit $$status

e2e-test-mac-mini: DEVICE ?= mac-mini.local
e2e-test-mac-mini: ## Run Swift E2E tests against Mac mini, run AI analysis, and open the HTML report on macOS
	@run_id="$${WENDY_E2E_RUN_ID:-mac-mini}"; \
	status=0; \
	WENDY_E2E_RUN_ID="$$run_id" WENDY_E2E_AGENT_OS=macOS bash ./Scripts/E2ETest.sh --output-dir Build/e2e --agent-address $(DEVICE) || status=$$?; \
	bash ./Scripts/E2EAnalyze.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	bash ./Scripts/E2EReport.sh --run-dir "Build/e2e/$$run_id" || { step_status=$$?; [ $$status -eq 0 ] && status=$$step_status; }; \
	$(OPEN_E2E_REPORT); \
	exit $$status

reference: ## Generate Swift E2E HTML reference documentation and open it
	rm -rf Build/Reference
	mkdir -p Build/Reference
	cd WendyE2ETests && swift run swift-e2e-testing reference --format html --output ../Build/Reference Tests/WendyE2ETests
	open Build/Reference/index.html

proto: ## Generate Swift gRPC code from Proto/
	./Scripts/GenerateProto.sh

clean: ## Remove Build/
	rm -rf Build/

agent-release: ## Build, sign, notarize, and package the macOS app
	VERSION='$(VERSION)' OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/Build.sh

build-cli: ## Build the Go CLI in ../go
	$(MAKE) -C ../go build-cli

agent-build: ## Build and package a dev-signed macOS app without notarization
	OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/Build.sh --dev

agent-start: ## Stop, build, and open the WendyAgentMac app
	@$(MAKE) agent-stop
	@$(MAKE) agent-build
	open Build/WendyAgentMac.app

agent-reset-settings: ## Reset WendyAgentMac preferences/defaults
	./Scripts/ResetMacAppSettings.sh

agent-reset-permissions: ## Reset WendyAgentMac permissions
	./Scripts/ResetMacAppTCC.sh

agent-reset: agent-reset-settings agent-reset-permissions ## Reset WendyAgentMac settings and permissions

agent-stop: ## Quit WendyAgentMac and fail if it is still running after 10 seconds
	./Scripts/Quit.sh
