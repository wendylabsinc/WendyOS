#!/usr/bin/env bash

# Re-exec under bash if invoked via sh (pipefail and [[ ]] require bash).
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi

set -euo pipefail

REPO="wendylabsinc/wendy-agent"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="wendy-agent"
HOMEBREW_TAP="wendylabsinc/tap"
HOMEBREW_CASK="wendy-agent"
HOMEBREW_CASK_QUALIFIED="wendylabsinc/tap/wendy-agent"
YES=false

usage() {
  cat <<EOF
Install the Wendy Agent.

The agent runs on Linux devices and Apple Silicon Macs, and provides remote
debugging and deployment capabilities.

Usage: install-agent.sh [OPTIONS]

Options:
  -y            Skip confirmation prompt
  -d DIR        Install directory (default: /usr/local/bin, only for binary fallback)
  -h, --help    Show this help message

Environment:
  WENDY_VERSION   Install a specific version (e.g. v0.2.0) instead of latest
  WENDY_ENROLLMENT_TOKEN
                  Pre-enroll this device into a Wendy Cloud org on first start.
                  Obtain it from 'wendy install' → "Linux Desktop" or "Headless Mac".
  WENDY_CLOUD_HOST
                  Wendy Cloud gRPC host (required when WENDY_ENROLLMENT_TOKEN is set).
EOF
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -y) YES=true; shift ;;
    -d) INSTALL_DIR="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "Unknown option: $1"; usage ;;
  esac
done

# --- Detect OS (the darwin/linux dispatch happens after helpers are defined) ---
case "$(uname -s)" in
  Linux*)  OS="linux" ;;
  Darwin*) OS="darwin" ;;
  *)       OS="unsupported" ;;
esac

# --- Determine sudo prefix ---
SUDO=""
if [[ "$(id -u)" -ne 0 ]]; then
  SUDO="sudo"
fi

# --- Detect Architecture ---
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) echo "unsupported" ;;
  esac
}

# >>> wendy-install-shared
# Shared installer helpers. This block MUST be byte-identical in cli.sh and
# agent.sh (enforced by .github/scripts/install-scripts_test.sh). It resolves
# the latest version from the GCS-hosted manifest first, so the mainstream
# install paths never call the rate-limited GitHub API.
MANIFEST_URL="https://install.wendy.sh/manifest.json"

# Fetch a raw URL to stdout using curl or wget.
fetch_stdout() {
  local url="$1"
  if command -v curl &>/dev/null; then
    curl -fsSL "$url"
  elif command -v wget &>/dev/null; then
    wget -qO- "$url"
  else
    return 1
  fi
}

# Print the manifest's stable "latest" version, or nothing on any failure.
# Matches the "latest" key only (not "latest_nightly").
manifest_latest() {
  fetch_stdout "$MANIFEST_URL" 2>/dev/null \
    | grep -oE '"latest"[[:space:]]*:[[:space:]]*"[^"]*"' \
    | head -1 \
    | sed -E 's/.*"([^"]*)"$/\1/'
}

# Print the newest GitHub release tag, or nothing on failure.
github_latest() {
  fetch_stdout "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
}

# Resolve the version to install: explicit override, else GCS manifest, else GitHub.
resolve_version() {
  if [[ -n "${WENDY_VERSION:-}" ]]; then
    echo "$WENDY_VERSION"
    return
  fi
  # `|| true` keeps a failed fetch (e.g. missing manifest) from tripping the
  # script's `set -e` inside the command substitution, so we can fall through.
  local v
  v="$(manifest_latest || true)"
  if [[ -n "$v" ]]; then
    echo "$v"
    return
  fi
  v="$(github_latest || true)"
  if [[ -n "$v" ]]; then
    echo "$v"
    return
  fi
  echo "Error: could not resolve the latest version from GCS or GitHub." >&2
  return 1
}

# --- Download helper ---
download() {
  local url="$1" dest="$2"
  if command -v curl &>/dev/null; then
    curl -fsSL -o "$dest" "$url"
  elif command -v wget &>/dev/null; then
    wget -qO "$dest" "$url"
  fi
}
# <<< wendy-install-shared

# --- Homebrew helpers (macOS) ---
homebrew_supports_trust() {
  brew help trust >/dev/null 2>&1
}

