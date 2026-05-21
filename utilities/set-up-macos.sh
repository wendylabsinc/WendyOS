#!/usr/bin/env bash
set -Eeuo pipefail

TRACE_COMMANDS="${TRACE_COMMANDS:-0}"
readonly WENDY_RAW_BASE="${WENDY_RAW_BASE:-https://raw.githubusercontent.com/wendylabsinc/wendy-agent/main}"
readonly WENDY_REPO_URL="${WENDY_REPO_URL:-https://github.com/wendylabsinc/wendy-agent.git}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly SCRIPT_DIR REPO_ROOT

CURRENT_USER="$(id -un)"
USER_HOME="${HOME}"
SUDOERS_DIR="/private/etc/sudoers.d"
SUDOERS_FILE="${SUDOERS_DIR}/90-${CURRENT_USER}-passwordless"
readonly CURRENT_USER USER_HOME SUDOERS_DIR SUDOERS_FILE

XCODE_APP_PATH=""
GIT_NAME=""
GIT_EMAIL=""
CONFIGURE_GIT=0
SETUP_PASSWORDLESS_SUDO=0
CONFIGURE_LOOPBACK_SSH=0
INSTALL_WENDY_CLI=0
INSTALL_WENDY_AGENT=0
SETUP_GITHUB_RUNNER=0
GITHUB_RUNNER_DIR="${USER_HOME}/.github/actions-runner"
GITHUB_RUNNER_RUN_MODE="manual"
INSTALL_SWIFT_TOOLCHAIN=1
INSTALL_DIRENV=0
CLONE_REPOSITORY=0
CLONE_DESTINATION=""
CONFIGURE_POWER_SETTINGS=0
SHOW_XCODE_MANUAL_STEP=1
SHOW_MAC_NAME_MANUAL_STEP=0
SHOW_REMOTE_LOGIN_MANUAL_STEP=0
SHOW_SCREEN_SHARING_MANUAL_STEP=0
SHOW_SCREEN_LOCK_MANUAL_STEP=0
SHOW_AUTO_LOGIN_MANUAL_STEP=0
WALK_THROUGH_MANUAL_STEPS=0
BREW=""
AUTHORIZED_LOGIN_KEYS=()
PS4='+ ${BASH_SOURCE##*/}:${LINENO}: '

if [[ -t 1 ]]; then
  STYLE_BOLD=$'\033[1m'
  STYLE_RESET=$'\033[0m'
else
  STYLE_BOLD=""
  STYLE_RESET=""
fi
readonly STYLE_BOLD STYLE_RESET

bold() { printf '%s%s%s\n' "$STYLE_BOLD" "$*" "$STYLE_RESET"; }
info() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m✓ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m! %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mError: %s\033[0m\n' "$*" >&2; exit 1; }

on_error() {
  local exit_code=$?
  local line_no="${1:-unknown}"
  set +x 2>/dev/null || true
  fail "setup failed at line ${line_no} with exit code ${exit_code}"
}
trap 'on_error "$LINENO"' ERR

enable_command_trace() {
  [[ "$TRACE_COMMANDS" == "0" ]] && return 0
  set -x
}

run_sudo() {
  sudo "$@"
}

ask_yes_no() {
  local prompt="$1" default="${2:-n}" answer suffix

  case "$default" in
    y|Y|yes|YES) suffix="[Y/n]"; default="y" ;;
    *) suffix="[y/N]"; default="n" ;;
  esac

  while true; do
    printf '%s %s ' "$prompt" "$suffix"
    read -r answer
    answer="${answer:-$default}"
    case "$answer" in
      y|Y|yes|YES) return 0 ;;
      n|N|no|NO) return 1 ;;
      *) warn "Please answer yes or no." ;;
    esac
  done
}

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Options:
  -v, --verbose   Show each command before it runs
  -h, --help      Show this help message

Environment:
  TRACE_COMMANDS=1 also enables command tracing.
EOF
}

parse_arguments() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -v|--verbose)
        TRACE_COMMANDS=1
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        fail "Unknown option: $1"
        ;;
    esac
  done
}

append_line_if_missing() {
  local file="$1" line="$2"
  touch "$file"
  grep -qxF "$line" "$file" || printf '\n%s\n' "$line" >> "$file"
}

require_macos() {
  [[ "$(uname -s)" == "Darwin" ]] || fail "This script is intended for macOS."
  [[ "$CURRENT_USER" != "root" ]] || fail "Run this as the normal macOS user, not directly as root."
  [[ -n "$USER_HOME" && -d "$USER_HOME" ]] || fail "Could not determine home directory for ${CURRENT_USER}."
}

