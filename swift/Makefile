SHELL := /bin/bash

.DEFAULT_GOAL := help

SWIFT_FORMAT_TARGETS := WendyAgentCore/Sources WendyAgentCore/Tests WendyAgentMac/Sources

.PHONY: help format lint test proto release-mac-app reset-mac-app-settings reset-mac-app-tcc reset-mac-app

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
	cd WendyAgentCore && swift test

proto: ## Generate Swift gRPC code from Proto/
	./Scripts/GenerateProto.sh

release-mac-app: ## Build, sign, notarize, and package the macOS app
	VERSION='$(VERSION)' OUTPUT_DIR='$(OUTPUT_DIR)' bash ./Scripts/ReleaseMacApp.sh

reset-mac-app-settings: ## Reset WendyAgentMac preferences/defaults
	./Scripts/ResetMacAppSettings.sh

reset-mac-app-tcc: ## Reset WendyAgentMac TCC permissions
	./Scripts/ResetMacAppTCC.sh

reset-mac-app: reset-mac-app-settings reset-mac-app-tcc ## Reset WendyAgentMac settings and TCC permissions
