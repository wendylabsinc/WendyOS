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
    gnupg \
    gnupg2 \
    golang-go \
    libcurl4-openssl-dev \
    libncurses-dev \
    libpython3-dev \
    libxml2-dev \
    libz3-dev \
    lsb-release \
    make \
    openssh-client \
    pkg-config \
    rsync \
    tar \
    unzip \
    xz-utils \
    zip \
    zlib1g-dev
}

sourceSwiftlyEnvironment() {
  local env_file="${SWIFTLY_HOME_DIR:-$HOME/.local/share/swiftly}/env.sh"
  if [ -f "$env_file" ]; then
    # shellcheck disable=SC1090
    . "$env_file"
  fi
}

swiftlyUbuntuPlatform() {
  local version_id=""
  if [ -f /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    version_id="${VERSION_ID:-}"
  fi

  case "$version_id" in
    18.04|20.04|22.04|24.04)
      printf 'ubuntu%s' "$version_id"
      ;;
    *)
      echo "ERROR: Unsupported Ubuntu version for Swift E2E setup: ${version_id:-unknown}" >&2
      exit 1
      ;;
  esac
}

installSwiftlyUbuntuIfNeeded() {
  sourceSwiftlyEnvironment
  if command -v swiftly >/dev/null 2>&1; then
    return 0
  fi

  logStep "Installing swiftly"
  local architecture download_url platform temporary_dir
  architecture="$(uname -m)"
  platform="$(swiftlyUbuntuPlatform)"
  download_url="https://download.swift.org/swiftly/linux/swiftly-${architecture}.tar.gz"
  temporary_dir="$(mktemp -d)"
  trap 'rm -rf "$temporary_dir"' EXIT

  curl -fsSL "$download_url" -o "$temporary_dir/swiftly.tar.gz"
  tar -xzf "$temporary_dir/swiftly.tar.gz" -C "$temporary_dir"
  (
    cd "$temporary_dir"
    ./swiftly init \
      --assume-yes \
      --quiet-shell-followup \
      --platform "$platform"
  )

  rm -rf "$temporary_dir"
  trap - EXIT
  sourceSwiftlyEnvironment
  hash -r
}

installSwiftUbuntuIfNeeded() {
  sourceSwiftlyEnvironment
  if command -v swift >/dev/null 2>&1; then
    return 0
  fi

  installSwiftlyUbuntuIfNeeded
  sourceSwiftlyEnvironment
  if ! command -v swift >/dev/null 2>&1; then
    logStep "Installing Swift with swiftly"
    (
      cd "$HOME"
      swiftly install --use latest --assume-yes
    )
    sourceSwiftlyEnvironment
    hash -r
  fi
}

setupE2EUbuntu() {
  logStep "Setting up Swift E2E dependencies for Ubuntu"
  export DEBIAN_FRONTEND=noninteractive

  installUbuntuPackages
  installSwiftUbuntuIfNeeded

  checkCommand bash
  checkCommand curl
  checkCommand git
  checkCommand go
  checkCommand make
  checkCommand rsync
  checkCommand swift
  checkCommand swiftly
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
