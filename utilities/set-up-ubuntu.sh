#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "${BASH_VERSINFO[0]}" -lt 4 ]]; then
  echo "This script needs bash 4 or newer." >&2
  exit 1
fi

TRACE_COMMANDS="${TRACE_COMMANDS:-0}"
readonly WENDY_RAW_BASE="${WENDY_RAW_BASE:-https://raw.githubusercontent.com/wendylabsinc/wendy-agent/main}"
readonly WENDY_REPO_URL="${WENDY_REPO_URL:-https://github.com/wendylabsinc/wendy-agent.git}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly SCRIPT_DIR REPO_ROOT

CURRENT_USER="${SUDO_USER:-$(id -un)}"
USER_HOME="$(getent passwd "$CURRENT_USER" | cut -d: -f6 || true)"
SUDOERS_FILE="/etc/sudoers.d/90-${CURRENT_USER}-passwordless"
readonly CURRENT_USER USER_HOME SUDOERS_FILE

GIT_NAME=""
GIT_EMAIL=""
CONFIGURE_GIT=0
SETUP_PASSWORDLESS_SUDO=0
CONFIGURE_LOOPBACK_SSH=0
ENABLE_SSH_LOGIN=0
INSTALL_DIRENV=0
INSTALL_WENDY_CLI=0
INSTALL_WENDY_AGENT=0
SETUP_GITHUB_RUNNER=0
GITHUB_RUNNER_DIR="${USER_HOME}/.github/actions-runner"
GITHUB_RUNNER_RUN_MODE="manual"
CLONE_REPOSITORY=0
CLONE_DESTINATION=""
SETUP_AUTO_LOGIN=0
CONFIGURE_REMOTE_DESKTOP=0
CONFIGURE_POWER_SETTINGS=0
WALK_THROUGH_MANUAL_STEPS=0
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

run_sudo() {
  sudo "$@"
}

run_as_user() {
  run_sudo -H -u "$CURRENT_USER" "$@"
}

ufw_allow_if_active() {
  command -v ufw >/dev/null 2>&1 || return 0
  run_sudo ufw status | grep -q 'Status: active' || return 0
  run_sudo ufw allow "$@"
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

require_ubuntu() {
  [[ -r /etc/os-release ]] || fail "Cannot find /etc/os-release. This script is intended for Ubuntu."
  # shellcheck disable=SC1091
  source /etc/os-release
  [[ "${ID:-}" == "ubuntu" ]] || fail "This script is intended for Ubuntu; detected '${ID:-unknown}'."
  [[ -n "$USER_HOME" ]] || fail "Could not determine home directory for ${CURRENT_USER}."
  [[ "$CURRENT_USER" != "root" ]] || fail "Run this as the normal Ubuntu user, not directly as root."
}

collect_configuration() {
  cat <<EOF
$(bold "Fresh Ubuntu headless setup")

This script is idempotent: it is safe to run repeatedly. Existing packages,
keys, SSH settings, and git settings will be reused or updated without creating
duplicates. Bash xtrace is enabled after password collection so you can see what
is being called; password-specific calls are redacted.
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

  if ask_yes_no "Enable SSH login via openssh-server?" "n"; then
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

  if ask_yes_no "Install or update the Wendy CLI?" "n"; then
    INSTALL_WENDY_CLI=1
  else
    INSTALL_WENDY_CLI=0
  fi

  if [[ -d "$REPO_ROOT/.git" ]]; then
    CLONE_REPOSITORY=0
  elif ask_yes_no "Clone the Wendy repository onto this machine?" "n"; then
    CLONE_REPOSITORY=1
    local default_clone_destination="${USER_HOME}/Projects/WendyLabs/wendy-agent"
    printf 'Clone destination [%s]: ' "$default_clone_destination"
    read -r CLONE_DESTINATION
    CLONE_DESTINATION="${CLONE_DESTINATION:-$default_clone_destination}"
  else
    CLONE_REPOSITORY=0
  fi

  if ask_yes_no "Install wendy-agent?" "n"; then
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
      printf 'Run GitHub runner as headless service, user login session, or manual only? [manual/service/login] '
      read -r GITHUB_RUNNER_RUN_MODE
      GITHUB_RUNNER_RUN_MODE="${GITHUB_RUNNER_RUN_MODE:-manual}"
      case "$GITHUB_RUNNER_RUN_MODE" in
        service|daemon|headless) GITHUB_RUNNER_RUN_MODE="service"; break ;;
        login|session|user) GITHUB_RUNNER_RUN_MODE="login"; break ;;
        manual|nothing|none) GITHUB_RUNNER_RUN_MODE="manual"; break ;;
        *) warn "Please answer service, login, or manual." ;;
      esac
    done
  else
    SETUP_GITHUB_RUNNER=0
  fi

  if ask_yes_no "Enable automatic desktop login for ${CURRENT_USER} on startup?" "n"; then
    SETUP_AUTO_LOGIN=1
  else
    SETUP_AUTO_LOGIN=0
  fi

  if ask_yes_no "Configure GNOME Remote Desktop when available?" "n"; then
    CONFIGURE_REMOTE_DESKTOP=1
  else
    CONFIGURE_REMOTE_DESKTOP=0
  fi

  if ask_yes_no "Disable Ubuntu sleep on AC, set display idle to 10 minutes, and disable screen locking?" "n"; then
    CONFIGURE_POWER_SETTINGS=1
  else
    CONFIGURE_POWER_SETTINGS=0
  fi

  if ask_yes_no "Walk through manual Ubuntu setup steps interactively at the end?" "n"; then
    WALK_THROUGH_MANUAL_STEPS=1
  else
    WALK_THROUGH_MANUAL_STEPS=0
  fi
}

