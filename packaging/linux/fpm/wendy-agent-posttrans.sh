#!/usr/bin/env bash
set -euo pipefail

# RPM only. During an upgrade rpm runs the old version's preremove after this
# version's postinstall. Versions whose preremove stopped the agent on every
# upgrade would leave it stopped, so start it again once the transaction ends.

if [ ! -d /run/systemd/system ]; then
  exit 0
fi

if ! command -v systemctl >/dev/null 2>&1; then
  exit 0
fi

if systemctl is-enabled wendy-agent >/dev/null 2>&1 &&
  ! systemctl is-active wendy-agent >/dev/null 2>&1; then
  systemctl start wendy-agent >/dev/null 2>&1 || true
fi
