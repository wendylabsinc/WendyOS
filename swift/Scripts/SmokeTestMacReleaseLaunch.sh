#!/bin/bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: $0 <WendyAgentMac.app>" >&2
  exit 64
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_PATH="$1"
BUNDLE_ID="sh.wendy.WendyAgentMac"

is_running() {
  [[ "$(osascript -e "application id \"$BUNDLE_ID\" is running" 2>/dev/null || true)" == "true" ]]
}

cleanup() {
  "$SCRIPT_DIR/Quit.sh" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The app can hand itself to launchd or defer to another running instance, so
# show which copies ran and what the app logged before reporting a failure.
diagnose() {
  {
    echo "WendyAgentMac processes:"
    pgrep -ilf WendyAgentMac || echo "  none"
    echo "WendyAgentMac log since launch:"
    log show --style compact --info --start "$launched_at" --predicate "subsystem == \"$BUNDLE_ID\"" 2>&1 || true
  } >&2
}

cleanup
launched_at=$(date '+%Y-%m-%d %H:%M:%S')
open -n -g "$APP_PATH"

for _ in {1..20}; do
  if is_running; then
    sleep 3
    if is_running; then
      echo "Validated packaged macOS app launch: $APP_PATH"
      exit 0
    fi
    echo "WendyAgentMac exited immediately after launch" >&2
    diagnose
    exit 1
  fi
  sleep 1
done

echo "WendyAgentMac did not launch within 20 seconds" >&2
diagnose
exit 1