trust_homebrew_tap() {
  local tap="$1"

  if ! homebrew_supports_trust; then
    return 0
  fi

  echo "Trusting Homebrew tap: ${tap}"
  if brew trust "$tap"; then
    return 0
  fi

  echo "Error: Homebrew could not trust ${tap}." >&2
  echo "Run this command, then re-run the installer:" >&2
  echo "  brew trust ${tap}" >&2
  exit 1
}

trust_homebrew_cask() {
  local cask="$1"

  if ! homebrew_supports_trust; then
    return 0
  fi

  echo "Trusting Homebrew cask: ${cask}"
  if brew trust --cask "$cask"; then
    return 0
  fi

  echo "Error: Homebrew could not trust ${cask}." >&2
  echo "Run this command, then re-run the installer:" >&2
  echo "  brew trust --cask ${cask}" >&2
  exit 1
}

apt_install_or_upgrade() {
  local package="$1"
  if dpkg-query -W -f='${Status}' "$package" 2>/dev/null | grep -q "install ok installed"; then
    echo "Updating ${package}..."
    $SUDO apt-get install -y --only-upgrade "$package"
  else
    echo "Installing ${package}..."
    $SUDO apt-get install -y "$package"
  fi
}

dnf_install_or_upgrade() {
  local package="$1"
  if rpm -q "$package" &>/dev/null; then
    echo "Updating ${package}..."
    $SUDO dnf upgrade -y "$package"
  else
    echo "Installing ${package}..."
    $SUDO dnf install -y "$package"
  fi
}

yum_install_or_upgrade() {
  local package="$1"
  if rpm -q "$package" &>/dev/null; then
    echo "Updating ${package}..."
    $SUDO yum update -y "$package"
  else
    echo "Installing ${package}..."
    $SUDO yum install -y "$package"
  fi
}

# --- Prompt for confirmation ---
confirm() {
  if [[ "$YES" == true ]]; then return 0; fi
  printf "%s [y/N] " "$1"
  read -r answer </dev/tty
  case "$answer" in
    [yY]|[yY][eE][sS]) return 0 ;;
    *) echo "Aborted."; exit 1 ;;
  esac
}

# --- Stage a pre-enrollment token for the agent to self-enroll on startup ---
stage_enrollment() {
  local token="${WENDY_ENROLLMENT_TOKEN:-}"
  local cloud_host="${WENDY_CLOUD_HOST:-}"
  if [[ -z "$token" ]]; then
    return 0
  fi
  if [[ -z "$cloud_host" ]]; then
    echo "Warning: WENDY_ENROLLMENT_TOKEN is set but WENDY_CLOUD_HOST is not; skipping pre-enrollment." >&2
    return 0
  fi
  $SUDO mkdir -p /etc/wendy-agent
  # Write via printf | tee (not a heredoc) so the token is not echoed to
  # stdout. The values are JSON-encoded assuming no embedded quotes/backslashes
  # (tokens are base64url + dots; cloud host is a hostname[:port]).
  # Create the file 0600 from birth (umask in the subshell applies to the
  # file tee creates), so the bearer token is never world-readable. The
  # chmod below is a defensive backstop, not the sole protection.
  ( umask 077; printf '{"token":"%s","cloudHost":"%s"}\n' "$token" "$cloud_host" \
      | $SUDO tee /etc/wendy-agent/enrollment.json >/dev/null )
  $SUDO chmod 600 /etc/wendy-agent/enrollment.json
  echo "Enrollment token staged; the device will enroll on startup."
  # Nudge an already-running (package-installed) agent to re-read it.
  if command -v systemctl &>/dev/null; then
    $SUDO systemctl try-restart wendy-agent >/dev/null 2>&1 || true
  fi
}

ARCH=$(detect_arch)

if [[ "$ARCH" == "unsupported" ]]; then
  echo "Error: Unsupported architecture: $(uname -m)"
  exit 1
fi