collect_configuration() {
  cat <<EOF
$(bold "Fresh macOS setup")

This script is idempotent: it is safe to run repeatedly. Existing packages,
keys, SSH settings, PATH entries, and git settings will be reused or updated
without creating duplicates. Bash xtrace is enabled after password collection so
you can see what is being called; password-specific calls are redacted.
EOF

  printf '\nConfigure global git identity? [y/N] '
  local answer
  read -r answer
  case "${answer:-n}" in
    y|Y|yes|YES)
      printf 'Git user.name (leave empty to skip git configuration): '
      read -r GIT_NAME
      if [[ -z "$GIT_NAME" ]]; then
        CONFIGURE_GIT=0
      else
        printf 'Git user.email (leave empty to skip git configuration): '
        read -r GIT_EMAIL
        if [[ -z "$GIT_EMAIL" ]]; then
          CONFIGURE_GIT=0
          GIT_NAME=""
        else
          CONFIGURE_GIT=1
        fi
      fi
      ;;
    *)
      CONFIGURE_GIT=0
      ;;
  esac

  if ask_yes_no "Install additional SSH public keys into ${CURRENT_USER}'s authorized_keys?" "n"; then
    printf 'Paste one public key per prompt. Leave empty when done.\n'
    local key
    while true; do
      printf 'SSH public key %d: ' "$(( ${#AUTHORIZED_LOGIN_KEYS[@]} + 1 ))"
      read -r key
      [[ -n "$key" ]] || break
      AUTHORIZED_LOGIN_KEYS+=("$key")
    done
  fi

  if ask_yes_no "Enable passwordless sudo for ${CURRENT_USER}?" "n"; then
    SETUP_PASSWORDLESS_SUDO=1
  else
    SETUP_PASSWORDLESS_SUDO=0
  fi

  if ask_yes_no "Enable passwordless loopback SSH for local automation?" "n"; then
    CONFIGURE_LOOPBACK_SSH=1
  else
    CONFIGURE_LOOPBACK_SSH=0
  fi

  if ask_yes_no "Install and configure direnv for repository-local developer tooling?" "n"; then
    INSTALL_DIRENV=1
  else
    INSTALL_DIRENV=0
  fi

  if [[ -d "$REPO_ROOT/.git" ]]; then
    CLONE_REPOSITORY=0
  elif ask_yes_no "Clone the Wendy repository onto this Mac?" "n"; then
    CLONE_REPOSITORY=1
    local default_clone_destination="${USER_HOME}/Projects/WendyLabs/wendy-agent"
    printf 'Clone destination [%s]: ' "$default_clone_destination"
    read -r CLONE_DESTINATION
    CLONE_DESTINATION="${CLONE_DESTINATION:-$default_clone_destination}"
  else
    CLONE_REPOSITORY=0
  fi

  if ask_yes_no "Install the Swift toolchain requested by .swift-version using Homebrew swiftly?" "y"; then
    INSTALL_SWIFT_TOOLCHAIN=1
  else
    INSTALL_SWIFT_TOOLCHAIN=0
  fi

  if ask_yes_no "Install or update the Wendy CLI using Homebrew?" "n"; then
    INSTALL_WENDY_CLI=1
  else
    INSTALL_WENDY_CLI=0
  fi

  if ask_yes_no "Install or update the Wendy macOS agent app using Homebrew?" "n"; then
    INSTALL_WENDY_AGENT=1
  else
    INSTALL_WENDY_AGENT=0
  fi

  if ask_yes_no "Install GitHub Actions self-hosted runner?" "n"; then
    SETUP_GITHUB_RUNNER=1
    local default_runner_dir="~/.github/actions-runner/"
    printf 'GitHub runner install location [%s]: ' "$default_runner_dir"
    read -r GITHUB_RUNNER_DIR
    GITHUB_RUNNER_DIR="${GITHUB_RUNNER_DIR:-$default_runner_dir}"
    GITHUB_RUNNER_DIR="${GITHUB_RUNNER_DIR/#\~/$USER_HOME}"

    while true; do
      printf 'Run GitHub runner in the user login session or manual only? [manual/login] '
      read -r GITHUB_RUNNER_RUN_MODE
      GITHUB_RUNNER_RUN_MODE="${GITHUB_RUNNER_RUN_MODE:-manual}"
      case "$GITHUB_RUNNER_RUN_MODE" in
        login|session|user) GITHUB_RUNNER_RUN_MODE="login"; break ;;
        manual|nothing|none) GITHUB_RUNNER_RUN_MODE="manual"; break ;;
        service|daemon|headless) warn "macOS GitHub runners must run in a logged-in user session for TCC/privacy permissions. Choose login or manual." ;;
        *) warn "Please answer login or manual." ;;
      esac
    done
  else
    SETUP_GITHUB_RUNNER=0
  fi

  if ask_yes_no "Show Xcode first-run manual step?" "y"; then
    SHOW_XCODE_MANUAL_STEP=1
  else
    SHOW_XCODE_MANUAL_STEP=0
  fi

  if ask_yes_no "Show manual step to review Mac name and local hostname?" "n"; then
    SHOW_MAC_NAME_MANUAL_STEP=1
  else
    SHOW_MAC_NAME_MANUAL_STEP=0
  fi

  if ask_yes_no "Show manual step to enable Remote Login?" "n"; then
    SHOW_REMOTE_LOGIN_MANUAL_STEP=1
  else
    SHOW_REMOTE_LOGIN_MANUAL_STEP=0
  fi

  if ask_yes_no "Show manual step to enable Screen Sharing?" "n"; then
    SHOW_SCREEN_SHARING_MANUAL_STEP=1
  else
    SHOW_SCREEN_SHARING_MANUAL_STEP=0
  fi

  if ask_yes_no "Disable macOS sleep on AC for unattended use?" "n"; then
    CONFIGURE_POWER_SETTINGS=1
  else
    CONFIGURE_POWER_SETTINGS=0
  fi

  if ask_yes_no "Show manual step to disable screen locking?" "n"; then
    SHOW_SCREEN_LOCK_MANUAL_STEP=1
  else
    SHOW_SCREEN_LOCK_MANUAL_STEP=0
  fi

  if ask_yes_no "Show manual step to enable automatic desktop login?" "n"; then
    SHOW_AUTO_LOGIN_MANUAL_STEP=1
  else
    SHOW_AUTO_LOGIN_MANUAL_STEP=0
  fi

  if ask_yes_no "Walk through selected manual macOS setup steps interactively at the end?" "n"; then
    WALK_THROUGH_MANUAL_STEPS=1
  else
    WALK_THROUGH_MANUAL_STEPS=0
  fi
}

