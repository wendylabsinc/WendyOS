SHELL := /bin/bash

.DEFAULT_GOAL := help

SWIFT_FORMAT_TARGETS := WendyAgentCore/Sources WendyAgentCore/Tests WendyAgentMac/Sources WendyE2ETests/Sources WendyE2ETests/Tests

.PHONY: help format lint test test-e2e reference proto build build-dev run reset-mac-app-settings reset-mac-app-tcc reset-mac-app quit clean

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

build-dev: ## Build and package a dev-signed macOS app without notarization
	OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/Build.sh --dev

run: ## Quit, build, and open the WendyAgentMac app
	@$(MAKE) quit
	@$(MAKE) build-dev
	open Build/WendyAgentMac.app

reset-mac-app-settings: ## Reset WendyAgentMac preferences/defaults
	./Scripts/ResetMacAppSettings.sh

reset-mac-app-tcc: ## Reset WendyAgentMac TCC permissions
	./Scripts/ResetMacAppTCC.sh

reset-mac-app: reset-mac-app-settings reset-mac-app-tcc ## Reset WendyAgentMac settings and TCC permissions

quit: ## Quit WendyAgentMac and fail if it is still running after 10 seconds
	./Scripts/Quit.sh
