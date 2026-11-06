#!/usr/bin/env bash
set -euo pipefail

if [ ! -d /run/systemd/system ]; then
  exit 0
fi

if ! command -v systemctl >/dev/null 2>&1; then
  exit 0
fi

# Stop the agent only when the package is removed. On an upgrade dpkg passes
# "upgrade" (or "failed-upgrade") and rpm passes the number of versions that
# remain; the new version's postinstall restarts the agent instead.
case "${1:-}" in
  upgrade | failed-upgrade | [1-9]*)
    exit 0
    ;;
esac

systemctl stop wendy-agent >/dev/null 2>&1 || true