confirm_plan() {
  local passwordless_sudo_summary git_summary ssh_key_summary swift_summary direnv_summary
  local loopback_ssh_summary clone_summary wendy_cli_summary wendy_agent_summary github_runner_summary power_settings_summary manual_steps_summary

  if (( SETUP_PASSWORDLESS_SUDO )); then
    passwordless_sudo_summary="Passwordless sudo will be enabled for ${CURRENT_USER}"
  else
    passwordless_sudo_summary="Passwordless sudo will not be changed"
  fi

  if (( CONFIGURE_GIT )); then
    git_summary="Global git user.name (${GIT_NAME}) and user.email (${GIT_EMAIL}) for ${CURRENT_USER}"
  else
    git_summary="Global git identity will not be changed"
  fi

  if (( CONFIGURE_LOOPBACK_SSH )); then
    loopback_ssh_summary="Generated SSH key will be authorized for passwordless loopback SSH"
  else
    loopback_ssh_summary="Passwordless loopback SSH will not be configured"
  fi

  if (( INSTALL_DIRENV )); then
    direnv_summary="direnv will be installed and shell hooks will be configured"
  else
    direnv_summary="direnv will not be installed or configured"
  fi

  if (( INSTALL_SWIFT_TOOLCHAIN )); then
    swift_summary="Swift will be installed using Homebrew swiftly and ${REPO_ROOT}/.swift-version when available"
  else
    swift_summary="Swift toolchain installation will be skipped"
  fi

  if (( CLONE_REPOSITORY )); then
    clone_summary="${WENDY_REPO_URL} will be cloned to ${CLONE_DESTINATION}"
  else
    clone_summary="Wendy repository will not be cloned"
  fi

  if (( INSTALL_WENDY_CLI )); then
    wendy_cli_summary="Wendy CLI will be installed or updated with Homebrew"
  else
    wendy_cli_summary="Wendy CLI will not be installed"
  fi

  if (( INSTALL_WENDY_AGENT )); then
    wendy_agent_summary="Wendy macOS agent app will be installed or updated with Homebrew Cask"
  else
    wendy_agent_summary="Wendy macOS agent app will not be installed"
  fi

  if (( SETUP_GITHUB_RUNNER )); then
    case "$GITHUB_RUNNER_RUN_MODE" in
      login) github_runner_summary="GitHub Actions runner will be installed at ${GITHUB_RUNNER_DIR} and set to run in the user login session after registration" ;;
      *) github_runner_summary="GitHub Actions runner will be installed at ${GITHUB_RUNNER_DIR} for manual runs in a logged-in user session" ;;
    esac
  else
    github_runner_summary="GitHub Actions runner will not be installed"
  fi

  if (( CONFIGURE_POWER_SETTINGS )); then
    power_settings_summary="AC sleep will be disabled with pmset; display sleep will be set to 10 minutes"
  else
    power_settings_summary="AC sleep settings will not be changed"
  fi

  if has_macos_manual_steps; then
    if (( WALK_THROUGH_MANUAL_STEPS )); then
      manual_steps_summary="Selected manual macOS steps will be shown one at a time with confirmation prompts"
    else
      manual_steps_summary="Selected manual macOS steps will be printed at the end"
    fi
  else
    manual_steps_summary="No manual macOS steps were selected"
  fi

  if (( ${#AUTHORIZED_LOGIN_KEYS[@]} )); then
    ssh_key_summary="${#AUTHORIZED_LOGIN_KEYS[@]} additional authorized SSH public key(s) for ${CURRENT_USER}"
  else
    ssh_key_summary="No additional authorized SSH public keys"
  fi

  cat <<EOF

This script will configure this Mac by doing the following:

  • Install developer tools and Homebrew packages:
      Xcode app is required and will be selected for command line builds.
      Homebrew packages: git, curl, go, Neovim, swiftly, Claude Code, and Codex.
      ${direnv_summary}
      ${swift_summary}
      ${clone_summary}
      ${wendy_cli_summary}
      ${wendy_agent_summary}
      ${github_runner_summary}

  • Configure:
      SSH key generation for ${CURRENT_USER}
      ${ssh_key_summary}
      ${loopback_ssh_summary}
      Neovim as the default CLI editor
      ${direnv_summary}
      ${passwordless_sudo_summary}
      Bonjour/mDNS local hostname discovery
      ${power_settings_summary}
      ${manual_steps_summary}
      ${git_summary}

sudo and other tools may ask for credentials when they need elevated access.
Homebrew is installed automatically if it is not already available.
EOF

  printf '\nContinue? [y/N] '
  local answer
  read -r answer
  case "$answer" in
    y|Y|yes|YES) ;;
    *) echo "Aborted."; exit 0 ;;
  esac
}

