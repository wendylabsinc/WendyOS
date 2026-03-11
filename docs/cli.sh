#!/usr/bin/env bash

# Re-exec under bash if invoked via sh or zsh (pipefail and [[ ]] require bash).
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi

set -euo pipefail

REPO="wendylabsinc/wendy-agent"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="wendy"
YES=false

usage() {
  cat <<EOF
Install the Wendy CLI.

Usage: install-cli.sh [OPTIONS]

Options:
  -y            Skip confirmation prompt
  -d DIR        Install directory (default: /usr/local/bin)
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

# --- Detect OS ---
detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
    *) echo "unsupported" ;;
  esac
}

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
  read -r answer </dev/tty
  case "$answer" in
    [yY]|[yY][eE][sS]) return 0 ;;
    *) echo "Aborted."; exit 1 ;;
  esac
}

OS=$(detect_os)
ARCH=$(detect_arch)

if [[ "$OS" == "unsupported" ]]; then
  echo "Error: Unsupported operating system: $(uname -s)"
  exit 1
fi
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

# --- Determine sudo prefix for Linux (macOS uses sudo selectively, Windows doesn't need it) ---
SUDO=""
if [[ "$OS" == "linux" && "$(id -u)" -ne 0 ]]; then
  SUDO="sudo"
fi

echo "Detected: OS=${OS} Arch=${ARCH}"
echo "Version:  ${TAG}"
echo ""

# ===== macOS =====
if [[ "$OS" == "darwin" ]]; then
  if command -v brew &>/dev/null; then
    echo "Homebrew detected. Will install via: brew install wendylabsinc/tap/wendy"
    confirm "Proceed?"
    brew install wendylabsinc/tap/wendy
  else
    ARTIFACT="wendy-cli-darwin-${ARCH}-${VERSION}.tar.gz"
    URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
    echo "Will download ${ARTIFACT}"
    echo "  and install '${BINARY_NAME}' to ${INSTALL_DIR}"
    confirm "Proceed?"

    TMPDIR_DL=$(mktemp -d)
    trap 'rm -rf "$TMPDIR_DL"' EXIT

    echo "Downloading ${URL}..."
    download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
    tar -xzf "${TMPDIR_DL}/${ARTIFACT}" -C "$TMPDIR_DL"
    sudo install -m 755 "${TMPDIR_DL}/wendy-cli-darwin-${ARCH}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
  fi

# ===== Linux =====
elif [[ "$OS" == "linux" ]]; then
  if command -v apt-get &>/dev/null; then
    echo "APT detected. Will add the Wendy repository and install wendy."
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
    $SUDO apt-get install -y wendy

  elif command -v dnf &>/dev/null; then
    echo "DNF detected. Will add the Wendy repository and install wendy."
    confirm "Proceed?"

    echo "Adding Wendy YUM repository..."
    $SUDO tee /etc/yum.repos.d/wendy.repo >/dev/null <<'REPO'
[wendy]
name=Wendy Repository
baseurl=https://us-central1-yum.pkg.dev/projects/cloud-c7e56/wendy-yum
enabled=1
gpgcheck=0
REPO
    $SUDO dnf makecache
    $SUDO dnf install -y wendy

  elif command -v yum &>/dev/null; then
    echo "YUM detected. Will add the Wendy repository and install wendy."
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
    $SUDO yum install -y wendy

  else
    TMPDIR_DL=$(mktemp -d)
    trap 'rm -rf "$TMPDIR_DL"' EXIT

    ARTIFACT="wendy-cli-linux-${ARCH}-${VERSION}.tar.gz"
    URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
    echo "Will download ${ARTIFACT}"
    echo "  and install '${BINARY_NAME}' to ${INSTALL_DIR}"
    confirm "Proceed?"

    echo "Downloading ${URL}..."
    download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
    tar -xzf "${TMPDIR_DL}/${ARTIFACT}" -C "$TMPDIR_DL"
    $SUDO install -m 755 "${TMPDIR_DL}/wendy-cli-linux-${ARCH}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
  fi

# ===== Windows (Git Bash / MSYS2) =====
elif [[ "$OS" == "windows" ]]; then
  ARTIFACT="wendy-cli-windows-${ARCH}-${VERSION}.zip"
  URL="https://github.com/${REPO}/releases/download/${TAG}/${ARTIFACT}"
  INSTALL_DIR="${INSTALL_DIR:-$HOME/bin}"

  echo "Will download ${ARTIFACT}"
  echo "  and extract to ${INSTALL_DIR}"
  confirm "Proceed?"

  TMPDIR_DL=$(mktemp -d)
  trap 'rm -rf "$TMPDIR_DL"' EXIT

  echo "Downloading ${URL}..."
  download "$URL" "${TMPDIR_DL}/${ARTIFACT}"
  mkdir -p "$INSTALL_DIR"
  unzip -o "${TMPDIR_DL}/${ARTIFACT}" -d "$TMPDIR_DL"
  cp "${TMPDIR_DL}/wendy-cli-windows-${ARCH}/${BINARY_NAME}.exe" "${INSTALL_DIR}/${BINARY_NAME}.exe"

  echo ""
  echo "Installed to ${INSTALL_DIR}/${BINARY_NAME}.exe"
  if [[ ":$PATH:" != *":${INSTALL_DIR}:"* ]]; then
    echo "NOTE: Add ${INSTALL_DIR} to your PATH to use '${BINARY_NAME}' from anywhere."
  fi
  exit 0
fi

# --- Verify ---
echo ""
if command -v "$BINARY_NAME" &>/dev/null; then
  echo "Installed successfully!"
  "$BINARY_NAME" --version
else
  echo "Installed to ${INSTALL_DIR}/${BINARY_NAME}."
  echo "Make sure ${INSTALL_DIR} is in your PATH."
fi
