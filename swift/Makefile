SHELL := /bin/bash

.DEFAULT_GOAL := help

SWIFT_FORMAT_TARGETS := WendyAgentCore/Sources WendyAgentCore/Tests WendyAgentMac/Sources WendyE2ETests/Sources WendyE2ETests/Tests

.PHONY: help format lint test test-e2e test-e2e-jetson-orin-nano test-e2e-raspberry-pi-5 test-e2e-mac-mini reference proto build build-cli build-dev agent-start agent-reset-settings agent-reset-tcc agent-reset agent-stop clean

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

test-e2e: ## Run Swift E2E tests and package command records
	bash ./Scripts/TestE2E.sh

test-e2e-jetson-orin-nano: DEVICE ?= wendyos-jetson-orin-nano.local
test-e2e-jetson-orin-nano: ## Run Swift E2E tests against Jetson Orin Nano
	WENDY_E2E_AGENT_OS=wendyOS bash ./Scripts/TestE2E.sh --agent-address $(DEVICE)

test-e2e-raspberry-pi-5: DEVICE ?= wendyos-raspberry-pi-5.local
test-e2e-raspberry-pi-5: ## Run Swift E2E tests against Raspberry Pi 5
	WENDY_E2E_AGENT_OS=wendyOS bash ./Scripts/TestE2E.sh --agent-address $(DEVICE)

test-e2e-mac-mini: DEVICE ?= mac-mini.local
test-e2e-mac-mini: ## Run Swift E2E tests against Mac mini
	WENDY_E2E_AGENT_OS=macOS bash ./Scripts/TestE2E.sh --agent-address $(DEVICE)

reference: ## Generate Swift E2E HTML reference documentation and open it
	rm -rf Build/Reference
	mkdir -p Build/Reference
	cd WendyE2ETests && swift run swift-e2e-testing reference --format html --output ../Build/Reference Tests/WendyE2ETests
	open Build/Reference/index.html

proto: ## Generate Swift gRPC code from Proto/
	./Scripts/GenerateProto.sh

clean: ## Remove Build/
	rm -rf Build/

build: ## Build, sign, notarize, and package the macOS app
	VERSION='$(VERSION)' OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/Build.sh

build-cli: ## Build the Go CLI in ../go
	$(MAKE) -C ../go build-cli

build-dev: ## Build and package a dev-signed macOS app without notarization
	OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/Build.sh --dev

agent-start: ## Stop, build, and open the WendyAgentMac app
	@$(MAKE) agent-stop
	@$(MAKE) build-dev
	open Build/WendyAgentMac.app

agent-reset-settings: ## Reset WendyAgentMac preferences/defaults
	./Scripts/ResetMacAppSettings.sh

agent-reset-tcc: ## Reset WendyAgentMac TCC permissions
	./Scripts/ResetMacAppTCC.sh

agent-reset: agent-reset-settings agent-reset-tcc ## Reset WendyAgentMac settings and TCC permissions

agent-stop: ## Quit WendyAgentMac and fail if it is still running after 10 seconds
	./Scripts/Quit.sh
