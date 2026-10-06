#!/usr/bin/env bash

# Re-exec under bash if invoked via sh or zsh (pipefail and [[ ]] require bash).
# Read from a pipe (`curl … | sh`, where sh is dash on Debian/Ubuntu) there is
# no file to re-exec — $0 is just the shell's name — so say how to run it.
if [ -z "${BASH_VERSION:-}" ]; then
  if [ -f "$0" ]; then
    exec bash "$0" "$@"
  fi
  echo "This installer needs bash: curl -fsSL https://install.wendy.dev/cli.sh | bash" >&2
  exit 1
fi

set -euo pipefail

REPO="wendylabsinc/wendy-agent"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="wendy"
HOMEBREW_TAP="wendylabsinc/tap"
HOMEBREW_FORMULA="wendylabsinc/tap/wendy"
YES=false
INSTALL_DIR_EXPLICIT=false
INSTALLED_BIN=""  # the binary this run installed, when known (see Verify)
INSTALLED_BY_PKG=false # true when a package manager installed it

usage() {
  cat <<EOF
Install the Wendy CLI.

Usage: install-cli.sh [OPTIONS]

Options:
  -y            Skip confirmation prompt (assumed when no terminal is attached)
  -d DIR        Install directory. Default: /usr/local/bin, or ~/.local/bin
                when that isn't writable and sudo can't be used (not installed,
                or no terminal to type its password into). An explicit -d DIR
                is never relocated.
  -h, --help    Show this help message

Environment:
  WENDY_VERSION   Install a specific version (e.g. v0.2.0) instead of latest
EOF
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -y) YES=true; shift ;;
    -d) INSTALL_DIR="$2"; INSTALL_DIR_EXPLICIT=true; shift 2 ;;
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
MANIFEST_URL="https://install.wendy.dev/manifest.json"

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

# have_tty reports whether a controlling terminal can actually be opened.
# `[[ -r /dev/tty ]]` is not enough: in agent shells and CI the node exists and
# passes the permission check, but open(2) fails with ENXIO ("Device not
# configured"), which aborts a `read </dev/tty` under `set -e`. The probe runs
# in a subshell: where sh is bash (Fedora, macOS), `curl … | sh` runs this in
# POSIX mode, and there a failed redirection on a special builtin such as `:`
# or `exec` exits the shell. Only the subshell exits.
have_tty() {
  (exec </dev/tty) 2>/dev/null
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
  if ! have_tty; then
    echo "No interactive terminal; continuing as if -y was passed."
    YES=true
    return 0
  fi
  printf "%s [y/N] " "$1"
  read -r answer </dev/tty
  case "$answer" in
    [yY]|[yY][eE][sS]) return 0 ;;
    *) echo "Aborted."; exit 1 ;;
  esac
}

# can_elevate reports whether root-level steps can run without failing on a
# password prompt nobody can answer: already root, a terminal sudo can prompt
# on, or sudo that needs no password (cached credentials or NOPASSWD).
can_elevate() {
  [[ "$(id -u)" -eq 0 ]] && return 0
  command -v sudo &>/dev/null || return 1
  have_tty && return 0
  sudo -n true 2>/dev/null
}

# install_binary SRC installs SRC as ${INSTALL_DIR}/${BINARY_NAME}, using sudo
# only when INSTALL_DIR isn't writable. When sudo can't be used here (no
# terminal to type its password into, or no sudo at all), the default install
# dir falls back to ~/.local/bin, updating INSTALL_DIR so the summary below
# points at the real location; a dir the user chose with -d never moves, so
# that fails with one line instead.
install_binary() {
  local src="$1" no_sudo="sudo is unavailable or needs a password that can't be entered here"
  if mkdir -p "$INSTALL_DIR" 2>/dev/null && [[ -w "$INSTALL_DIR" ]]; then
    install -m 755 "$src" "${INSTALL_DIR}/${BINARY_NAME}"
  elif can_elevate; then
    sudo mkdir -p "$INSTALL_DIR"
    sudo install -m 755 "$src" "${INSTALL_DIR}/${BINARY_NAME}"
  elif [[ "$INSTALL_DIR_EXPLICIT" == true ]]; then
    echo "Error: ${INSTALL_DIR} is not writable, and ${no_sudo}. Re-run with -d <writable dir>." >&2
    exit 1
  elif [[ -z "${HOME:-}" ]]; then
    echo "Error: ${INSTALL_DIR} is not writable, ${no_sudo}, and HOME is unset. Re-run with -d <writable dir>." >&2
    exit 1
  else
    local fallback="${HOME}/.local/bin"
    echo "${INSTALL_DIR} is not writable and ${no_sudo}; installing to ${fallback} instead."
    INSTALL_DIR="$fallback"
    mkdir -p "$INSTALL_DIR"
    install -m 755 "$src" "${INSTALL_DIR}/${BINARY_NAME}"
  fi
  INSTALLED_BIN="${INSTALL_DIR}/${BINARY_NAME}"
}

