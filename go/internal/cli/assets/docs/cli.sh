#!/usr/bin/env bash

# Re-exec under bash if invoked via sh or zsh (pipefail and [[ ]] require bash).
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi

set -euo pipefail

REPO="wendylabsinc/wendy-agent"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="wendy"
HOMEBREW_TAP="wendylabsinc/tap"
HOMEBREW_FORMULA="wendylabsinc/tap/wendy"
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

# --- Homebrew helper ---
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

trust_homebrew_formula() {
  local formula="$1"

  if ! homebrew_supports_trust; then
    return 0
  fi

  echo "Trusting Homebrew formula: ${formula}"
  if brew trust --formula "$formula"; then
    return 0
  fi

  echo "Error: Homebrew could not trust ${formula}." >&2
  echo "Run this command, then re-run the installer:" >&2
  echo "  brew trust --formula ${formula}" >&2
  exit 1
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

# --- Determine sudo prefix for Linux (macOS uses sudo selectively, Windows doesn't need it) ---
SUDO=""
if [[ "$OS" == "linux" && "$(id -u)" -ne 0 ]]; then
  SUDO="sudo"
fi

# resolve_and_set_version populates TAG and VERSION for the binary-download
# fallback paths only. The Homebrew and apt/dnf/yum/pacman paths install from
# package sources and never need a version, so they never call this.
resolve_and_set_version() {
  TAG=$(resolve_version) || exit 1
  if [[ -z "$TAG" ]]; then
    echo "Error: Could not determine latest version."
    exit 1
  fi
  VERSION="${TAG#v}"
}

echo "Detected: OS=${OS} Arch=${ARCH}"
echo ""

# ===== macOS =====
if [[ "$OS" == "darwin" ]]; then
  if [[ "$ARCH" != "arm64" ]]; then
    # On Apple Silicon running under Rosetta, uname -m reports x86_64; still install arm64.
    if [[ "$(sysctl -in hw.optional.arm64 2>/dev/null || echo 0)" == "1" ]]; then
      ARCH="arm64"
    else
      echo "Error: the Wendy CLI for macOS requires Apple Silicon (arm64)." >&2
      echo "Intel (x86_64) Macs are no longer supported." >&2
      exit 1
    fi
  fi
  if command -v brew &>/dev/null; then
    if homebrew_supports_trust; then
      echo "Homebrew detected. Will trust and install via:"
      echo "  brew trust ${HOMEBREW_TAP}"
      echo "  brew trust --formula ${HOMEBREW_FORMULA}"
      echo "  brew install ${HOMEBREW_FORMULA}"
    else
      echo "Homebrew detected. Will install via: brew install ${HOMEBREW_FORMULA}"
    fi
    confirm "Proceed?"
    trust_homebrew_tap "$HOMEBREW_TAP"
    trust_homebrew_formula "$HOMEBREW_FORMULA"
    brew install "$HOMEBREW_FORMULA"
  else
    resolve_and_set_version
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
    if [[ ! -d "$INSTALL_DIR" ]]; then
      mkdir -p "$INSTALL_DIR" 2>/dev/null || sudo mkdir -p "$INSTALL_DIR"
    fi
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

  elif command -v pacman &>/dev/null; then
    echo "Pacman detected. Will install wendy from the AUR."
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
      $AS_USER yay -S --noconfirm wendy
    elif command -v paru &>/dev/null; then
      $AS_USER paru -S --noconfirm wendy
    else
      echo "No AUR helper (yay/paru) found. Installing with makepkg..."
      $SUDO pacman -S --needed --noconfirm base-devel git
      TMPDIR_AUR=$(mktemp -d)
      trap 'rm -rf "$TMPDIR_AUR"' EXIT
      [[ -n "$AS_USER" ]] && chown "${SUDO_USER}:${SUDO_USER}" "$TMPDIR_AUR"
      $AS_USER git clone https://aur.archlinux.org/wendy.git "$TMPDIR_AUR/wendy"
      cd "$TMPDIR_AUR/wendy"
      $AS_USER makepkg -si --noconfirm
    fi

  else
    TMPDIR_DL=$(mktemp -d)
    trap 'rm -rf "$TMPDIR_DL"' EXIT

    resolve_and_set_version
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
  resolve_and_set_version
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

# --- Offer tour ---
if [[ "$YES" != true ]] && command -v "$BINARY_NAME" &>/dev/null && [[ -t 1 ]] && [[ -r /dev/tty ]] && [[ -w /dev/tty ]]; then
  printf "\nWould you like a quick guided tour of the Wendy CLI? [Y/n] "
  read -r tour_answer </dev/tty
  case "$tour_answer" in
    # The installer may be run as `curl ... | bash`, which leaves the script's
    # stdin attached to the download pipe. Reattach the tour to the controlling
    # terminal so Bubble Tea sees an interactive stdin and stdout.
    ""|[yY]|[yY][eE][sS]) "$BINARY_NAME" tour </dev/tty >/dev/tty ;;
  esac
else
  echo ""
  echo "Run '${INSTALL_DIR}/${BINARY_NAME} tour' at any time for a guided walkthrough."
fi
