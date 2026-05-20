#!/usr/bin/env bash
set -uo pipefail

# Format: hostname|operating system|ip|ssh user
CI_MACHINES=(
  "wendy-developer-ubuntu|Ubuntu 24|100.65.138.11|wendy"
  "wendy-windows|Windows 11|100.74.221.36|wendy"
  "wendy-mac-mini-1|macOS 26|100.76.185.124|wendy"
  "wendy-mac-mini-2|macOS 26|100.86.205.126|wendy"
)

SSH_USER="${CHECK_CI_SSH_USER:-}"
SSH_CONNECT_TIMEOUT="${CHECK_CI_SSH_CONNECT_TIMEOUT:-10}"
STRICT_HOST_KEY_CHECKING="${CHECK_CI_STRICT_HOST_KEY_CHECKING:-no}"
COLOR="${CHECK_CI_COLOR:-auto}"

init_colors() {
  local use_color="false"
  case "$(printf '%s' "$COLOR" | tr '[:upper:]' '[:lower:]')" in
    always|true|1|yes|on|enabled)
      use_color="true"
      ;;
    never|false|0|no|off|disabled)
      use_color="false"
      ;;
    auto)
      if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
        use_color="true"
      fi
      ;;
    *)
      echo "ERROR: CHECK_CI_COLOR must be auto, always, or never." >&2
      exit 64
      ;;
  esac

  if [[ "$use_color" == "true" ]]; then
    C_RESET=$'\033[0m'
    C_BOLD=$'\033[1m'
    C_RED=$'\033[31m'
    C_GREEN=$'\033[32m'
    C_CYAN=$'\033[36m'
  else
    C_RESET=""
    C_BOLD=""
    C_RED=""
    C_GREEN=""
    C_CYAN=""
  fi
}

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Check SSH access to each hardcoded Wendy CI machine by hostname and by IP.

Options:
  --ssh-user USER            Default SSH user for machines without a per-machine user.
  --ssh-connect-timeout SEC  SSH connection timeout in seconds (default: $SSH_CONNECT_TIMEOUT).
  --color WHEN               Color output: auto, always, never (default: $COLOR).
  --no-color                 Disable color output.
  --help, -h                 Show this help message.
EOF
}

require_tool() {
  local command_name="$1"
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "ERROR: Missing required tool: $command_name" >&2
    exit 1
  fi
}

status_ok() {
  printf '%sOK%s' "$C_GREEN" "$C_RESET"
}

status_failed() {
  printf '%sFAIL%s' "$C_RED" "$C_RESET"
}

ssh_target() {
  local user="$1"
  local host="$2"

  if [[ "$host" == *:* && "$host" != \[*\] ]]; then
    host="[$host]"
  fi

  if [[ -n "$user" ]]; then
    printf "%s@%s" "$user" "$host"
  else
    printf "%s" "$host"
  fi
}

run_ssh_check() {
  local target="$1"

  ssh \
    -o BatchMode=yes \
    -o ConnectTimeout="$SSH_CONNECT_TIMEOUT" \
    -o StrictHostKeyChecking="$STRICT_HOST_KEY_CHECKING" \
    -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR \
    -T \
    "$target" \
    true >/dev/null 2>&1
}

check_ssh() {
  local user="$1"
  local host="$2"
  local target

  target="$(ssh_target "$user" "$host")"
  run_ssh_check "$target"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ssh-user)
      SSH_USER="$2"
      shift 2
      ;;
    --ssh-connect-timeout)
      SSH_CONNECT_TIMEOUT="$2"
      shift 2
      ;;
    --color)
      COLOR="$2"
      shift 2
      ;;
    --no-color)
      COLOR="never"
      shift
      ;;
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

init_colors
require_tool ssh

passed=0
failed=0

printf '%s%sWendy CI SSH check%s\n\n' "$C_CYAN" "$C_BOLD" "$C_RESET"
printf '%-24s %-12s %-15s %-10s %-10s\n' "HOSTNAME" "OS" "IP" "HOST SSH" "IP SSH"
printf '%-24s %-12s %-15s %-10s %-10s\n' "--------" "--" "--" "--------" "------"

for entry in "${CI_MACHINES[@]}"; do
  IFS='|' read -r hostname machine_os machine_ip machine_user <<< "$entry"
  effective_user="${machine_user:-$SSH_USER}"

  if check_ssh "$effective_user" "$hostname"; then
    host_status="$(status_ok)"
    passed=$((passed + 1))
  else
    host_status="$(status_failed)"
    failed=$((failed + 1))
  fi

  if check_ssh "$effective_user" "$machine_ip"; then
    ip_status="$(status_ok)"
    passed=$((passed + 1))
  else
    ip_status="$(status_failed)"
    failed=$((failed + 1))
  fi

  printf '%-24s %-12s %-15s %-10s %-10s\n' \
    "$hostname" \
    "$machine_os" \
    "$machine_ip" \
    "$host_status" \
    "$ip_status"
done

printf '\nSummary: %s%d OK%s, ' "$C_GREEN" "$passed" "$C_RESET"
if [[ "$failed" -eq 0 ]]; then
  printf '%s%d FAIL%s\n' "$C_GREEN" "$failed" "$C_RESET"
else
  printf '%s%d FAIL%s\n' "$C_RED" "$failed" "$C_RESET"
  exit 1
fi