# package_bin MANAGER prints where MANAGER (brew, apt, rpm, pacman) put the
# wendy binary it just installed, or nothing if it can't tell.
package_bin() {
  local p=""
  case "$1" in
    brew) p="$(brew --prefix 2>/dev/null || true)"; [[ -n "$p" ]] && p="${p}/bin/${BINARY_NAME}" ;;
    apt) p="$(dpkg -L "$BINARY_NAME" 2>/dev/null | grep "/bin/${BINARY_NAME}\$" | head -1 || true)" ;;
    rpm) p="$(rpm -ql "$BINARY_NAME" 2>/dev/null | grep "/bin/${BINARY_NAME}\$" | head -1 || true)" ;;
    pacman) p="$(pacman -Qlq "$BINARY_NAME" 2>/dev/null | grep "/bin/${BINARY_NAME}\$" | head -1 || true)" ;;
  esac
  if [[ -n "$p" && -x "$p" ]]; then echo "$p"; fi
}

# package_installed MANAGER records the binary MANAGER just installed, so the
# summary reports that one rather than whichever wendy is first on PATH.
package_installed() {
  INSTALLED_BIN="$(package_bin "$1")"
  INSTALLED_BY_PKG=true
}

# package_upgrade_hint FILE prints, when a package manager owns FILE, which
# package it belongs to and how to upgrade it there; otherwise nothing. FILE
# must be absolute: given anything else, `dpkg-query -S` does a substring
# search and would claim some other package's file.
package_upgrade_hint() {
  local f="$1" pkg="" link=""
  [[ "$f" == /* ]] || return 0
  if command -v dpkg-query &>/dev/null && pkg="$(dpkg-query -S "$f" 2>/dev/null)"; then
    pkg="${pkg%%$'\n'*}"; pkg="${pkg%%: *}"; pkg="${pkg%%,*}"
    echo "It belongs to the apt package ${pkg}; upgrade it: sudo apt-get update && sudo apt-get install --only-upgrade ${pkg}"
  elif command -v rpm &>/dev/null && pkg="$(rpm -qf --qf '%{NAME}\n' "$f" 2>/dev/null)"; then
    pkg="${pkg%%$'\n'*}"
    if command -v dnf &>/dev/null; then
      echo "It belongs to the rpm package ${pkg}; upgrade it: sudo dnf upgrade ${pkg}"
    else
      echo "It belongs to the rpm package ${pkg}; upgrade it: sudo yum update ${pkg}"
    fi
  elif command -v pacman &>/dev/null && pkg="$(pacman -Qqo "$f" 2>/dev/null)"; then
    echo "It belongs to the pacman package ${pkg}; upgrade it with your AUR helper, e.g.: yay -S ${pkg}"
  elif link="$(readlink "$f" 2>/dev/null)" && [[ "$link" == *"/Cellar/"* ]]; then
    pkg="${link#*/Cellar/}"; pkg="${pkg%%/*}"
    echo "It belongs to the Homebrew formula ${pkg}; upgrade it: brew upgrade ${pkg}"
  fi
}

# print_version BIN prints BIN's version. Its stdin is /dev/null because
# under `curl … | bash` stdin is the rest of this script. A binary that can't
# run here (a noexec mount, the wrong architecture) gets a warning rather than
# aborting the summary under set -e.
print_version() {
  if ! "$1" --version </dev/null; then
    echo "Warning: '$1 --version' failed; ${BINARY_NAME} may not be able to run on this system."
  fi
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
    package_installed brew
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
    install_binary "${TMPDIR_DL}/wendy-cli-darwin-${ARCH}/${BINARY_NAME}"
  fi

# ===== Linux =====
elif [[ "$OS" == "linux" ]]; then
  # Package-manager installs need root. With no way to elevate (no terminal to
  # type a sudo password into, e.g. an agent shell or CI without NOPASSWD),
  # skip them and fall through to the standalone binary below.
  PKG_OK=true
  can_elevate || PKG_OK=false
  pkg_manager() { [[ "$PKG_OK" == true ]] && command -v "$1" &>/dev/null; }

  if pkg_manager apt-get; then
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
    package_installed apt

  elif pkg_manager dnf; then
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
    package_installed rpm

  elif pkg_manager yum; then
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
    package_installed rpm

  elif pkg_manager pacman; then
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
    package_installed pacman

  else
    if [[ "$PKG_OK" != true ]]; then
      echo "sudo is unavailable or needs a password that can't be entered here; installing the standalone binary instead of using a system package manager."
    fi
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
    install_binary "${TMPDIR_DL}/wendy-cli-linux-${ARCH}/${BINARY_NAME}"
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
if [[ -n "$INSTALLED_BIN" ]]; then
  # Report the binary this run installed, and say so when another `wendy`
  # earlier on PATH (an older install, e.g. a standalone ~/.local/bin/wendy
  # left by a no-terminal install) would run instead.
  INSTALLED_DIR="$(dirname "$INSTALLED_BIN")"
  ON_PATH="$(command -v "$BINARY_NAME" 2>/dev/null || true)"
  if [[ -n "$ON_PATH" && "$ON_PATH" != /* ]]; then
    # Found through a relative PATH entry: name it by its absolute path.
    ON_PATH="$(cd "$(dirname "$ON_PATH")" && pwd)/$(basename "$ON_PATH")"
  fi
  if [[ -n "$ON_PATH" && "$ON_PATH" -ef "$INSTALLED_BIN" ]]; then
    echo "Installed successfully!"
    print_version "$INSTALLED_BIN"
  else
    echo "Installed to ${INSTALLED_BIN}."
    print_version "$INSTALLED_BIN"
    if [[ -n "$ON_PATH" ]]; then
      echo "Warning: '${BINARY_NAME}' on your PATH is ${ON_PATH}, which runs instead of the version just installed."
      UPGRADE_HINT="$(package_upgrade_hint "$ON_PATH" || true)"
      if [[ -n "$UPGRADE_HINT" ]]; then
        echo "  ${UPGRADE_HINT}"
      else
        echo "  To remove it: rm ${ON_PATH}"
      fi
      # A package manager's bin dir is shared with other tools; don't suggest
      # moving it ahead of everything else.
      if [[ "$INSTALLED_BY_PKG" != true ]]; then
        echo "Or put the new one first on your PATH: export PATH=\"${INSTALLED_DIR}:\$PATH\""
      fi
    else
      echo "Add it to your PATH: export PATH=\"${INSTALLED_DIR}:\$PATH\""
    fi
  fi
elif command -v "$BINARY_NAME" &>/dev/null; then
  echo "Installed successfully!"
  print_version "$BINARY_NAME"
else
  echo "Installed to ${INSTALL_DIR}/${BINARY_NAME}."
  echo "Add it to your PATH: export PATH=\"${INSTALL_DIR}:\$PATH\""
fi

# --- Offer tour ---
# The tour runs the binary this run installed, not whichever is first on PATH.
WENDY_CMD="${INSTALLED_BIN:-$BINARY_NAME}"
if [[ "$YES" != true ]] && command -v "$WENDY_CMD" &>/dev/null && [[ -t 1 ]] && have_tty; then
  printf "\nWould you like a quick guided tour of the Wendy CLI? [Y/n] "
  read -r tour_answer </dev/tty
  case "$tour_answer" in
    # The installer may be run as `curl ... | bash`, which leaves the script's
    # stdin attached to the download pipe. Reattach the tour to the controlling
    # terminal so Bubble Tea sees an interactive stdin and stdout.
    ""|[yY]|[yY][eE][sS]) "$WENDY_CMD" tour </dev/tty >/dev/tty ;;
  esac
elif have_tty; then
  # Only suggest the tour where it can run: it needs an interactive terminal.
  echo ""
  echo "Run '${WENDY_CMD} tour' at any time for a guided walkthrough."
fi