confirm_plan() {
  local passwordless_sudo_summary git_summary ssh_key_summary wendy_cli_summary wendy_agent_summary github_runner_summary direnv_summary
  local ssh_summary ssh_package_summary loopback_ssh_summary auto_login_summary clone_summary remote_desktop_summary power_settings_summary manual_steps_summary

  if (( SETUP_PASSWORDLESS_SUDO )); then
    passwordless_sudo_summary="Passwordless sudo will be enabled for ${CURRENT_USER}"
  else
    passwordless_sudo_summary="Passwordless sudo will not be changed"
  fi

  if (( ENABLE_SSH_LOGIN )); then
    ssh_summary="SSH login via openssh-server will be enabled"
    ssh_package_summary="OpenSSH server/client first, then git, curl, build-essential,"
  else
    ssh_summary="SSH login will not be changed"
    ssh_package_summary="OpenSSH client, then git, curl, build-essential,"
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

  if (( INSTALL_WENDY_CLI )); then
    wendy_cli_summary="Wendy CLI will be installed using ${REPO_ROOT}/docs/cli.sh"
  else
    wendy_cli_summary="Wendy CLI will not be installed"
  fi

  if (( INSTALL_WENDY_AGENT )); then
    wendy_agent_summary="wendy-agent will be installed using ${REPO_ROOT}/docs/agent.sh"
  else
    wendy_agent_summary="wendy-agent will not be installed"
  fi

  if (( SETUP_GITHUB_RUNNER )); then
    case "$GITHUB_RUNNER_RUN_MODE" in
      service) github_runner_summary="GitHub Actions runner will be installed at ${GITHUB_RUNNER_DIR} and set to run as a headless service after registration" ;;
      login) github_runner_summary="GitHub Actions runner will be installed at ${GITHUB_RUNNER_DIR} and set to run in the user login session after registration" ;;
      *) github_runner_summary="GitHub Actions runner will be installed at ${GITHUB_RUNNER_DIR} for manual runs" ;;
    esac
  else
    github_runner_summary="GitHub Actions runner will not be installed"
  fi

  if (( CLONE_REPOSITORY )); then
    clone_summary="${WENDY_REPO_URL} will be cloned to ${CLONE_DESTINATION}"
  else
    clone_summary="Wendy repository will not be cloned"
  fi

  if (( SETUP_AUTO_LOGIN )); then
    auto_login_summary="Automatic desktop login will be enabled for ${CURRENT_USER} when a supported display manager is available"
  else
    auto_login_summary="Automatic desktop login will not be changed"
  fi

  if (( INSTALL_DIRENV )); then
    direnv_summary="direnv will be installed and its Bash hook will be configured"
  else
    direnv_summary="direnv will not be installed or configured"
  fi

  if (( CONFIGURE_REMOTE_DESKTOP )); then
    remote_desktop_summary="GNOME Remote Desktop will be configured when available"
  else
    remote_desktop_summary="GNOME Remote Desktop will not be changed"
  fi

  if (( CONFIGURE_POWER_SETTINGS )); then
    power_settings_summary="AC sleep and screen locking will be disabled, display idle will be set to 10 minutes, and lid close on AC will be ignored"
  else
    power_settings_summary="Power settings will not be changed"
  fi

  if (( WALK_THROUGH_MANUAL_STEPS )); then
    manual_steps_summary="Manual Ubuntu steps will be shown one at a time with confirmation prompts"
  else
    manual_steps_summary="Manual Ubuntu steps will be printed at the end"
  fi

  if (( ${#AUTHORIZED_LOGIN_KEYS[@]} )); then
    ssh_key_summary="${#AUTHORIZED_LOGIN_KEYS[@]} additional authorized SSH public key(s) for ${CURRENT_USER}"
  else
    ssh_key_summary="No additional authorized SSH public keys"
  fi

  cat <<EOF

This script will configure this machine by doing the following:

  • Install packages:
      ${ssh_package_summary}
      golang, Swift via swiftly, Avahi/mDNS tools, Neovim,
      Claude Code, and Codex.
      ${direnv_summary}

  • Configure:
      ${ssh_summary}
      SSH key generation for ${CURRENT_USER}
      ${ssh_key_summary}
      ${loopback_ssh_summary}
      Neovim as the default CLI editor
      ${direnv_summary}
      ${passwordless_sudo_summary}
      Avahi/mDNS discovery and name resolution
      ${remote_desktop_summary}
      ${power_settings_summary}
      ${auto_login_summary}
      ${manual_steps_summary}
      ${clone_summary}
      ${wendy_cli_summary}
      ${wendy_agent_summary}
      ${github_runner_summary}
      ${git_summary}

sudo and other tools may ask for credentials when they need elevated access.
EOF

  printf '\nContinue? [y/N] '
  local answer
  read -r answer
  case "$answer" in
    y|Y|yes|YES) ;;
    *) echo "Aborted."; exit 0 ;;
  esac
}

