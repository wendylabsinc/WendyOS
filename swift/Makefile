SHELL := /bin/bash

.DEFAULT_GOAL := help

SWIFT_FORMAT_TARGETS := WendyAgentCore/Sources WendyAgentCore/Tests WendyAgentMac/Sources WendyE2ETests/Sources WendyE2ETests/Tests
E2E_REPORT_PATH := $$(ls -t Build/e2e/*/report.html 2>/dev/null | head -n 1)
OPEN_E2E_REPORT := report_path="$(E2E_REPORT_PATH)"; if [ -z "$$report_path" ]; then echo "HTML report not found"; elif [ "$$(uname -s)" = "Darwin" ]; then open "$$report_path" || echo "HTML report: $$report_path"; else echo "HTML report: $$report_path"; fi

.PHONY: help format lint test e2e-test e2e-test-jetson-orin-nano e2e-test-raspberry-pi-5 e2e-test-mac-mini reference proto agent-release build-cli agent-build agent-start agent-reset-settings agent-reset-permissions agent-reset agent-stop clean

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

e2e-test: ## Run Swift E2E tests and open the HTML report on macOS
	WENDY_E2E_RUN_ID=$${WENDY_E2E_RUN_ID:-local} bash ./Scripts/E2ETest.sh --output-dir Build/e2e --cli-root-dir Build/e2e --agent-root-dir Build/e2e --parallel
	@$(OPEN_E2E_REPORT)

e2e-test-jetson-orin-nano: DEVICE ?= wendyos-jetson-orin-nano.local
e2e-test-jetson-orin-nano: ## Run Swift E2E tests against Jetson Orin Nano and open the HTML report on macOS
	WENDY_E2E_RUN_ID=$${WENDY_E2E_RUN_ID:-jetson-orin-nano} WENDY_E2E_AGENT_OS=wendyOS bash ./Scripts/E2ETest.sh --output-dir Build/e2e --agent-address $(DEVICE)
	@$(OPEN_E2E_REPORT)

e2e-test-raspberry-pi-5: DEVICE ?= wendyos-raspberry-pi-5.local
e2e-test-raspberry-pi-5: ## Run Swift E2E tests against Raspberry Pi 5 and open the HTML report on macOS
	WENDY_E2E_RUN_ID=$${WENDY_E2E_RUN_ID:-raspberry-pi-5} WENDY_E2E_AGENT_OS=wendyOS bash ./Scripts/E2ETest.sh --output-dir Build/e2e --agent-address $(DEVICE)
	@$(OPEN_E2E_REPORT)

e2e-test-mac-mini: DEVICE ?= mac-mini.local
e2e-test-mac-mini: ## Run Swift E2E tests against Mac mini and open the HTML report on macOS
	WENDY_E2E_RUN_ID=$${WENDY_E2E_RUN_ID:-mac-mini} WENDY_E2E_AGENT_OS=macOS bash ./Scripts/E2ETest.sh --output-dir Build/e2e --agent-address $(DEVICE)
	@$(OPEN_E2E_REPORT)

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
