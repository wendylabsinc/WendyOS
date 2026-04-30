#!/usr/bin/env bash
set -euo pipefail

bundle_id="sh.wendy.WendyAgentMac"

if [[ "$(osascript -e "application id \"$bundle_id\" is running")" == "true" ]]; then
  osascript -e "tell application id \"$bundle_id\" to quit"

  for attempt in {1..10}; do
    if [[ "$(osascript -e "application id \"$bundle_id\" is running")" != "true" ]]; then
      exit 0
    fi

    echo "$bundle_id is still running; waiting... ($attempt/10)"
    sleep 1
  done
fi

if [[ "$(osascript -e "application id \"$bundle_id\" is running")" == "true" ]]; then
  echo "Error: $bundle_id is still running after quit request." >&2
  exit 1
fi