install_ssh_packages() {
  if (( ! ENABLE_SSH_LOGIN )); then
    ok "OpenSSH server not installed"
    return 0
  fi

  info "Installing OpenSSH server/client first"
  run_sudo apt-get update
  run_sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y \
    openssh-client \
    openssh-server
  ok "OpenSSH packages installed"
}

install_packages() {
  info "Updating apt package indexes"
  run_sudo apt-get update
  ok "apt package indexes updated"

  info "Ensuring Ubuntu universe repository is available"
  run_sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y \
    ca-certificates \
    curl \
    software-properties-common

  if ! grep -RhsE '^[^#].*\buniverse\b' /etc/apt/sources.list /etc/apt/sources.list.d 2>/dev/null; then
    run_sudo add-apt-repository -y universe
    run_sudo apt-get update
  fi
  ok "universe repository is available"

  info "Installing base tools, Avahi, and Neovim"
  run_sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y \
    avahi-autoipd \
    avahi-daemon \
    avahi-utils \
    build-essential \
    ca-certificates \
    curl \
    git \
    golang \
    libnss-mdns \
    mdns-scan \
    neovim \
    nodejs \
    npm \
    openssh-client
  info "Installing Claude Code and Codex"
  run_sudo npm install -g \
    @anthropic-ai/claude-code \
    @openai/codex

  ok "packages installed"
}

install_swiftly() {
  if run_as_user bash -c '
    swiftly_env="${SWIFTLY_HOME_DIR:-$HOME/.local/share/swiftly}/env.sh"
    [[ ! -f "$swiftly_env" ]] || . "$swiftly_env"
    command -v swiftly >/dev/null 2>&1
  '; then
    ok "swiftly is already installed"
  else
    info "Installing swiftly for ${CURRENT_USER}"
    run_as_user bash -c '
      set -euo pipefail
      tmp_dir="$(mktemp -d)"
      cleanup() { rm -rf "$tmp_dir"; }
      trap cleanup EXIT

      cd "$tmp_dir"
      archive="swiftly-$(uname -m).tar.gz"
      curl -fLO "https://download.swift.org/swiftly/linux/${archive}"
      tar zxf "$archive"
      ./swiftly init --quiet-shell-followup
      . "${SWIFTLY_HOME_DIR:-$HOME/.local/share/swiftly}/env.sh"
      hash -r
    '
    ok "swiftly installed"
  fi

  info "Ensuring swiftly is loaded by interactive Bash shells"
  run_as_user bash -c '
    set -euo pipefail
    bashrc="$HOME/.bashrc"
    env_line="[ -f \"\${SWIFTLY_HOME_DIR:-\$HOME/.local/share/swiftly}/env.sh\" ] && . \"\${SWIFTLY_HOME_DIR:-\$HOME/.local/share/swiftly}/env.sh\""

    touch "$bashrc"
    grep -qxF "$env_line" "$bashrc" || printf "\n%s\n" "$env_line" >> "$bashrc"
  '
  ok "swiftly is loaded by interactive Bash shells"

  info "Ensuring a Swift toolchain is installed via swiftly"
  run_as_user bash -c '
    set -euo pipefail

    swiftly_env="${SWIFTLY_HOME_DIR:-$HOME/.local/share/swiftly}/env.sh"
    if [[ -f "$swiftly_env" ]]; then
      source "$swiftly_env"
    fi

    if ! command -v swiftly >/dev/null 2>&1; then
      echo "swiftly was installed, but is not on PATH yet. Open a new shell and run from the repository root: swiftly install"
      exit 0
    fi

    repo_root="$1"
    raw_base="$2"
    tmp_dir=""
    cleanup() { [[ -z "$tmp_dir" ]] || rm -rf "$tmp_dir"; }
    trap cleanup EXIT

    if [[ -f "$repo_root/.swift-version" ]]; then
      cd "$repo_root"
    else
      tmp_dir="$(mktemp -d)"
      if curl -fsSL "${raw_base}/.swift-version" -o "$tmp_dir/.swift-version"; then
        cd "$tmp_dir"
      else
        echo "No .swift-version found at $repo_root and could not download one; running swiftly install from the current directory."
      fi
    fi

    swiftly install
    swift --version | head -n 1
  ' bash "$REPO_ROOT" "$WENDY_RAW_BASE"
  ok "Swift toolchain from .swift-version is available"
}