find_xcode_app() {
  local app
  while IFS= read -r app; do
    [[ -d "$app" ]] || continue
    [[ -x "$app/Contents/Developer/usr/bin/xcodebuild" ]] || continue
    printf '%s\n' "$app"
    return 0
  done < <(find /Applications -maxdepth 1 -type d -name 'Xcode*.app' -print 2>/dev/null | sort -r)

  return 1
}

ensure_xcode_app() {
  if XCODE_APP_PATH="$(find_xcode_app)"; then
    ok "Xcode app found at ${XCODE_APP_PATH}"
    return 0
  fi

  fail "Xcode.app was not found in /Applications. Download Xcode from https://xcodereleases.com/, install it into /Applications, name it Xcode-x.y.z.app (for example, Xcode-16.2.app), then rerun this script."
}

download_xcode_metal_tooling() {
  [[ -n "$XCODE_APP_PATH" ]] || XCODE_APP_PATH="$(find_xcode_app || true)"
  if [[ -z "$XCODE_APP_PATH" ]]; then
    warn "Xcode app was not found; skipping Metal tooling download."
    return 0
  fi

  info "Downloading Metal tooling for Xcode"
  run_sudo xcode-select -s "$XCODE_APP_PATH/Contents/Developer" || true
  DEVELOPER_DIR="$XCODE_APP_PATH/Contents/Developer" \
    xcodebuild -downloadComponent MetalToolchain || warn "Could not download Metal tooling automatically; finish Xcode first-launch setup and install it from Xcode if prompted."
  ok "Metal tooling download step completed"
}

find_brew() {
  if command -v brew >/dev/null 2>&1; then
    command -v brew
  elif [[ -x /opt/homebrew/bin/brew ]]; then
    printf '/opt/homebrew/bin/brew\n'
  elif [[ -x /usr/local/bin/brew ]]; then
    printf '/usr/local/bin/brew\n'
  else
    return 1
  fi
}

load_homebrew_environment() {
  BREW="$(find_brew)" || return 1
  eval "$("$BREW" shellenv)"
  BREW="$(command -v brew)"
}

install_homebrew() {
  if load_homebrew_environment; then
    ok "Homebrew is already installed"
    return 0
  fi

  info "Installing Homebrew"
  local xtrace_was_enabled=0
  if [[ $- == *x* ]]; then
    xtrace_was_enabled=1
    set +x
  fi

  NONINTERACTIVE=1 /usr/bin/env bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

  if (( xtrace_was_enabled )); then
    set -x
  fi

  load_homebrew_environment || fail "Homebrew installation completed, but brew was not found on PATH."
  ok "Homebrew installed"
}

configure_homebrew_shellenv() {
  info "Ensuring Homebrew is loaded by login shells"
  local brew_shellenv
  brew_shellenv="eval \"\$(${BREW} shellenv)\""
  append_line_if_missing "$USER_HOME/.zprofile" "$brew_shellenv"
  append_line_if_missing "$USER_HOME/.bash_profile" "$brew_shellenv"
  ok "Homebrew shell environment configured"
}

brew_install_or_upgrade_formula() {
  local formula="$1"
  if "$BREW" list --formula "$formula" >/dev/null 2>&1; then
    "$BREW" upgrade "$formula" || true
  else
    "$BREW" install "$formula"
  fi
}

brew_install_or_upgrade_cask() {
  local cask="$1" token
  token="${cask##*/}"
  if "$BREW" list --cask "$token" >/dev/null 2>&1; then
    "$BREW" upgrade --cask "$cask" || true
  else
    "$BREW" install --cask "$cask"
  fi
}

install_packages() {
  info "Installing base tools and Neovim with Homebrew"
  "$BREW" update
  brew_install_or_upgrade_formula git
  brew_install_or_upgrade_formula curl
  brew_install_or_upgrade_formula go
  brew_install_or_upgrade_formula neovim
  brew_install_or_upgrade_formula swiftly
  brew_install_or_upgrade_cask claude-code
  brew_install_or_upgrade_cask codex
  ok "Homebrew packages installed"
}

swiftly_env_path() {
  if [[ -n "${SWIFTLY_HOME_DIR:-}" && -f "${SWIFTLY_HOME_DIR}/env.sh" ]]; then
    printf '%s/env.sh\n' "$SWIFTLY_HOME_DIR"
  elif [[ -f "$USER_HOME/.swiftly/env.sh" ]]; then
    printf '%s/.swiftly/env.sh\n' "$USER_HOME"
  elif [[ -f "$USER_HOME/.local/share/swiftly/env.sh" ]]; then
    printf '%s/.local/share/swiftly/env.sh\n' "$USER_HOME"
  else
    return 1
  fi
}

