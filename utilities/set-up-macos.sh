#!/usr/bin/env bash
set -Eeuo pipefail

readonly TRACE_COMMANDS="${TRACE_COMMANDS:-1}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly SCRIPT_DIR REPO_ROOT

CURRENT_USER="$(id -un)"
USER_HOME="${HOME}"
SUDOERS_DIR="/private/etc/sudoers.d"
SUDOERS_FILE="${SUDOERS_DIR}/90-${CURRENT_USER}-passwordless"
readonly CURRENT_USER USER_HOME SUDOERS_DIR SUDOERS_FILE

LOGIN_PASSWORD=""
GIT_NAME=""
GIT_EMAIL=""
CONFIGURE_GIT=1
SETUP_PASSWORDLESS_SUDO=0
SETUP_AUTO_LOGIN=0
ENABLE_SSH_LOGIN=0
INSTALL_WENDY_CLI=0
INSTALL_WENDY_AGENT=0
INSTALL_SWIFT_TOOLCHAIN=1
INSTALL_BUILD_TOOLS=1
INSTALL_DIRENV=0
ENABLE_SCREEN_SHARING=0
DISABLE_AC_SLEEP=0
DISABLE_SCREEN_LOCKING=0
BREW=""
AUTHORIZED_LOGIN_KEYS=()
PS4='+ ${BASH_SOURCE##*/}:${LINENO}: '

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
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

sudo_authenticate() {
  sudo -n true 2>/dev/null && return 0

  local xtrace_was_enabled=0
  if [[ $- == *x* ]]; then
    xtrace_was_enabled=1
    set +x
  fi

  local status=0
  sudo -S -v <<<"$LOGIN_PASSWORD" || status=$?

  if (( xtrace_was_enabled )); then
    set -x
  fi

  return "$status"
}

run_sudo() {
  sudo_authenticate
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

  printf '\nConfigure global git identity? [Y/n] '
  local answer
  read -r answer
  case "${answer:-y}" in
    n|N|no|NO)
      CONFIGURE_GIT=0
      ;;
    *)
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
  esac

  if ask_yes_no "Enable SSH login via macOS Remote Login?" "n"; then
    ENABLE_SSH_LOGIN=1
  else
    ENABLE_SSH_LOGIN=0
  fi

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

  if ask_yes_no "Enable automatic desktop login for ${CURRENT_USER} on startup? This stores an obfuscated copy of your password." "n"; then
    SETUP_AUTO_LOGIN=1
  else
    SETUP_AUTO_LOGIN=0
  fi

  if ask_yes_no "Install Xcode Command Line Tools for building native code?" "y"; then
    INSTALL_BUILD_TOOLS=1
  else
    INSTALL_BUILD_TOOLS=0
  fi

  if ask_yes_no "Install and configure direnv for repository-local developer tooling?" "n"; then
    INSTALL_DIRENV=1
  else
    INSTALL_DIRENV=0
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

  if ask_yes_no "Enable macOS Screen Sharing / Remote Management?" "n"; then
    ENABLE_SCREEN_SHARING=1
  else
    ENABLE_SCREEN_SHARING=0
  fi

  if ask_yes_no "Disable automatic sleep and display sleep on AC power?" "n"; then
    DISABLE_AC_SLEEP=1
  else
    DISABLE_AC_SLEEP=0
  fi

  if ask_yes_no "Disable screen locking for ${CURRENT_USER}?" "n"; then
    DISABLE_SCREEN_LOCKING=1
  else
    DISABLE_SCREEN_LOCKING=0
  fi
}

