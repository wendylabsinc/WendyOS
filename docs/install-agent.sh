#!/usr/bin/env bash

# Re-exec under bash if invoked via sh (pipefail and [[ ]] require bash).
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi

set -euo pipefail

REPO="wendylabsinc/wendy-agent"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="wendy-agent"
YES=false

usage() {
  cat <<EOF
Install the Wendy Agent.

The agent runs on Linux devices and provides remote debugging and deployment
capabilities. It requires root privileges to install.

Usage: install-agent.sh [OPTIONS]

Options:
  -y            Skip confirmation prompt
  -d DIR        Install directory (default: /usr/local/bin, only for binary fallback)
  -h, --help    Show this help message

Environment:
  WENDY_VERSION   Install a specific version (e.g. v0.2.0) instead of latest
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

# --- Require Linux ---
if [[ "$(uname -s)" != "Linux" ]]; then
  echo "Error: The Wendy Agent only runs on Linux."
  echo "  Detected OS: $(uname -s)"
  exit 1
fi

# --- Require root ---
if [[ "$(id -u)" -ne 0 ]]; then
  echo "Error: This installer must be run as root."
  echo "  Try: sudo bash install-agent.sh"
  exit 1
fi

# --- Detect Architecture ---
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) echo "unsupported" ;;
  esac
}

# --- Resolve latest release tag ---
resolve_version() {
  if [[ -n "${WENDY_VERSION:-}" ]]; then
    echo "$WENDY_VERSION"
    return
  fi

  if command -v curl &>/dev/null; then
    curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
      | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
  elif command -v wget &>/dev/null; then
    wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" \
      | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
  else
    echo "Error: curl or wget is required" >&2
    exit 1
  fi
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

# --- Prompt for confirmation ---
confirm() {
  if [[ "$YES" == true ]]; then return 0; fi
  printf "%s [y/N] " "$1"
  read -r answer
  case "$answer" in
    [yY]|[yY][eE][sS]) return 0 ;;
    *) echo "Aborted."; exit 1 ;;
  esac
}

ARCH=$(detect_arch)

if [[ "$ARCH" == "unsupported" ]]; then
  echo "Error: Unsupported architecture: $(uname -m)"
  exit 1
fi

TAG=$(resolve_version)
if [[ -z "$TAG" ]]; then
  echo "Error: Could not determine latest version."
  exit 1
fi

# Strip leading 'v' for the version used in artifact filenames.
VERSION="${TAG#v}"

echo "Detected: Arch=${ARCH}"
echo "Version:  ${TAG}"
echo ""

TMPDIR_DL=$(mktemp -d)
trap 'rm -rf "$TMPDIR_DL"' EXIT

if command -v apt-get &>/dev/null; then
  ARTIFACT="wendy-agent_${VERSION}_${ARCH}.deb"
  URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
  echo "APT detected. Will install ${ARTIFACT} via dpkg."
  confirm "Proceed?"

  echo "Downloading ${URL}..."
  download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
  dpkg -i "${TMPDIR_DL}/${ARTIFACT}" || apt-get install -f -y

elif command -v dnf &>/dev/null; then
  ARTIFACT="wendy-agent-${VERSION}-1.$(uname -m).rpm"
  URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
  echo "DNF detected. Will install ${ARTIFACT}."
  confirm "Proceed?"

  echo "Downloading ${URL}..."
  download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
  dnf install -y "${TMPDIR_DL}/${ARTIFACT}"

elif command -v yum &>/dev/null; then
  ARTIFACT="wendy-agent-${VERSION}-1.$(uname -m).rpm"
  URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
  echo "YUM detected. Will install ${ARTIFACT}."
  confirm "Proceed?"

  echo "Downloading ${URL}..."
  download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
  yum install -y "${TMPDIR_DL}/${ARTIFACT}"

else
  ARTIFACT="wendy-agent-linux-${ARCH}-${VERSION}.tar.gz"
  URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
  echo "No package manager detected. Will download ${ARTIFACT}"
  echo "  and install '${BINARY_NAME}' to ${INSTALL_DIR}"
  confirm "Proceed?"

  echo "Downloading ${URL}..."
  download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
  tar -xzf "${TMPDIR_DL}/${ARTIFACT}" -C "$TMPDIR_DL"
  install -m 755 "${TMPDIR_DL}/wendy-agent-linux-${ARCH}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
fi

# --- Enable and start the service if systemd is available ---
if command -v systemctl &>/dev/null && systemctl is-system-running &>/dev/null 2>&1; then
  echo ""
  echo "Enabling and starting wendy-agent service..."
  systemctl daemon-reload
  systemctl enable --now wendy-agent || true
fi

# --- Verify ---
echo ""
if command -v "$BINARY_NAME" &>/dev/null; then
  echo "Installed successfully!"
  "$BINARY_NAME" --version
else
  echo "Installed to ${INSTALL_DIR}/${BINARY_NAME}."
fi