install_swift_toolchain() {
  if (( ! INSTALL_SWIFT_TOOLCHAIN )); then
    ok "Swift toolchain installation skipped"
    return 0
  fi

  info "Initializing swiftly"
  if [[ ! -f "$USER_HOME/.swiftly/config.json" && ! -f "$USER_HOME/.local/share/swiftly/config.json" ]]; then
    swiftly init --skip-install --assume-yes --quiet-shell-followup
  fi
  ok "swiftly initialized"

  info "Ensuring swiftly is loaded by login shells"
  local env_line
  env_line='[ -f "${SWIFTLY_HOME_DIR:-$HOME/.swiftly}/env.sh" ] && . "${SWIFTLY_HOME_DIR:-$HOME/.swiftly}/env.sh"'
  append_line_if_missing "$USER_HOME/.zprofile" "$env_line"
  append_line_if_missing "$USER_HOME/.bash_profile" "$env_line"
  ok "swiftly shell environment configured"

  local env_path
  if env_path="$(swiftly_env_path)"; then
    # shellcheck disable=SC1090
    source "$env_path"
    hash -r
  fi

  info "Ensuring a Swift toolchain is installed via swiftly"
  if [[ -f "$REPO_ROOT/.swift-version" ]]; then
    (cd "$REPO_ROOT" && swiftly install)
  else
    local tmp_dir=""
    tmp_dir="$(mktemp -d)"
    if curl -fsSL "${WENDY_RAW_BASE}/.swift-version" -o "$tmp_dir/.swift-version"; then
      (cd "$tmp_dir" && swiftly install)
    else
      warn "No .swift-version found at ${REPO_ROOT} and could not download one; installing swiftly's default Swift toolchain."
      swiftly install
    fi
    rm -rf "$tmp_dir"
  fi
  swift --version | head -n 1
  ok "Swift toolchain is available"
}

clone_repository() {
  if (( ! CLONE_REPOSITORY )); then
    ok "Wendy repository not cloned"
    return 0
  fi

  info "Cloning Wendy repository"
  local parent_dir
  parent_dir="$(dirname "$CLONE_DESTINATION")"
  mkdir -p "$parent_dir"

  if [[ -d "$CLONE_DESTINATION/.git" ]]; then
    ok "Wendy repository already exists at ${CLONE_DESTINATION}"
    return 0
  fi

  if [[ -e "$CLONE_DESTINATION" ]] && [[ -n "$(find "$CLONE_DESTINATION" -mindepth 1 -maxdepth 1 2>/dev/null | head -n 1)" ]]; then
    warn "${CLONE_DESTINATION} exists and is not an empty git checkout; skipping clone."
    return 0
  fi

  git clone "$WENDY_REPO_URL" "$CLONE_DESTINATION"
  ok "Wendy repository cloned to ${CLONE_DESTINATION}"
}

configure_editor() {
  info "Setting Neovim as the default CLI editor"
  append_line_if_missing "$USER_HOME/.zshrc" "export EDITOR=nvim"
  append_line_if_missing "$USER_HOME/.zshrc" "export VISUAL=nvim"
  append_line_if_missing "$USER_HOME/.bashrc" "export EDITOR=nvim"
  append_line_if_missing "$USER_HOME/.bashrc" "export VISUAL=nvim"
  append_line_if_missing "$USER_HOME/.profile" "export EDITOR=nvim"
  append_line_if_missing "$USER_HOME/.profile" "export VISUAL=nvim"
  ok "Neovim is the default editor for new shells"
}

configure_direnv() {
  if (( ! INSTALL_DIRENV )); then
    ok "direnv not installed or configured"
    return 0
  fi

  info "Installing and configuring direnv"
  brew_install_or_upgrade_formula direnv
  append_line_if_missing "$USER_HOME/.zshrc" 'eval "$(direnv hook zsh)"'
  append_line_if_missing "$USER_HOME/.bashrc" 'eval "$(direnv hook bash)"'
  append_line_if_missing "$USER_HOME/.bash_profile" 'eval "$(direnv hook bash)"'
  ok "direnv installed and shell hooks configured"
}

configure_passwordless_sudo() {
  if (( ! SETUP_PASSWORDLESS_SUDO )); then
    ok "passwordless sudo not changed"
    return 0
  fi

  info "Enabling passwordless sudo for ${CURRENT_USER}"
  if ! run_sudo grep -Eq '^[#@]includedir[[:space:]]+/private/etc/sudoers.d' /private/etc/sudoers; then
    fail "${SUDOERS_DIR} is not included by /private/etc/sudoers on this Mac."
  fi

  run_sudo install -d -m 0755 "$SUDOERS_DIR"
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$CURRENT_USER" | run_sudo tee "$SUDOERS_FILE" >/dev/null
  run_sudo chmod 0440 "$SUDOERS_FILE"
  run_sudo visudo -cf "$SUDOERS_FILE" >/dev/null
  run_sudo visudo -cf /private/etc/sudoers >/dev/null
  ok "passwordless sudo enabled"
}