clone_repository() {
  if (( ! CLONE_REPOSITORY )); then
    ok "Wendy repository not cloned"
    return 0
  fi

  info "Cloning Wendy repository"
  local parent_dir
  parent_dir="$(dirname "$CLONE_DESTINATION")"
  run_as_user mkdir -p "$parent_dir"

  if [[ -d "$CLONE_DESTINATION/.git" ]]; then
    ok "Wendy repository already exists at ${CLONE_DESTINATION}"
    return 0
  fi

  if [[ -e "$CLONE_DESTINATION" ]] && [[ -n "$(run_as_user find "$CLONE_DESTINATION" -mindepth 1 -maxdepth 1 2>/dev/null | head -n 1)" ]]; then
    warn "${CLONE_DESTINATION} exists and is not an empty git checkout; skipping clone."
    return 0
  fi

  run_as_user git clone "$WENDY_REPO_URL" "$CLONE_DESTINATION"
  ok "Wendy repository cloned to ${CLONE_DESTINATION}"
}

configure_editor() {
  info "Setting Neovim as the default CLI editor"
  run_sudo update-alternatives --install /usr/bin/editor editor /usr/bin/nvim 100
  run_sudo update-alternatives --set editor /usr/bin/nvim

  run_as_user bash -c '
    set -euo pipefail
    profile="$HOME/.profile"
    touch "$profile"
    grep -qxF "export EDITOR=nvim" "$profile" || printf "\nexport EDITOR=nvim\n" >> "$profile"
    grep -qxF "export VISUAL=nvim" "$profile" || printf "export VISUAL=nvim\n" >> "$profile"
  '
  ok "Neovim is the default editor"
}

configure_direnv() {
  if (( ! INSTALL_DIRENV )); then
    ok "direnv not installed or configured"
    return 0
  fi

  info "Installing and configuring direnv"
  run_sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y direnv

  run_as_user bash -c '
    set -euo pipefail
    bashrc="$HOME/.bashrc"
    hook_line="eval \"\$(direnv hook bash)\""

    touch "$bashrc"
    grep -qxF "$hook_line" "$bashrc" || printf "\n%s\n" "$hook_line" >> "$bashrc"
  '

  ok "direnv installed and shell hook configured"
}

configure_passwordless_sudo() {
  if (( ! SETUP_PASSWORDLESS_SUDO )); then
    ok "passwordless sudo not changed"
    return 0
  fi

  info "Enabling passwordless sudo for ${CURRENT_USER}"
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$CURRENT_USER" | run_sudo tee "$SUDOERS_FILE" >/dev/null
  run_sudo chmod 0440 "$SUDOERS_FILE"
  run_sudo visudo -cf "$SUDOERS_FILE" >/dev/null
  ok "passwordless sudo enabled"
}

configure_ssh() {
  if (( ! ENABLE_SSH_LOGIN )); then
    ok "SSH login not changed"
    return 0
  fi

  info "Enabling SSH login"
  run_sudo systemctl enable --now ssh
  ufw_allow_if_active OpenSSH

  run_sudo install -d -m 0755 /etc/ssh/sshd_config.d
  run_sudo tee /etc/ssh/sshd_config.d/99-local-login.conf >/dev/null <<'EOF'
PasswordAuthentication yes
KbdInteractiveAuthentication yes
PubkeyAuthentication yes
PermitRootLogin no
EOF

  run_sudo systemctl reload ssh || run_sudo systemctl restart ssh
  ok "SSH login enabled"
}