# ===== macOS: install the Wendy Agent app (Homebrew cask, or the signed zip) =====
if [[ "$OS" == "darwin" ]]; then
  if [[ "$ARCH" != "arm64" ]]; then
    # On Apple Silicon running under Rosetta, uname -m reports x86_64; still install arm64.
    if [[ "$(sysctl -in hw.optional.arm64 2>/dev/null || echo 0)" == "1" ]]; then
      ARCH="arm64"
    else
      echo "Error: the Wendy Agent for macOS requires Apple Silicon (arm64)." >&2
      echo "Intel (x86_64) Macs are not supported." >&2
      exit 1
    fi
  fi

  if command -v brew &>/dev/null; then
    if homebrew_supports_trust; then
      echo "Homebrew detected. Will trust and install via:"
      echo "  brew tap ${HOMEBREW_TAP}"
      echo "  brew trust ${HOMEBREW_TAP}"
      echo "  brew trust --cask ${HOMEBREW_CASK_QUALIFIED}"
      echo "  brew install --cask ${HOMEBREW_CASK}"
    else
      echo "Homebrew detected. Will install via:"
      echo "  brew tap ${HOMEBREW_TAP}"
      echo "  brew install --cask ${HOMEBREW_CASK}"
    fi
    confirm "Proceed?"
    # Let a genuine tap failure abort (set -e) before the trust/install steps
    # run against a missing or partial tap. Re-tapping an existing tap is a
    # successful no-op, so this is safe to run unconditionally.
    brew tap "$HOMEBREW_TAP"
    trust_homebrew_tap "$HOMEBREW_TAP"
    trust_homebrew_cask "$HOMEBREW_CASK_QUALIFIED"
    brew install --cask "$HOMEBREW_CASK"
  else
    # No Homebrew — download the signed, notarized app bundle from GitHub and
    # install it to /Applications.
    TAG=$(resolve_version) || exit 1
    VERSION="${TAG#v}"
    ARTIFACT="wendy-agent-macos-${ARCH}-${VERSION}.zip"
    URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
    echo "Homebrew not found. Will download ${ARTIFACT}"
    echo "  and install WendyAgentMac.app to /Applications."
    confirm "Proceed?"

    TMPDIR_DL=$(mktemp -d)
    trap 'rm -rf "$TMPDIR_DL"' EXIT

    echo "Downloading ${URL}..."
    download "$URL" "${TMPDIR_DL}/${ARTIFACT}"

    # Extract with ditto: it is the macOS-native unarchiver, preserves .app
    # bundle metadata (including the notarization ticket), and confines writes
    # to the destination directory rather than honoring archive path-traversal
    # entries the way a bare `unzip` might.
    ditto -x -k "${TMPDIR_DL}/${ARTIFACT}" "${TMPDIR_DL}/extracted"

    APP_SRC=$(/usr/bin/find "${TMPDIR_DL}/extracted" -maxdepth 2 -name 'WendyAgentMac.app' -type d | head -1)
    if [[ -z "$APP_SRC" ]]; then
      echo "Error: WendyAgentMac.app not found in ${ARTIFACT}." >&2
      exit 1
    fi
    # Defense-in-depth: the located bundle must live inside our temp dir.
    case "$APP_SRC" in
      "${TMPDIR_DL}/extracted"/*) ;;
      *) echo "Error: unexpected app path outside the download directory: ${APP_SRC}" >&2; exit 1 ;;
    esac

    # Verify the bundle's code signature before copying it into /Applications.
    # A tampered artifact (compromised release/CDN) fails this check, so we
    # reject it here instead of relying on Gatekeeper's first-launch check,
    # which happens only after the bytes are already on disk.
    if ! codesign --verify --deep --strict "$APP_SRC" 2>/dev/null; then
      echo "Error: WendyAgentMac.app failed code-signature verification; refusing to install." >&2
      exit 1
    fi
    # Notarization/Gatekeeper policy is advisory here (a validly signed but
    # unstapled build should still install); surface a warning rather than block.
    if command -v spctl &>/dev/null && ! spctl --assess --type execute "$APP_SRC" >/dev/null 2>&1; then
      echo "Warning: Gatekeeper could not confirm notarization for WendyAgentMac.app." >&2
    fi

    # Replace any existing install, elevating only when the unprivileged
    # operation fails and announcing the escalation for auditability.
    if [[ -e "/Applications/WendyAgentMac.app" ]]; then
      rm -rf "/Applications/WendyAgentMac.app" 2>/dev/null || {
        echo "Elevated permissions required to replace /Applications/WendyAgentMac.app"
        $SUDO rm -rf "/Applications/WendyAgentMac.app"
      }
    fi
    cp -R "$APP_SRC" /Applications/ 2>/dev/null || {
      echo "Elevated permissions required to install into /Applications"
      $SUDO cp -R "$APP_SRC" /Applications/
    }
    echo "Installed /Applications/WendyAgentMac.app"
    open /Applications/WendyAgentMac.app || true
  fi

  echo ""
  echo "Wendy Agent for macOS installed. Look for it in the menu bar."
  echo "See https://docs.wendy.dev/latest/installation/wendy-agent-macos for setup."
  exit 0

elif [[ "$OS" != "linux" ]]; then
  echo "Error: The Wendy Agent runs on Linux and Apple Silicon macOS."
  echo "  Detected OS: $(uname -s)"
  exit 1
fi

if command -v apt-get &>/dev/null; then
  echo "APT detected. Will add the Wendy repository and install or update wendy-agent."
  confirm "Proceed?"

  echo "Adding Wendy APT repository..."
  # Ensure gnupg is available for key import
  $SUDO apt-get update -qq
  $SUDO apt-get install -y -qq ca-certificates curl gnupg >/dev/null
  # Import the Google Artifact Registry GPG key
  $SUDO mkdir -p /usr/share/keyrings
  curl -fsSL https://us-central1-apt.pkg.dev/doc/repo-signing-key.gpg \
    | $SUDO gpg --dearmor --yes -o /usr/share/keyrings/wendy-archive-keyring.gpg
  echo "deb [signed-by=/usr/share/keyrings/wendy-archive-keyring.gpg] https://us-central1-apt.pkg.dev/projects/cloud-c7e56 wendy-apt main" \
    | $SUDO tee /etc/apt/sources.list.d/wendy.list >/dev/null
  $SUDO apt-get update
  apt_install_or_upgrade wendy-agent

elif command -v dnf &>/dev/null; then
  echo "DNF detected. Will add the Wendy repository and install or update wendy-agent."
  confirm "Proceed?"

  echo "Adding Wendy YUM repository..."
  $SUDO tee /etc/yum.repos.d/wendy.repo >/dev/null <<'REPO'
[wendy]
name=Wendy Repository
baseurl=https://us-central1-yum.pkg.dev/projects/cloud-c7e56/wendy-yum
enabled=1
gpgcheck=0
REPO
  $SUDO dnf makecache --refresh
  dnf_install_or_upgrade wendy-agent

elif command -v yum &>/dev/null; then
  echo "YUM detected. Will add the Wendy repository and install or update wendy-agent."
  confirm "Proceed?"

  echo "Adding Wendy YUM repository..."
  $SUDO tee /etc/yum.repos.d/wendy.repo >/dev/null <<'REPO'
[wendy]
name=Wendy Repository
baseurl=https://us-central1-yum.pkg.dev/projects/cloud-c7e56/wendy-yum
enabled=1
gpgcheck=0
REPO
  $SUDO yum makecache
  yum_install_or_upgrade wendy-agent

elif command -v pacman &>/dev/null; then
  echo "Pacman detected. Will install wendy-agent from the AUR."
  confirm "Proceed?"

  # AUR helpers and makepkg refuse to run as root. If we're root, drop
  # privileges back to the invoking user via SUDO_USER.
  AS_USER=""
  if [[ "$(id -u)" -eq 0 ]]; then
    if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
      AS_USER="sudo -u $SUDO_USER"
    else
      echo "Error: AUR packages cannot be built as root."
      echo "  Please re-run this script as a normal user (with or without sudo)."
      exit 1
    fi
  fi

  if command -v yay &>/dev/null; then
    $AS_USER yay -S --noconfirm wendy-agent
  elif command -v paru &>/dev/null; then
    $AS_USER paru -S --noconfirm wendy-agent
  else
    echo "No AUR helper (yay/paru) found. Installing with makepkg..."
    $SUDO pacman -S --needed --noconfirm base-devel git
    TMPDIR_AUR=$(mktemp -d)
    trap 'rm -rf "$TMPDIR_AUR"' EXIT
    [[ -n "$AS_USER" ]] && chown "${SUDO_USER}:${SUDO_USER}" "$TMPDIR_AUR"
    $AS_USER git clone https://aur.archlinux.org/wendy-agent.git "$TMPDIR_AUR/wendy-agent"
    cd "$TMPDIR_AUR/wendy-agent"
    $AS_USER makepkg -si --noconfirm
  fi

else
  # No package manager — fall back to downloading the tarball from GitHub
  # and manually installing the binary, systemd services, and dev registry.
  TAG=$(resolve_version) || exit 1
  VERSION="${TAG#v}"

  ARTIFACT="wendy-agent-linux-${ARCH}-${VERSION}.tar.gz"
  URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
  echo "No package manager detected. Will download ${ARTIFACT}"
  echo "  and install '${BINARY_NAME}' with systemd services and dev container registry."
  confirm "Proceed?"

  TMPDIR_DL=$(mktemp -d)
  trap 'rm -rf "$TMPDIR_DL"' EXIT

  echo "Downloading ${URL}..."
  download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
  tar -xzf "${TMPDIR_DL}/${ARTIFACT}" -C "$TMPDIR_DL"
  $SUDO install -m 755 "${TMPDIR_DL}/wendy-agent-linux-${ARCH}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

  # --- Set up systemd services and supporting files ---
  if [[ -d /run/systemd/system ]] && command -v systemctl &>/dev/null; then
    echo "Setting up systemd services..."

    $SUDO mkdir -p /var/lib/wendy-agent/storage
    $SUDO mkdir -p /etc/wendy-agent
    $SUDO mkdir -p /usr/lib/systemd/system
    $SUDO mkdir -p /usr/share/wendyos/offline-images

    # wendy-agent systemd unit (unquoted heredoc so INSTALL_DIR is expanded)
    $SUDO tee /usr/lib/systemd/system/wendy-agent.service >/dev/null <<EOF
[Unit]
Description=Wendy Agent
After=network-online.target dbus.service containerd.service
Wants=network-online.target
Requires=containerd.service

[Service]
Type=simple
EnvironmentFile=-/etc/default/wendy-agent
WorkingDirectory=/var/lib/wendy-agent
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
Restart=always
RestartSec=2
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

    # Environment defaults
    if [[ ! -f /etc/default/wendy-agent ]]; then
      $SUDO mkdir -p /etc/default
      $SUDO tee /etc/default/wendy-agent >/dev/null <<'EOF'
# Environment overrides for wendy-agent.
WENDY_SYSTEMD_SERVICE_NAME=wendy-agent
# Network manager selection options:
# auto, connman, networkmanager, force-connman, force-networkmanager
# WENDY_NETWORK_MANAGER=auto
EOF
    fi

    # Placeholder config
    if [[ ! -f /etc/wendy-agent/config.json ]]; then
      printf "{}\n" | $SUDO tee /etc/wendy-agent/config.json >/dev/null
      $SUDO chmod 600 /etc/wendy-agent/config.json
    fi

    # --- Dev container registry ---
    REGISTRY_REPO="wendylabsinc/containerd-registry"
    REGISTRY_VERSION="v1.1.0"
    REGISTRY_ASSET="containerd-registry-${ARCH}.tar.gz"
    REGISTRY_URL="https://github.com/${REGISTRY_REPO}/releases/download/${REGISTRY_VERSION}/${REGISTRY_ASSET}"

    echo "Downloading dev container registry image..."
    if download "$REGISTRY_URL" "${TMPDIR_DL}/${REGISTRY_ASSET}"; then
      gunzip -f "${TMPDIR_DL}/${REGISTRY_ASSET}"
      $SUDO install -m 644 "${TMPDIR_DL}/containerd-registry-${ARCH}.tar" \
        /usr/share/wendyos/offline-images/containerd-registry.tar

      # Registry image import service (runs once on first boot)
      $SUDO tee /usr/lib/systemd/system/wendyos-dev-registry-import.service >/dev/null <<'EOF'
[Unit]
Description=WendyOS Dev Registry Image Import (First Boot)
Documentation=https://github.com/wendylabsinc/containerd-registry
After=containerd.service
Requires=containerd.service
ConditionPathExists=!/var/lib/wendyos/dev-registry-imported
ConditionPathExists=/usr/share/wendyos/offline-images/containerd-registry.tar

[Service]
Type=oneshot
ExecStartPre=/bin/mkdir -p /var/lib/wendyos
ExecStart=/usr/bin/ctr -n default images import /usr/share/wendyos/offline-images/containerd-registry.tar
ExecStartPost=/bin/sh -c '/usr/bin/ctr -n default images tag ghcr.io/wendylabsinc/containerd-registry:1.1.0 wendyos/containerd-registry:v1.1.0 || true'
ExecStartPost=/bin/sh -c '/usr/bin/ctr -n default images tag wendyos/containerd-registry:v1.1.0 wendyos/containerd-registry:latest || true'
ExecStartPost=/usr/bin/ctr -n default images label wendyos/containerd-registry:v1.1.0 containerd.io/gc.root=true
ExecStartPost=/bin/touch /var/lib/wendyos/dev-registry-imported
RemainAfterExit=yes
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

      # Registry container service
      $SUDO tee /usr/lib/systemd/system/wendyos-dev-registry.service >/dev/null <<'EOF'
[Unit]
Description=WendyOS Development Container Registry
Documentation=https://github.com/wendylabsinc/containerd-registry
After=containerd.service network-online.target wendyos-dev-registry-import.service
Requires=containerd.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=REGISTRY_NAMESPACE=default
Environment=REGISTRY_NAME=wendyos-dev-registry
Environment=REGISTRY_IMAGE=wendyos/containerd-registry:v1.1.0
Environment=LISTEN_ADDRESS=0.0.0.0:5000

ExecStartPre=/bin/sh -c '\
  if ! /usr/bin/ctr -n ${REGISTRY_NAMESPACE} images ls | /bin/grep -q ${REGISTRY_IMAGE}; then \
    echo "Registry image not found, importing from archive..."; \
    /usr/bin/ctr -n ${REGISTRY_NAMESPACE} images import /usr/share/wendyos/offline-images/containerd-registry.tar; \
    /usr/bin/ctr -n ${REGISTRY_NAMESPACE} images tag wendyos/containerd-registry:v1.1.0 wendyos/containerd-registry:latest || true; \
    echo "Registry image imported successfully"; \
  fi'
ExecStartPre=-/bin/sh -c '/usr/bin/ctr -n ${REGISTRY_NAMESPACE} tasks kill ${REGISTRY_NAME} 2>/dev/null; /usr/bin/ctr -n ${REGISTRY_NAMESPACE} tasks delete ${REGISTRY_NAME} 2>/dev/null; /usr/bin/ctr -n ${REGISTRY_NAMESPACE} containers delete ${REGISTRY_NAME} 2>/dev/null; true'

ExecStart=/usr/bin/ctr -n ${REGISTRY_NAMESPACE} run \
    --detach \
    --net-host \
    --mount type=bind,src=/run/containerd/containerd.sock,dst=/run/containerd/containerd.sock,options=rbind:rw \
    --env LISTEN_ADDRESS=${LISTEN_ADDRESS} \
    --env CONTAINERD_NAMESPACE=${REGISTRY_NAMESPACE} \
    --env LOG_FORMAT=json \
    ${REGISTRY_IMAGE} \
    ${REGISTRY_NAME}

ExecStop=/usr/bin/ctr -n ${REGISTRY_NAMESPACE} tasks kill ${REGISTRY_NAME}
ExecStopPost=-/bin/sh -c '/usr/bin/ctr -n ${REGISTRY_NAMESPACE} tasks delete ${REGISTRY_NAME} 2>/dev/null; /usr/bin/ctr -n ${REGISTRY_NAMESPACE} containers delete ${REGISTRY_NAME} 2>/dev/null; true'

TimeoutStartSec=60s
TimeoutStopSec=45s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=wendyos-dev-registry
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

      # Dev registry manager script
      $SUDO tee /usr/bin/wendyos-dev-registry >/dev/null <<'SCRIPT'
#!/bin/bash
# WendyOS Dev Registry Manager
set -e
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
NAMESPACE="default"
CONTAINER_NAME="wendyos-dev-registry"
IMAGE_NAME="wendyos/containerd-registry:latest"
LISTEN_ADDRESS="${LISTEN_ADDRESS:-0.0.0.0:5000}"
CTR="/usr/bin/ctr"
GREP="/bin/grep"

check_image_exists() {
    if ! $CTR -n "${NAMESPACE}" images ls | $GREP -q "${IMAGE_NAME}"; then
        echo "ERROR: Registry image '${IMAGE_NAME}' not found in namespace '${NAMESPACE}'"
        echo "Did the import service run? Check: systemctl status wendyos-dev-registry-import.service"
        exit 1
    fi
}

start_registry() {
    echo "Starting WendyOS dev registry..."
    check_image_exists
    if $CTR -n "${NAMESPACE}" tasks ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "Registry container is already running"; return 0
    fi
    if $CTR -n "${NAMESPACE}" containers ls | $GREP -q "${CONTAINER_NAME}"; then
        $CTR -n "${NAMESPACE}" tasks start -d "${CONTAINER_NAME}"
    else
        $CTR -n "${NAMESPACE}" run --detach --net-host \
            --mount type=bind,src=/run/containerd/containerd.sock,dst=/run/containerd/containerd.sock,options=rbind:rw \
            --env LISTEN_ADDRESS="${LISTEN_ADDRESS}" \
            --env CONTAINERD_NAMESPACE="${NAMESPACE}" \
            --env LOG_FORMAT=json \
            "${IMAGE_NAME}" "${CONTAINER_NAME}"
    fi
    echo "Dev registry started on ${LISTEN_ADDRESS}"
}

stop_registry() {
    echo "Stopping WendyOS dev registry..."
    if ! $CTR -n "${NAMESPACE}" tasks ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "Registry is not running"; return 0
    fi
    $CTR -n "${NAMESPACE}" tasks kill "${CONTAINER_NAME}" || true
    sleep 1
    $CTR -n "${NAMESPACE}" tasks delete "${CONTAINER_NAME}" || true
    echo "Dev registry stopped"
}

status_registry() {
    if $CTR -n "${NAMESPACE}" tasks ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "Registry is running"
        $CTR -n "${NAMESPACE}" tasks ls | $GREP "${CONTAINER_NAME}"
    else
        echo "Registry is not running"; return 1
    fi
}

COMMAND="${1:-}"
if [ -n "${2:-}" ]; then LISTEN_ADDRESS="$2"; fi
case "$COMMAND" in
    start)   start_registry ;;
    stop)    stop_registry ;;
    status)  status_registry ;;
    restart) stop_registry; sleep 1; start_registry ;;
    *)       echo "Usage: $(basename "$0") {start|stop|status|restart} [listen_address]"; exit 1 ;;
esac
SCRIPT
      $SUDO chmod 755 /usr/bin/wendyos-dev-registry

      echo "Dev container registry installed."
    else
      echo "Warning: Could not download dev container registry image."
      echo "  The dev registry will not be available for pushing apps."
      echo "  You can set it up later by installing the wendy-agent package."
    fi

    # Avahi mDNS advertisement
    if command -v avahi-daemon &>/dev/null; then
      $SUDO mkdir -p /etc/avahi/services
      $SUDO tee /etc/avahi/services/wendy-agent.service >/dev/null <<'EOF'
<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<service-group>
  <name replace-wildcards="yes">%h</name>
  <service protocol="any">
    <type>_wendyos._udp</type>
    <port>50051</port>
  </service>
</service-group>
EOF
    fi

    # Enable and start services (mirrors wendy-agent-postinstall.sh)
    $SUDO systemctl daemon-reload >/dev/null 2>&1 || true
    if systemctl is-enabled wendy-agent >/dev/null 2>&1; then
      $SUDO systemctl try-restart wendy-agent >/dev/null 2>&1 || true
    else
      $SUDO systemctl enable --now wendy-agent >/dev/null 2>&1 || true
    fi
    $SUDO systemctl enable wendyos-dev-registry-import >/dev/null 2>&1 || true
    $SUDO systemctl start wendyos-dev-registry-import >/dev/null 2>&1 || true
    if command -v avahi-daemon &>/dev/null; then
      $SUDO systemctl try-restart avahi-daemon >/dev/null 2>&1 || true
    fi

    echo "Systemd services configured and started."
  fi
fi

# --- Verify ---
echo ""
if command -v "$BINARY_NAME" &>/dev/null; then
  echo "Installed or updated successfully!"
else
  echo "Installed to ${INSTALL_DIR}/${BINARY_NAME}."
fi

stage_enrollment