configure_ssh_keys() {
  info "Generating SSH keys and installing authorized login keys"

  mkdir -p "$USER_HOME/.ssh"
  chmod 700 "$USER_HOME/.ssh"

  if [[ ! -f "$USER_HOME/.ssh/id_ed25519" ]]; then
    ssh-keygen -t ed25519 -a 100 -N "" -C "${CURRENT_USER}@$(hostname)-$(date +%Y%m%d)" -f "$USER_HOME/.ssh/id_ed25519"
  elif [[ ! -f "$USER_HOME/.ssh/id_ed25519.pub" ]]; then
    ssh-keygen -y -f "$USER_HOME/.ssh/id_ed25519" > "$USER_HOME/.ssh/id_ed25519.pub"
  fi

  [[ ! -f "$USER_HOME/.ssh/id_ed25519" ]] || chmod 600 "$USER_HOME/.ssh/id_ed25519"
  [[ ! -f "$USER_HOME/.ssh/id_ed25519.pub" ]] || chmod 644 "$USER_HOME/.ssh/id_ed25519.pub"
  touch "$USER_HOME/.ssh/authorized_keys"
  chmod 600 "$USER_HOME/.ssh/authorized_keys"

  local key key_type key_body authorized_keys generated_public_key
  authorized_keys="$USER_HOME/.ssh/authorized_keys"
  generated_public_key="$(cat "$USER_HOME/.ssh/id_ed25519.pub" 2>/dev/null || true)"
  if (( CONFIGURE_LOOPBACK_SSH )) && [[ -n "$generated_public_key" ]]; then
    AUTHORIZED_LOGIN_KEYS=("$generated_public_key" "${AUTHORIZED_LOGIN_KEYS[@]}")
  fi

  for key in "${AUTHORIZED_LOGIN_KEYS[@]}"; do
    key_type="$(awk '{print $1}' <<<"$key")"
    key_body="$(awk '{print $2}' <<<"$key")"

    if [[ -z "$key_type" || -z "$key_body" ]]; then
      warn "Skipping malformed SSH public key: ${key}"
      continue
    fi

    if ! awk -v type="$key_type" -v body="$key_body" \
      '$1 == type && $2 == body { found = 1 } END { exit !found }' "$authorized_keys"; then
      printf '%s\n' "$key" >> "$authorized_keys"
    fi
  done

  if (( CONFIGURE_LOOPBACK_SSH )); then
    touch "$USER_HOME/.ssh/known_hosts"
    chmod 644 "$USER_HOME/.ssh/known_hosts"

    local host_alias
    for host_alias in localhost 127.0.0.1 ::1 "$(hostname -s 2>/dev/null || true)" "$(hostname 2>/dev/null || true)"; do
      [[ -n "$host_alias" ]] || continue
      ssh-keygen -F "$host_alias" -f "$USER_HOME/.ssh/known_hosts" >/dev/null 2>&1 || \
        ssh-keyscan -T 5 -H "$host_alias" >> "$USER_HOME/.ssh/known_hosts" 2>/dev/null || true
    done
    chmod 644 "$USER_HOME/.ssh/known_hosts"
  fi

  chmod 700 "$USER_HOME/.ssh"
  chmod 600 "$authorized_keys"
  ok "SSH keys configured"
}

configure_mdns() {
  info "Configuring Bonjour/mDNS local hostname"

  if scutil --get LocalHostName >/dev/null 2>&1; then
    ok "Bonjour/mDNS local hostname is already configured"
    return 0
  fi

  local name
  name="$(hostname -s 2>/dev/null || printf 'wendy-mac')"
  name="$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]-')"
  [[ -n "$name" ]] || name="wendy-mac"

  run_sudo scutil --set LocalHostName "$name"
  ok "Bonjour/mDNS local hostname set to ${name}.local"
}

configure_power_settings() {
  if (( ! CONFIGURE_POWER_SETTINGS )); then
    ok "AC sleep settings not changed"
    return 0
  fi

  info "Configuring macOS AC power settings for unattended use"
  run_sudo pmset -c standby 0 hibernatemode 0 powernap 0 displaysleep 10 sleep 0 disksleep 0
  ok "AC sleep disabled; display sleep set to 10 minutes"
}

install_wendy_cli() {
  if (( ! INSTALL_WENDY_CLI )); then
    ok "Wendy CLI not installed"
    return 0
  fi

  info "Installing or updating Wendy CLI with Homebrew"
  "$BREW" tap wendylabsinc/tap
  brew_install_or_upgrade_formula wendylabsinc/tap/wendy
  ok "Wendy CLI installed or updated"
}

install_wendy_agent() {
  if (( ! INSTALL_WENDY_AGENT )); then
    ok "Wendy macOS agent app not installed"
    return 0
  fi

  info "Installing or updating Wendy macOS agent app with Homebrew"
  "$BREW" tap wendylabsinc/tap
  brew_install_or_upgrade_cask wendylabsinc/tap/wendy-agent
  ok "Wendy macOS agent app installed or updated"
}

github_runner_asset_platform() {
  case "$(uname -m)" in
    arm64) printf 'osx-arm64\n' ;;
    x86_64) printf 'osx-x64\n' ;;
    *) fail "Unsupported macOS architecture for GitHub Actions runner: $(uname -m)" ;;
  esac
}

github_runner_latest_tag() {
  curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | awk -F '"' '/"tag_name"[[:space:]]*:/ && !found { print $4; found = 1 }'
}

github_runner_is_configured() {
  [[ -f "$GITHUB_RUNNER_DIR/.runner" ]]
}

github_runner_launch_agent_label() {
  printf 'com.github.actions.runner.%s\n' "$(printf '%s' "$GITHUB_RUNNER_DIR" | cksum | awk '{print $1}')"
}

github_runner_launch_agent_path() {
  printf '%s/Library/LaunchAgents/%s.plist\n' "$USER_HOME" "$(github_runner_launch_agent_label)"
}

xml_escape() {
  local value="$1"
  value="${value//&/&amp;}"
  value="${value//</&lt;}"
  value="${value//>/&gt;}"
  value="${value//\"/&quot;}"
  printf '%s' "$value"
}

