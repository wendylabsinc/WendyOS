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
    openssh-server \
    pkg-config \
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
  case "$(uname -s)" in
    Darwin)
      if sshLoopbackWorks; then
        return 0
      fi

      if sudo -n /usr/sbin/systemsetup -setremotelogin on >/dev/null 2>&1; then
        sudo -n /bin/launchctl kickstart -k system/com.openssh.sshd >/dev/null 2>&1 || true
        return 0
      fi

      echo "ERROR: SSH loopback is required for Swift E2E sessions." >&2
      echo "Enable macOS Remote Login, or allow this runner to run without a sudo prompt:" >&2
      echo "  sudo systemsetup -setremotelogin on" >&2
      return 1
      ;;
    *)
      if command -v systemctl >/dev/null 2>&1; then
        sudo systemctl enable --now ssh >/dev/null 2>&1 || sudo systemctl start ssh >/dev/null 2>&1 || true
      elif command -v service >/dev/null 2>&1; then
        sudo service ssh start >/dev/null 2>&1 || true
      fi
      ;;
  esac
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

setupE2EUbuntu() {
  logStep "Setting up Swift E2E dependencies for Ubuntu"
  export DEBIAN_FRONTEND=noninteractive

  installUbuntuPackages
  installSwiftUbuntuIfNeeded
  setupSSHLoopback

  checkCommand bash
  checkCommand curl
  checkCommand git
  checkCommand go
  checkCommand make
  checkCommand swift
  checkCommand swiftly
  checkCommand zip
  checkCommand unzip
  checkCommand ssh "openssh-client"
  checkCommand sshd "openssh-server"
}

setupE2EMacOS() {
  logStep "Setting up Swift E2E dependencies for macOS"

  checkCommand bash
  checkCommand curl
  checkCommand git
  checkCommand go
  checkCommand make
  checkCommand swift
  checkCommand zip
  checkCommand ssh "openssh-client"
  checkCommand ssh-keygen
  checkCommand xcodebuild "Xcode command line tools"

  setupSSHLoopback
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