configure_ssh_keys() {
  info "Generating SSH keys and installing authorized login keys"

  run_as_user bash -c '
    set -euo pipefail
    mkdir -p "$HOME/.ssh"
    chmod 700 "$HOME/.ssh"

    if [[ ! -f "$HOME/.ssh/id_ed25519" ]]; then
      ssh-keygen -t ed25519 -a 100 -N "" -C "${USER}@$(hostname)-$(date +%Y%m%d)" -f "$HOME/.ssh/id_ed25519"
    elif [[ ! -f "$HOME/.ssh/id_ed25519.pub" ]]; then
      ssh-keygen -y -f "$HOME/.ssh/id_ed25519" > "$HOME/.ssh/id_ed25519.pub"
    fi

    [[ ! -f "$HOME/.ssh/id_ed25519" ]] || chmod 600 "$HOME/.ssh/id_ed25519"
    [[ ! -f "$HOME/.ssh/id_ed25519.pub" ]] || chmod 644 "$HOME/.ssh/id_ed25519.pub"
    touch "$HOME/.ssh/authorized_keys"
    chmod 600 "$HOME/.ssh/authorized_keys"
  '

  local key key_type key_body generated_public_key
  local authorized_keys="$USER_HOME/.ssh/authorized_keys"
  generated_public_key="$(run_as_user bash -c 'cat "$HOME/.ssh/id_ed25519.pub" 2>/dev/null || true')"
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

    if ! run_as_user awk -v type="$key_type" -v body="$key_body" \
      '$1 == type && $2 == body { found = 1 } END { exit !found }' "$authorized_keys"; then
      printf '%s\n' "$key" | run_as_user tee -a "$authorized_keys" >/dev/null
    fi
  done

  if (( CONFIGURE_LOOPBACK_SSH )); then
    run_as_user touch "$USER_HOME/.ssh/known_hosts"
    run_as_user chmod 644 "$USER_HOME/.ssh/known_hosts"

    local host_alias
    for host_alias in localhost 127.0.0.1 ::1 "$(hostname -s 2>/dev/null || true)" "$(hostname 2>/dev/null || true)"; do
      [[ -n "$host_alias" ]] || continue
      if ! run_as_user ssh-keygen -F "$host_alias" -f "$USER_HOME/.ssh/known_hosts" >/dev/null 2>&1; then
        ssh-keyscan -T 5 -H "$host_alias" 2>/dev/null | run_as_user tee -a "$USER_HOME/.ssh/known_hosts" >/dev/null || true
      fi
    done
    run_as_user chmod 644 "$USER_HOME/.ssh/known_hosts"
  fi

  run_as_user chmod 700 "$USER_HOME/.ssh"
  run_as_user chmod 600 "$authorized_keys"
  ok "SSH keys configured"
}

configure_avahi() {
  info "Enabling Avahi/mDNS services"
  run_sudo systemctl enable --now avahi-daemon
  ufw_allow_if_active 5353/udp comment 'mDNS/Avahi'

  if [[ -f /etc/nsswitch.conf ]] && ! grep -Eq '^hosts:.*mdns4_minimal' /etc/nsswitch.conf; then
    run_sudo cp /etc/nsswitch.conf /etc/nsswitch.conf.bak-before-avahi
    run_sudo sed -i -E 's/^(hosts:[[:space:]]*files)([[:space:]]|$)/\1 mdns4_minimal [NOTFOUND=return] /' /etc/nsswitch.conf
  fi

  ok "Avahi/mDNS enabled"
}


configure_remote_desktop() {
  if (( ! CONFIGURE_REMOTE_DESKTOP )); then
    ok "GNOME Remote Desktop not changed"
    return 0
  fi

  info "Configuring GNOME Remote Desktop sharing + control"

  if ! command -v grdctl >/dev/null 2>&1; then
    warn "grdctl was not found; gnome-remote-desktop may not support CLI setup on this Ubuntu release."
    return 0
  fi

  local uid runtime_dir session_bus
  uid="$(id -u "$CURRENT_USER")"
  runtime_dir="/run/user/${uid}"
  session_bus="unix:path=${runtime_dir}/bus"

  if [[ ! -S "${runtime_dir}/bus" ]]; then
    warn "No active GNOME user session bus found. Log in graphically as ${CURRENT_USER}, then rerun this script to finish Remote Desktop setup."
    return 0
  fi

  warn "GNOME Remote Desktop credentials must be set manually in Settings before RDP login will work."

  run_sudo -H -u "$CURRENT_USER" env \
    XDG_RUNTIME_DIR="$runtime_dir" \
    DBUS_SESSION_BUS_ADDRESS="$session_bus" \
    grdctl rdp enable || warn "Could not enable RDP."

  run_sudo -H -u "$CURRENT_USER" env \
    XDG_RUNTIME_DIR="$runtime_dir" \
    DBUS_SESSION_BUS_ADDRESS="$session_bus" \
    grdctl rdp disable-view-only >/dev/null 2>&1 || true

  run_sudo -H -u "$CURRENT_USER" env \
    XDG_RUNTIME_DIR="$runtime_dir" \
    DBUS_SESSION_BUS_ADDRESS="$session_bus" \
    gsettings set org.gnome.desktop.remote-desktop.rdp enable true >/dev/null 2>&1 || true

  run_sudo -H -u "$CURRENT_USER" env \
    XDG_RUNTIME_DIR="$runtime_dir" \
    DBUS_SESSION_BUS_ADDRESS="$session_bus" \
    gsettings set org.gnome.desktop.remote-desktop.rdp view-only false >/dev/null 2>&1 || true

  ufw_allow_if_active 3389/tcp comment 'GNOME Remote Desktop RDP'
  run_sudo systemctl --global enable gnome-remote-desktop.service >/dev/null 2>&1 || true
  ok "GNOME Remote Desktop configured where supported"
}