configure_github_runner_startup() {
  case "$GITHUB_RUNNER_RUN_MODE" in
    manual)
      ok "GitHub Actions runner will be started manually"
      ;;
    service|daemon|headless)
      warn "macOS GitHub runners must run in a logged-in user session for TCC/privacy permissions; headless service mode is not configured."
      ;;
    login)
      if ! github_runner_is_configured; then
        warn "GitHub runner is installed but not registered. After running ./config.sh in ${GITHUB_RUNNER_DIR}, rerun this script to enable login-session startup."
        return 0
      fi
      info "Configuring GitHub Actions runner for the user login session"
      local uid label plist_path escaped_runner_dir
      uid="$(id -u "$CURRENT_USER")"
      label="$(github_runner_launch_agent_label)"
      plist_path="$(github_runner_launch_agent_path)"
      escaped_runner_dir="$(xml_escape "$GITHUB_RUNNER_DIR")"
      mkdir -p "$USER_HOME/Library/LaunchAgents"
      cat > "$plist_path" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${label}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${escaped_runner_dir}/run.sh</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${escaped_runner_dir}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>LimitLoadToSessionType</key>
  <string>Aqua</string>
</dict>
</plist>
EOF
      launchctl bootout "gui/${uid}" "$plist_path" >/dev/null 2>&1 || true
      launchctl bootstrap "gui/${uid}" "$plist_path" >/dev/null 2>&1 || warn "Could not start the GitHub runner LaunchAgent now; it should start at the next login."
      launchctl enable "gui/${uid}/${label}" >/dev/null 2>&1 || true
      launchctl kickstart -k "gui/${uid}/${label}" >/dev/null 2>&1 || true
      ok "GitHub Actions runner login-session startup configured at ${plist_path}"
      ;;
  esac
}

install_github_runner() {
  if (( ! SETUP_GITHUB_RUNNER )); then
    ok "GitHub Actions runner not installed"
    return 0
  fi

  info "Installing GitHub Actions self-hosted runner"
  mkdir -p "$GITHUB_RUNNER_DIR"

  if [[ ! -x "$GITHUB_RUNNER_DIR/bin/Runner.Listener" ]]; then
    local platform tag version archive url tmp_dir
    platform="$(github_runner_asset_platform)"
    tag="$(github_runner_latest_tag)"
    [[ -n "$tag" ]] || fail "Could not determine the latest GitHub Actions runner version."
    version="${tag#v}"
    archive="actions-runner-${platform}-${version}.tar.gz"
    url="https://github.com/actions/runner/releases/download/${tag}/${archive}"
    tmp_dir="$(mktemp -d)"
    curl -fL "$url" -o "$tmp_dir/$archive"
    tar -xzf "$tmp_dir/$archive" -C "$GITHUB_RUNNER_DIR"
    rm -rf "$tmp_dir"
  fi

  chmod +x "$GITHUB_RUNNER_DIR/config.sh" "$GITHUB_RUNNER_DIR/run.sh" 2>/dev/null || true
  configure_github_runner_startup
  ok "GitHub Actions runner installed at ${GITHUB_RUNNER_DIR}"
}

configure_git() {
  if (( ! CONFIGURE_GIT )); then
    ok "git identity not changed"
    return 0
  fi

  info "Configuring git identity for ${CURRENT_USER}"
  git config --global user.name "$GIT_NAME"
  git config --global user.email "$GIT_EMAIL"
  ok "git identity configured"
}

primary_ip() {
  local iface ip
  iface="$(route -n get default 2>/dev/null | awk '/interface:/{print $2; exit}' || true)"
  if [[ -n "$iface" ]]; then
    ip="$(ipconfig getifaddr "$iface" 2>/dev/null || true)"
    [[ -n "$ip" ]] && { printf '%s\n' "$ip"; return 0; }
  fi

  ip="$(ipconfig getifaddr en0 2>/dev/null || true)"
  [[ -n "$ip" ]] && { printf '%s\n' "$ip"; return 0; }

  return 1
}

has_macos_manual_steps() {
  (( SHOW_XCODE_MANUAL_STEP || INSTALL_WENDY_CLI || INSTALL_WENDY_AGENT || SHOW_MAC_NAME_MANUAL_STEP || SHOW_REMOTE_LOGIN_MANUAL_STEP || SHOW_SCREEN_SHARING_MANUAL_STEP || SHOW_SCREEN_LOCK_MANUAL_STEP || SHOW_AUTO_LOGIN_MANUAL_STEP || SETUP_GITHUB_RUNNER ))
}

manual_step() {
  local message="$1"

  if (( WALK_THROUGH_MANUAL_STEPS )); then
    printf '\n%s\n\n' "$message"
    printf 'Continue? [Return] '
    local answer
    read -r answer
    return 0
  fi

  printf '\n%s\n' "$message"
}

assisted_manual_step() {
  local message="$1"
  local command_to_run="$2"

  if (( WALK_THROUGH_MANUAL_STEPS )); then
    printf '\n%s\n\n' "$message"
    printf 'Ready? [Return, s to skip] '
    local answer
    read -r answer
    case "$answer" in
      s|S|skip|SKIP) return 0 ;;
    esac
    eval "$command_to_run" || true
    return 0
  fi

  printf '\n%s\n' "$message"
}