confirm_plan() {
  local passwordless_sudo_summary git_summary ssh_key_summary swift_summary build_tools_summary direnv_summary
  local ssh_summary auto_login_summary wendy_cli_summary wendy_agent_summary screen_sharing_summary sleep_summary lock_summary

  if (( SETUP_PASSWORDLESS_SUDO )); then
    passwordless_sudo_summary="Passwordless sudo will be enabled for ${CURRENT_USER}"
  else
    passwordless_sudo_summary="Passwordless sudo will not be changed"
  fi

  if (( ENABLE_SSH_LOGIN )); then
    ssh_summary="SSH login via macOS Remote Login will be enabled"
  else
    ssh_summary="SSH login will not be changed"
  fi

  if (( SETUP_AUTO_LOGIN )); then
    auto_login_summary="Automatic desktop login will be enabled for ${CURRENT_USER}; macOS stores an obfuscated password"
  else
    auto_login_summary="Automatic desktop login will not be changed"
  fi

  if (( CONFIGURE_GIT )); then
    git_summary="Global git user.name (${GIT_NAME}) and user.email (${GIT_EMAIL}) for ${CURRENT_USER}"
  else
    git_summary="Global git identity will not be changed"
  fi

  if (( INSTALL_BUILD_TOOLS )); then
    build_tools_summary="Xcode Command Line Tools will be installed when missing"
  else
    build_tools_summary="Xcode Command Line Tools installation will be skipped"
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

  if (( ENABLE_SCREEN_SHARING )); then
    screen_sharing_summary="Screen Sharing / Remote Management will be enabled where supported"
  else
    screen_sharing_summary="Screen Sharing / Remote Management will not be changed"
  fi

  if (( DISABLE_AC_SLEEP )); then
    sleep_summary="AC sleep and display sleep will be disabled"
  else
    sleep_summary="AC sleep and display sleep will not be changed"
  fi

  if (( DISABLE_SCREEN_LOCKING )); then
    lock_summary="Screen locking will be disabled for ${CURRENT_USER}"
  else
    lock_summary="Screen locking will not be changed"
  fi

  if (( ${#AUTHORIZED_LOGIN_KEYS[@]} )); then
    ssh_key_summary="${#AUTHORIZED_LOGIN_KEYS[@]} additional authorized SSH public key(s) for ${CURRENT_USER}"
  else
    ssh_key_summary="No additional authorized SSH public keys"
  fi

  cat <<EOF

This script will configure this Mac by doing the following:

  • Install developer tools and Homebrew packages:
      ${build_tools_summary}
      Homebrew packages: git, curl, go, Neovim, and swiftly.
      ${direnv_summary}
      ${swift_summary}
      ${wendy_cli_summary}
      ${wendy_agent_summary}

  • Configure:
      ${ssh_summary}
      SSH key generation for ${CURRENT_USER}
      ${ssh_key_summary}
      Neovim as the default CLI editor
      ${direnv_summary}
      ${passwordless_sudo_summary}
      Bonjour/mDNS local hostname discovery
      ${screen_sharing_summary}
      ${sleep_summary}
      ${lock_summary}
      ${auto_login_summary}
      ${git_summary}

You will be asked for your macOS login password once. It is used to run sudo
commands. Homebrew is installed automatically if it is not already available.
EOF

  printf '\nContinue? [y/N] '
  local answer
  read -r answer
  case "$answer" in
    y|Y|yes|YES) ;;
    *) echo "Aborted."; exit 0 ;;
  esac
}

ask_for_password() {
  printf '\nmacOS login password for %s: ' "$CURRENT_USER"
  read -rs LOGIN_PASSWORD
  printf '\n'
  [[ -n "$LOGIN_PASSWORD" ]] || fail "macOS login password cannot be empty; it is required for sudo."

  info "Checking sudo access"
  sudo_authenticate || fail "sudo authentication failed."
  ok "sudo access confirmed"
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

install_build_tools() {
  if (( ! INSTALL_BUILD_TOOLS )); then
    ok "Xcode Command Line Tools installation skipped"
    return 0
  fi

  if xcode-select -p >/dev/null 2>&1; then
    ok "Xcode Command Line Tools are already installed"
    return 0
  fi

  info "Installing Xcode Command Line Tools"
  run_sudo touch /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress

  local product
  product="$(softwareupdate -l 2>/dev/null \
    | awk -F'*' '/^ *\*/ {print $2}' \
    | sed -E 's/^ *Label: //; s/^ *//; s/ *$//' \
    | grep -E '^Command Line Tools' \
    | tail -n 1 || true)"

  if [[ -n "$product" ]]; then
    run_sudo softwareupdate -i "$product" --verbose
  else
    warn "Could not find Command Line Tools in softwareupdate; opening Apple's installer prompt."
    xcode-select --install || true
    warn "Finish the Command Line Tools installer, then rerun this script."
  fi

  run_sudo rm -f /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress

  if xcode-select -p >/dev/null 2>&1; then
    ok "Xcode Command Line Tools installed"
  else
    fail "Xcode Command Line Tools are still not installed."
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
    warn "No .swift-version found at ${REPO_ROOT}; installing swiftly's default Swift toolchain."
    swiftly install
  fi
  swift --version | head -n 1
  ok "Swift toolchain is available"
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

configure_auto_login() {
  if (( ! SETUP_AUTO_LOGIN )); then
    ok "automatic desktop login not changed"
    return 0
  fi

  info "Enabling automatic desktop login for ${CURRENT_USER}"

  if command -v fdesetup >/dev/null 2>&1 && fdesetup status 2>/dev/null | grep -q 'FileVault is On'; then
    warn "FileVault is on; macOS may ignore automatic login until FileVault is disabled."
  fi

  local xtrace_was_enabled=0
  if [[ $- == *x* ]]; then
    xtrace_was_enabled=1
    set +x
  fi

  printf '%s\0' "$LOGIN_PASSWORD" | run_sudo /usr/bin/perl -e '
    use strict;
    use warnings;
    my @key = (0x7d, 0x89, 0x52, 0x23, 0xd2, 0xbc, 0xdd, 0xea, 0xa3, 0xb9, 0x1f);
    local $/;
    my $password = <STDIN>;
    open my $fh, ">:raw", "/etc/kcpassword" or die "open /etc/kcpassword: $!";
    for my $i (0 .. length($password) - 1) {
      print {$fh} chr(ord(substr($password, $i, 1)) ^ $key[$i % @key]);
    }
    close $fh or die "close /etc/kcpassword: $!";
  '

  if (( xtrace_was_enabled )); then
    set -x
  fi

  run_sudo chmod 0600 /etc/kcpassword
  run_sudo defaults write /Library/Preferences/com.apple.loginwindow autoLoginUser "$CURRENT_USER"
  ok "automatic desktop login configured for next boot"
}

configure_ssh() {
  if (( ! ENABLE_SSH_LOGIN )); then
    ok "SSH login not changed"
    return 0
  fi

  info "Enabling SSH login via macOS Remote Login"
  run_sudo systemsetup -setremotelogin on

  if [[ -x /usr/libexec/ApplicationFirewall/socketfilterfw ]]; then
    run_sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add /usr/libexec/sshd-keygen-wrapper >/dev/null 2>&1 || true
    run_sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp /usr/libexec/sshd-keygen-wrapper >/dev/null 2>&1 || true
  fi

  ok "SSH login enabled"
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

  local key key_type key_body authorized_keys
  authorized_keys="$USER_HOME/.ssh/authorized_keys"

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

configure_screen_sharing() {
  if (( ! ENABLE_SCREEN_SHARING )); then
    ok "Screen Sharing / Remote Management not changed"
    return 0
  fi

  info "Enabling Screen Sharing / Remote Management"
  local kickstart="/System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart"
  if [[ ! -x "$kickstart" ]]; then
    warn "Apple Remote Desktop kickstart tool was not found; enable Screen Sharing manually in System Settings."
    return 0
  fi

  run_sudo "$kickstart" -activate -configure -access -on -restart -agent || warn "Could not enable Screen Sharing / Remote Management."
  ok "Screen Sharing / Remote Management configured where supported"
}

configure_power_settings() {
  if (( ! DISABLE_AC_SLEEP && ! DISABLE_SCREEN_LOCKING )); then
    ok "power and lock settings not changed"
    return 0
  fi

  if (( DISABLE_AC_SLEEP )); then
    info "Disabling automatic sleep and display sleep on AC power"
    run_sudo pmset -c sleep 0
    run_sudo pmset -c displaysleep 0
    run_sudo pmset -c disksleep 0
    ok "AC sleep policy configured"
  else
    ok "AC sleep policy not changed"
  fi

  if (( DISABLE_SCREEN_LOCKING )); then
    info "Disabling screen locking for ${CURRENT_USER}"
    defaults write com.apple.screensaver askForPassword -int 0
    defaults write com.apple.screensaver askForPasswordDelay -int 0
    killall cfprefsd >/dev/null 2>&1 || true
    ok "screen locking disabled for ${CURRENT_USER}"
  else
    ok "screen locking not changed"
  fi
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

You may need to open a new terminal, log out and back in, or launch the Wendy
macOS agent app once for shell PATH, editor, Swift, and app changes to appear.
EOF
}

main() {
  require_macos
  collect_configuration
  confirm_plan
  ask_for_password
  enable_command_trace
  configure_ssh
  configure_ssh_keys
  configure_passwordless_sudo
  configure_auto_login
  install_build_tools
  install_homebrew
  configure_homebrew_shellenv
  install_packages
  install_swift_toolchain
  configure_editor
  configure_direnv
  configure_mdns
  configure_screen_sharing
  configure_power_settings
  install_wendy_cli
  install_wendy_agent
  configure_git
  summary
}

main "$@"