configure_auto_login() {
  if (( ! SETUP_AUTO_LOGIN )); then
    ok "automatic desktop login not changed"
    return 0
  fi

  info "Enabling automatic desktop login for ${CURRENT_USER}"

  if [[ -f /etc/gdm3/custom.conf ]]; then
    run_sudo cp -n /etc/gdm3/custom.conf /etc/gdm3/custom.conf.bak-before-autologin || true
    run_sudo grep -q '^\[daemon\]' /etc/gdm3/custom.conf || printf '\n[daemon]\n' | run_sudo tee -a /etc/gdm3/custom.conf >/dev/null

    if run_sudo grep -Eq '^#?AutomaticLoginEnable=' /etc/gdm3/custom.conf; then
      run_sudo sed -i -E 's/^#?AutomaticLoginEnable=.*/AutomaticLoginEnable=true/' /etc/gdm3/custom.conf
    else
      run_sudo sed -i '/^\[daemon\]/a AutomaticLoginEnable=true' /etc/gdm3/custom.conf
    fi

    if run_sudo grep -Eq '^#?AutomaticLogin=' /etc/gdm3/custom.conf; then
      run_sudo sed -i -E "s/^#?AutomaticLogin=.*/AutomaticLogin=${CURRENT_USER}/" /etc/gdm3/custom.conf
    else
      run_sudo sed -i "/^\[daemon\]/a AutomaticLogin=${CURRENT_USER}" /etc/gdm3/custom.conf
    fi

    ok "GDM automatic desktop login configured for next boot"
    return 0
  fi

  if [[ -d /etc/lightdm ]]; then
    run_sudo install -d -m 0755 /etc/lightdm/lightdm.conf.d
    run_sudo tee /etc/lightdm/lightdm.conf.d/50-wendy-autologin.conf >/dev/null <<EOF
[Seat:*]
autologin-user=${CURRENT_USER}
autologin-user-timeout=0
EOF
    ok "LightDM automatic desktop login configured for next boot"
    return 0
  fi

  warn "No supported desktop login manager configuration was found; automatic desktop login was not changed."
}

configure_power_settings() {
  if (( ! CONFIGURE_POWER_SETTINGS )); then
    ok "power settings not changed"
    return 0
  fi

  info "Configuring Ubuntu AC power settings for unattended use"

  local uid runtime_dir session_bus user_unit_dir applied_gsettings=0
  uid="$(id -u "$CURRENT_USER")"
  runtime_dir="/run/user/${uid}"
  session_bus="unix:path=${runtime_dir}/bus"
  user_unit_dir="$USER_HOME/.config/systemd/user"

  if [[ -S "${runtime_dir}/bus" ]]; then
    run_as_user env XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="$session_bus" \
      systemctl --user disable --now ubuntu-ac-power-mode.service >/dev/null 2>&1 || true
  fi
  run_as_user rm -f \
    "$user_unit_dir/default.target.wants/ubuntu-ac-power-mode.service" \
    "$user_unit_dir/ubuntu-ac-power-mode.service"
  run_sudo rm -f /usr/local/bin/ubuntu-ac-power-mode

  if command -v gsettings >/dev/null 2>&1; then
    if [[ -S "${runtime_dir}/bus" ]]; then
      run_as_user env XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="$session_bus" bash -c '
        set -euo pipefail

        set_if_exists() {
          local schema="$1" key="$2" value="$3"
          gsettings range "$schema" "$key" >/dev/null 2>&1 || return 0
          gsettings set "$schema" "$key" "$value"
        }

        set_if_exists org.gnome.settings-daemon.plugins.power sleep-inactive-ac-type "'\''nothing'\''"
        set_if_exists org.gnome.settings-daemon.plugins.power sleep-inactive-ac-timeout 0
        set_if_exists org.gnome.desktop.session idle-delay "uint32 600"
        set_if_exists org.gnome.desktop.screensaver lock-enabled false
        set_if_exists org.gnome.desktop.screensaver lock-delay "uint32 0"
        set_if_exists org.gnome.desktop.screensaver ubuntu-lock-on-suspend false
        set_if_exists org.gnome.desktop.lockdown disable-lock-screen true
      '
      applied_gsettings=1
    else
      warn "No active GNOME user session bus found. Log in graphically as ${CURRENT_USER}, then rerun this script to apply GNOME power settings."
    fi
  else
    warn "gsettings was not found; skipping GNOME power configuration."
  fi

  run_sudo install -d -m 0755 /etc/systemd/logind.conf.d
  run_sudo tee /etc/systemd/logind.conf.d/99-local-ac-power.conf >/dev/null <<'EOF'
[Login]
HandleLidSwitchExternalPower=ignore
EOF
  run_sudo systemctl reload systemd-logind || run_sudo systemctl restart systemd-logind

  if (( applied_gsettings )); then
    ok "AC sleep disabled; display idle set to 10 minutes; screen locking disabled; lid close on AC ignored"
  else
    ok "legacy power helper removed; lid close on AC ignored; GNOME power settings still need an active user session"
  fi
}