run_manual_steps() {
  has_macos_manual_steps || return 0

  cat <<EOF

$(bold "Manual macOS steps")
EOF

  if (( SHOW_XCODE_MANUAL_STEP )); then
    assisted_manual_step "  • I can open ${STYLE_BOLD}Xcode${STYLE_RESET} next. Please complete its first-run setup,
    tour/wizard, component installation, and license prompts." \
      "open -a \"$XCODE_APP_PATH\""
  fi

  if (( INSTALL_WENDY_CLI )); then
    assisted_manual_step "  • I can launch the installed ${STYLE_BOLD}wendy${STYLE_RESET} CLI next. If a permission
    dialog appears, approve Bluetooth and any other requested permissions." \
      "command -v wendy >/dev/null 2>&1 && wendy --help >/dev/null 2>&1"
  fi

  if (( INSTALL_WENDY_AGENT )); then
    assisted_manual_step "  • I can launch the installed ${STYLE_BOLD}Wendy agent${STYLE_RESET} app next. Approve
    permissions by following the instructions on its Welcome screen." \
      "open -a WendyAgentMac || open -a 'Wendy Agent' || open -a wendy-agent"
  fi

  if (( SHOW_MAC_NAME_MANUAL_STEP )); then
    manual_step "  • Review or change the Mac ${STYLE_BOLD}name${STYLE_RESET} and local ${STYLE_BOLD}hostname${STYLE_RESET} if desired:
      System Settings → General → About → Name
      System Settings → General → Sharing → Local hostname (at the bottom)"
  fi

  if (( SHOW_REMOTE_LOGIN_MANUAL_STEP )); then
    manual_step "  • Enable ${STYLE_BOLD}Remote Login${STYLE_RESET} if you want SSH access:
      System Settings → General → Sharing → Remote Login"
  fi

  if (( SHOW_SCREEN_SHARING_MANUAL_STEP )); then
    manual_step "  • Enable ${STYLE_BOLD}Screen Sharing${STYLE_RESET} if you want remote desktop access:
      System Settings → General → Sharing → Screen Sharing"
  fi

  if (( SHOW_SCREEN_LOCK_MANUAL_STEP )); then
    manual_step "  • Disable ${STYLE_BOLD}screen locking${STYLE_RESET} if desired:
      System Settings → Lock Screen → Require password after screen saver begins or display is turned off → ${STYLE_BOLD}Never${STYLE_RESET}"
  fi

  if (( SHOW_AUTO_LOGIN_MANUAL_STEP )); then
    manual_step "  • Enable ${STYLE_BOLD}automatic desktop login${STYLE_RESET} if desired:
      System Settings → Users & Groups → Automatically log in as ${CURRENT_USER}"
  fi

  if (( SETUP_GITHUB_RUNNER )); then
    if [[ "$GITHUB_RUNNER_RUN_MODE" == "login" ]]; then
      if (( WALK_THROUGH_MANUAL_STEPS )); then
        assisted_manual_step "  • Register the ${STYLE_BOLD}GitHub Actions runner${STYLE_RESET} if it is not already registered:
      cd \"${GITHUB_RUNNER_DIR}\"
      ./config.sh --url https://github.com/OWNER/REPO --token TOKEN
      Then return here. When you press Return, I will create, load, and start the user-session LaunchAgent:
      $(github_runner_launch_agent_path)" \
          "configure_github_runner_startup"
      else
        manual_step "  • Register the ${STYLE_BOLD}GitHub Actions runner${STYLE_RESET} if it is not already registered:
      cd \"${GITHUB_RUNNER_DIR}\"
      ./config.sh --url https://github.com/OWNER/REPO --token TOKEN
      Then rerun this setup script to create, load, and start the user-session LaunchAgent:
      $(github_runner_launch_agent_path)"
      fi
    else
      manual_step "  • Register the ${STYLE_BOLD}GitHub Actions runner${STYLE_RESET} if it is not already registered:
      cd \"${GITHUB_RUNNER_DIR}\"
      ./config.sh --url https://github.com/OWNER/REPO --token TOKEN
      Start it manually when needed with:
      ./run.sh"
    fi
  fi
}

summary() {
  local mdns_name ip public_key
  mdns_name="$(scutil --get LocalHostName 2>/dev/null || hostname -s)"
  ip="$(primary_ip || true)"
  public_key="$(cat "$USER_HOME/.ssh/id_ed25519.pub" 2>/dev/null || true)"

  cat <<EOF

$(bold "Setup complete")

Useful connection details:
  Username:        ${CURRENT_USER}
  Hostname:        $(hostname)
  mDNS name:       ${mdns_name}.local
  Primary IP:      ${ip:-unknown}
  SSH:             ssh ${CURRENT_USER}@${mdns_name}.local if Remote Login was enabled
  Screen Sharing:  vnc://${mdns_name}.local if enabled and permitted by macOS

Generated SSH public key:
  ${public_key:-not available}

EOF

  run_manual_steps

  cat <<EOF

You may need to open a new terminal, log out and back in, or launch the Wendy
macOS agent app once for shell PATH, editor, Swift, and app changes to appear.
EOF
}

main() {
  parse_arguments "$@"
  require_macos
  ensure_xcode_app
  collect_configuration
  confirm_plan
  enable_command_trace
  configure_ssh_keys
  configure_passwordless_sudo
  download_xcode_metal_tooling
  install_homebrew
  configure_homebrew_shellenv
  install_packages
  clone_repository
  install_swift_toolchain
  configure_editor
  configure_direnv
  configure_mdns
  configure_power_settings
  install_wendy_cli
  install_wendy_agent
  install_github_runner
  configure_git
  summary
}

main "$@"
