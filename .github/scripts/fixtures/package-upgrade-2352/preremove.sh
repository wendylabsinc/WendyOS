#!/usr/bin/env bash
set -euo pipefail

if [ ! -d /run/systemd/system ]; then
  exit 0
fi

if ! command -v systemctl >/dev/null 2>&1; then
  exit 0
fi

systemctl stop wendy-agent >/dev/null 2>&1 || true