install_wendy_cli() {
  if (( ! INSTALL_WENDY_CLI )); then
    ok "Wendy CLI not installed"
    return 0
  fi

  local install_script="${REPO_ROOT}/docs/cli.sh"
  local tmp_script=""
  if [[ ! -f "$install_script" ]]; then
    tmp_script="$(mktemp)"
    curl -fsSL "${WENDY_RAW_BASE}/docs/cli.sh" -o "$tmp_script" || fail "Could not find ${install_script} or download docs/cli.sh"
    install_script="$tmp_script"
  fi

  info "Installing Wendy CLI"
  run_sudo bash "$install_script" -y
  [[ -z "$tmp_script" ]] || rm -f "$tmp_script"
  ok "Wendy CLI installation complete"
}

install_wendy_agent() {
  if (( ! INSTALL_WENDY_AGENT )); then
    ok "wendy-agent not installed"
    return 0
  fi

  local install_script="${REPO_ROOT}/docs/agent.sh"
  local tmp_script=""
  if [[ ! -f "$install_script" ]]; then
    tmp_script="$(mktemp)"
    curl -fsSL "${WENDY_RAW_BASE}/docs/agent.sh" -o "$tmp_script" || fail "Could not find ${install_script} or download docs/agent.sh"
    install_script="$tmp_script"
  fi

  info "Installing wendy-agent"
  run_sudo bash "$install_script" -y
  [[ -z "$tmp_script" ]] || rm -f "$tmp_script"
  ok "wendy-agent installation complete"
}

github_runner_asset_platform() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'linux-x64\n' ;;
    aarch64|arm64) printf 'linux-arm64\n' ;;
    armv7l|armv6l) printf 'linux-arm\n' ;;
    *) fail "Unsupported Linux architecture for GitHub Actions runner: $(uname -m)" ;;
  esac
}

github_runner_latest_tag() {
  curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | awk -F '"' '/"tag_name"[[:space:]]*:/ && !found { print $4; found = 1 }'
}

github_runner_is_configured() {
  [[ -f "$GITHUB_RUNNER_DIR/.runner" ]]
}

configure_github_runner_startup() {
  case "$GITHUB_RUNNER_RUN_MODE" in
    manual)
      ok "GitHub Actions runner will be started manually"
      ;;
    service)
      if ! github_runner_is_configured; then
        warn "GitHub runner is installed but not registered. After running ./config.sh in ${GITHUB_RUNNER_DIR}, rerun this script or run: cd ${GITHUB_RUNNER_DIR} && sudo ./svc.sh install ${CURRENT_USER} && sudo ./svc.sh start"
        return 0
      fi
      info "Configuring GitHub Actions runner as a headless service"
      (cd "$GITHUB_RUNNER_DIR" && run_sudo ./svc.sh install "$CURRENT_USER" || true)
      (cd "$GITHUB_RUNNER_DIR" && run_sudo ./svc.sh start)
      ok "GitHub Actions runner service configured"
      ;;
    login)
      if ! github_runner_is_configured; then
        warn "GitHub runner is installed but not registered. After running ./config.sh in ${GITHUB_RUNNER_DIR}, rerun this script to enable login-session startup."
        return 0
      fi
      info "Configuring GitHub Actions runner for the user login session"
      local uid runtime_dir session_bus user_unit_dir wrapper unit_name
      uid="$(id -u "$CURRENT_USER")"
      runtime_dir="/run/user/${uid}"
      session_bus="unix:path=${runtime_dir}/bus"
      user_unit_dir="$USER_HOME/.config/systemd/user"
      wrapper="$USER_HOME/.local/bin/github-actions-runner"
      unit_name="github-actions-runner.service"

      run_as_user install -d -m 0755 "$USER_HOME/.local/bin" "$user_unit_dir" "$user_unit_dir/default.target.wants"
      run_as_user tee "$wrapper" >/dev/null <<EOF
#!/usr/bin/env bash
set -Eeuo pipefail
cd $(printf '%q' "$GITHUB_RUNNER_DIR")
exec ./run.sh
EOF
      run_as_user chmod 0755 "$wrapper"
      run_as_user tee "$user_unit_dir/$unit_name" >/dev/null <<EOF
[Unit]
Description=GitHub Actions self-hosted runner
After=default.target

[Service]
Type=simple
ExecStart=${wrapper}
Restart=always
RestartSec=5s

[Install]
WantedBy=default.target
EOF
      run_as_user ln -sfn "../$unit_name" "$user_unit_dir/default.target.wants/$unit_name"

      if [[ -S "${runtime_dir}/bus" ]]; then
        run_as_user env XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="$session_bus" systemctl --user daemon-reload
        run_as_user env XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="$session_bus" systemctl --user enable --now "$unit_name"
      else
        warn "No active user session bus found; GitHub runner will start on the next login."
      fi
      ok "GitHub Actions runner login-session startup configured"
      ;;
  esac
}

