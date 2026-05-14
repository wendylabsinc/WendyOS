#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Prepare macOS for WendyAgent Swift E2E tests.

The setup asks for sudo access, installs Homebrew if needed, installs required
developer tools, installs Swift via swiftly if needed, and configures
passwordless SSH loopback for the current user.

Options:
  --help, -h  Show this help message.
EOF
}

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

requireSudo() {
  logStep "Requesting sudo access"
  sudo -v
}

installHomebrewIfNeeded() {
  if command -v brew >/dev/null 2>&1; then
    return 0
  fi

  logStep "Installing Homebrew"
  NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
  sourceHomebrewEnvironment
  hash -r
}

sourceHomebrewEnvironment() {
  if command -v brew >/dev/null 2>&1; then
    return 0
  fi

  local brew_shellenv=""
  case "$(uname -m)" in
    arm64)
      brew_shellenv="/opt/homebrew/bin/brew shellenv"
      ;;
    *)
      brew_shellenv="/usr/local/bin/brew shellenv"
      ;;
  esac

  # shellcheck disable=SC2086
  if [ -x "${brew_shellenv%% *}" ]; then
    eval "$($brew_shellenv)"
  fi
}

installHomebrewPackages() {
  logStep "Installing Homebrew E2E dependencies"
  sourceHomebrewEnvironment
  brew update
  brew install bash curl git go make zip
}

sourceSwiftlyEnvironment() {
  local env_file="${SWIFTLY_HOME_DIR:-$HOME/.swiftly}/env.sh"
  if [ -f "$env_file" ]; then
    # shellcheck disable=SC1090
    . "$env_file"
  fi
}

installSwiftlyMacOSIfNeeded() {
  sourceSwiftlyEnvironment
  if command -v swiftly >/dev/null 2>&1; then
    return 0
  fi

  logStep "Installing swiftly"
  local temporary_dir
  temporary_dir="$(mktemp -d)"
  trap 'rm -rf "$temporary_dir"' EXIT

  (
    cd "$temporary_dir"
    curl -O https://download.swift.org/swiftly/darwin/swiftly.pkg
    installer -pkg swiftly.pkg -target CurrentUserHomeDirectory
    ~/.swiftly/bin/swiftly init --quiet-shell-followup
  )

  rm -rf "$temporary_dir"
  trap - EXIT
  sourceSwiftlyEnvironment
  hash -r
}

installSwiftMacOSIfNeeded() {
  sourceSwiftlyEnvironment
  if command -v swift >/dev/null 2>&1; then
    return 0
  fi

  installSwiftlyMacOSIfNeeded
  sourceSwiftlyEnvironment
  if ! command -v swift >/dev/null 2>&1; then
    logStep "Installing Swift with swiftly"
    swiftly install --use latest --assume-yes
    sourceSwiftlyEnvironment
    hash -r
  fi
}

sshLoopbackWorks() {
  ssh \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR \
    -o ConnectTimeout=10 \
    localhost true >/dev/null 2>&1
}

startSSHServiceIfPossible() {
  if sshLoopbackWorks; then
    return 0
  fi

  sudo /usr/sbin/systemsetup -setremotelogin on >/dev/null 2>&1
  sudo /bin/launchctl kickstart -k system/com.openssh.sshd >/dev/null 2>&1 || true
}

setupSSHLoopback() {
  logStep "Setting up SSH loopback for E2E sessions"

  mkdir -p "$HOME/.ssh"
  chmod 700 "$HOME/.ssh"

  if [ ! -f "$HOME/.ssh/id_ed25519" ]; then
    ssh-keygen -q -t ed25519 -N "" -C "${USER:-wendy-e2e}@$(hostname)" -f "$HOME/.ssh/id_ed25519"
  fi

  touch "$HOME/.ssh/authorized_keys"
  chmod 600 "$HOME/.ssh/authorized_keys"

  local public_key
  public_key="$(cat "$HOME/.ssh/id_ed25519.pub")"
  if ! grep -qxF "$public_key" "$HOME/.ssh/authorized_keys"; then
    printf '%s\n' "$public_key" >> "$HOME/.ssh/authorized_keys"
  fi

  startSSHServiceIfPossible

  if ! sshLoopbackWorks; then
    echo "ERROR: Could not establish passwordless SSH to localhost." >&2
    echo "Swift E2E sessions execute local commands through SSH; verify Remote Login/sshd and ~/.ssh/authorized_keys." >&2
    exit 1
  fi
}

setupE2EMacOS() {
  logStep "Setting up Swift E2E dependencies for macOS"

  requireSudo
  installHomebrewIfNeeded
  installHomebrewPackages
  installSwiftMacOSIfNeeded
  setupSSHLoopback

  checkCommand bash
  checkCommand curl
  checkCommand git
  checkCommand go
  checkCommand make
  checkCommand swift
  checkCommand swiftly
  checkCommand zip
  checkCommand ssh "openssh-client"
  checkCommand ssh-keygen
  checkCommand xcodebuild "Xcode command line tools"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 64
      ;;
  esac
done

case "$(uname -s)" in
  Darwin)
    setupE2EMacOS
    ;;
  *)
    echo "ERROR: SetupE2E.macOS.sh must run on macOS; current platform: $(uname -s)" >&2
    exit 1
    ;;
esac
