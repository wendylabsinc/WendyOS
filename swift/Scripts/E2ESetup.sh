#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Prepare the current host for WendyAgent Swift E2E tests by dispatching to the
platform-specific setup script.

Options:
  --help, -h  Show this help message.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
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

case "$(uname -s)" in
  Darwin)
    exec bash "$SCRIPT_DIR/E2ESetup.macOS.sh"
    ;;
  Linux)
    if command -v lsb_release >/dev/null 2>&1; then
      distribution="$(lsb_release -is)"
    else
      distribution="$(. /etc/os-release && printf '%s' "${ID:-}")"
    fi

    case "${distribution,,}" in
      ubuntu)
        exec bash "$SCRIPT_DIR/E2ESetup.ubuntu.sh"
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