install_github_runner() {
  if (( ! SETUP_GITHUB_RUNNER )); then
    ok "GitHub Actions runner not installed"
    return 0
  fi

  info "Installing GitHub Actions self-hosted runner"
  local platform
  platform="$(github_runner_asset_platform)"

  run_as_user bash -c '
    set -Eeuo pipefail
    runner_dir="$1"
    platform="$2"
    mkdir -p "$runner_dir"

    if [[ ! -x "$runner_dir/bin/Runner.Listener" ]]; then
      tag="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | awk -F "\\\"" '\''/"tag_name"[[:space:]]*:/ && !found { print $4; found = 1 }'\'')"
      [[ -n "$tag" ]] || { echo "Could not determine the latest GitHub Actions runner version." >&2; exit 1; }
      version="${tag#v}"
      archive="actions-runner-${platform}-${version}.tar.gz"
      url="https://github.com/actions/runner/releases/download/${tag}/${archive}"
      tmp_dir="$(mktemp -d)"
      trap '\''rm -rf "$tmp_dir"'\'' EXIT
      curl -fL "$url" -o "$tmp_dir/$archive"
      tar -xzf "$tmp_dir/$archive" -C "$runner_dir"
    fi

    chmod +x "$runner_dir/config.sh" "$runner_dir/run.sh" 2>/dev/null || true
  ' bash "$GITHUB_RUNNER_DIR" "$platform"

  if [[ -x "$GITHUB_RUNNER_DIR/bin/installdependencies.sh" ]]; then
    (cd "$GITHUB_RUNNER_DIR" && run_sudo ./bin/installdependencies.sh) || warn "Could not install optional GitHub runner dependencies automatically."
  fi

  configure_github_runner_startup
  ok "GitHub Actions runner installed at ${GITHUB_RUNNER_DIR}"
}

configure_git() {
  if (( ! CONFIGURE_GIT )); then
    ok "git identity not changed"
    return 0
  fi

  info "Configuring git identity for ${CURRENT_USER}"
  run_as_user git config --global user.name "$GIT_NAME"
  run_as_user git config --global user.email "$GIT_EMAIL"
  ok "git identity configured"
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

run_manual_steps() {
  cat <<EOF

$(bold "Manual Ubuntu steps")
EOF

  manual_step "  • Open a new terminal, or log out and back in, so PATH, editor,
    Swift, and direnv shell changes are loaded."

  manual_step "  • Launch Claude Code and Codex once and complete their sign-in or
    first-run setup flows."

  if (( INSTALL_DIRENV )); then
    manual_step "  • If you use this checkout with direnv, run this once from the repo:
      direnv allow"
  fi

  if (( CONFIGURE_REMOTE_DESKTOP )); then
    manual_step "  • Set GNOME Remote Desktop credentials manually before RDP login:
      Settings → System → Remote Desktop"
  fi

  if (( ENABLE_SSH_LOGIN )); then
    manual_step "  • Verify SSH from another machine. Password login uses the Ubuntu
    account password; key login works after your public keys are installed."
  fi

  if (( SETUP_AUTO_LOGIN || CONFIGURE_POWER_SETTINGS )); then
    manual_step "  • Reboot or log out and back in once to verify automatic login
    and selected power settings."
  fi

  if (( SETUP_GITHUB_RUNNER )); then
    manual_step "  • Register the GitHub Actions runner if it is not already registered:
      cd "${GITHUB_RUNNER_DIR}"
      ./config.sh --url https://github.com/OWNER/REPO --token TOKEN
      Start it manually with ./run.sh, or rerun this setup script to enable the selected startup mode."
  fi
}

summary() {
  local hostname_ip public_key
  hostname_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  public_key="$(run_as_user bash -c 'cat "$HOME/.ssh/id_ed25519.pub" 2>/dev/null || true')"

  cat <<EOF

$(bold "Setup complete")

Useful connection details:
  Username:        ${CURRENT_USER}
  Hostname:        $(hostname)
  mDNS name:       $(hostname).local
  Primary IP:      ${hostname_ip:-unknown}
  SSH:             ssh ${CURRENT_USER}@$(hostname).local if SSH login was enabled
  Remote Desktop:  RDP to $(hostname).local if GNOME Remote Desktop was available

Generated SSH public key:
  ${public_key:-not available}
EOF

  run_manual_steps
}

main() {
  parse_arguments "$@"
  require_ubuntu
  collect_configuration
  confirm_plan
  enable_command_trace
  install_ssh_packages
  configure_ssh
  configure_ssh_keys
  configure_passwordless_sudo
  install_packages
  clone_repository
  install_swiftly
  configure_editor
  configure_direnv
  configure_avahi
  configure_remote_desktop
  configure_auto_login
  configure_power_settings
  install_wendy_cli
  install_wendy_agent
  install_github_runner
  configure_git
  summary
}

main "$@"
