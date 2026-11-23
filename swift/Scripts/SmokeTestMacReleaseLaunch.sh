#!/bin/bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: $0 <WendyAgentMac.app>" >&2
  exit 64
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_PATH="$1"
BUNDLE_ID="sh.wendy.WendyAgentMac"
WORK_DIR=$(mktemp -d)
OBSERVER="$WORK_DIR/app-process"

is_running() {
  "$OBSERVER" running "$APP_PATH"
}

stop_candidate() {
  "$OBSERVER" quit "$APP_PATH"
  for _ in {1..10}; do
    if ! is_running; then return 0; fi
    sleep 1
  done
  echo "Packaged app did not stop: $APP_PATH" >&2
  return 1
}

cleanup() {
  if [[ -x "$OBSERVER" ]]; then stop_candidate >/dev/null 2>&1 || true; fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT
xcrun swiftc "$SCRIPT_DIR/MacReleaseAppProcess.swift" -o "$OBSERVER"

# The app can hand itself to launchd or defer to another running instance, so
# show which copies ran and what the app logged before reporting a failure.
diagnose() {
  {
    echo "WendyAgentMac processes:"
    "$OBSERVER" diagnose "$APP_PATH"
    echo "WendyAgentMac log since launch:"
    log show --style compact --info --start "$launched_at" --predicate "subsystem == \"$BUNDLE_ID\"" 2>&1 || true
  } >&2
}

stop_candidate
if ! "$OBSERVER" exclusive "$APP_PATH"; then
  echo "Another WendyAgentMac bundle is running; use an idle runner for release validation." >&2
  "$OBSERVER" diagnose "$APP_PATH" >&2
  exit 1
fi
launched_at=$(date '+%Y-%m-%d %H:%M:%S')
open -n -g "$APP_PATH"

# A successful launch may briefly disappear while it hands off to launchd.
# Require three consecutive observations of this bundle, allowing that handoff
# to finish rather than treating its first process's exit as an app crash.
stable=0
for _ in {1..60}; do
  if is_running; then
    stable=$((stable + 1))
    if ((stable >= 4)); then
      stop_candidate
      echo "Validated packaged macOS app launch: $APP_PATH"
      exit 0
    fi
  else
    stable=0
  fi
  sleep 1
done

echo "Packaged WendyAgentMac did not remain running within 60 seconds: $APP_PATH" >&2
diagnose
exit 1
