#!/usr/bin/env bash
set -euo pipefail

if [ ! -d /run/systemd/system ]; then
  exit 0
fi

if ! command -v systemctl >/dev/null 2>&1; then
  exit 0
fi

systemctl daemon-reload || true

if systemctl is-enabled wendy-agent >/dev/null 2>&1; then
  systemctl try-restart wendy-agent || true
else
  systemctl enable --now wendy-agent || true
fi
