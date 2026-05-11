#!/usr/bin/env bash
set -euo pipefail

logStep() {
  printf '==> %s\n' "$1"
}

checkCommand() {
  local command_name="$1"
  local label="${2:-$command_name}"

  printf 'Checking `%s` installed ... ' "$label"
  if command -v "$command_name" >/dev/null 2>&1; then
    printf '\033[32mYes\033[0m\n'
  else
    printf 'No\n' >&2
    echo "ERROR: Missing required tool: $label" >&2
    exit 1
  fi
}

installUbuntuPackages() {
  logStep "Installing Ubuntu E2E dependencies"
  sudo apt-get update
  sudo apt-get install -y --no-install-recommends \
    bash \
    build-essential \
    ca-certificates \
    curl \
    git \
    golang-go \
    make \
    openssh-client \
    rsync \
    unzip \
    zip
}

setupE2EUbuntu() {
  logStep "Setting up Swift E2E dependencies for Ubuntu"
  export DEBIAN_FRONTEND=noninteractive

  installUbuntuPackages

  checkCommand bash
  checkCommand curl
  checkCommand git
  checkCommand go
  checkCommand make
  checkCommand rsync
  checkCommand swift
  checkCommand zip
  checkCommand unzip
  checkCommand ssh "openssh-client"
}

setupE2EMacOS() {
  logStep "Setting up Swift E2E dependencies for macOS"

  checkCommand bash
  checkCommand curl
  checkCommand git
  checkCommand go
  checkCommand make
  checkCommand rsync
  checkCommand swift
  checkCommand zip
  checkCommand xcodebuild "Xcode command line tools"
}

case "$(uname -s)" in
  Darwin)
    setupE2EMacOS
    ;;
  Linux)
    if command -v lsb_release >/dev/null 2>&1; then
      distribution="$(lsb_release -is)"
    else
      distribution="$(. /etc/os-release && printf '%s' "${ID:-}")"
    fi

    case "${distribution,,}" in
      ubuntu)
        setupE2EUbuntu
        ;;
      *)
        echo "ERROR: Unsupported Linux distribution for E2E setup: ${distribution:-unknown}" >&2
        exit 1
        ;;
    esac
    ;;
  *)
    echo "ERROR: Unsupported platform for E2E setup: $(uname -s)" >&2
    exit 1
    ;;
esac
