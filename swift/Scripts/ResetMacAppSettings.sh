#!/usr/bin/env bash
set -euo pipefail

bundle_id="sh.wendy.WendyAgentMac"
prefs_plist="$HOME/Library/Preferences/${bundle_id}.plist"

echo "Resetting settings for ${bundle_id}"

if defaults delete "$bundle_id" >/dev/null 2>&1; then
  echo "Deleted defaults domain ${bundle_id}"
else
  echo "Defaults domain ${bundle_id} did not exist"
fi

if [[ -f "$prefs_plist" ]]; then
  rm -f "$prefs_plist"
  echo "Removed $prefs_plist"
else
  echo "Preferences plist not found at $prefs_plist"
fi

killall cfprefsd >/dev/null 2>&1 || true

echo "Done"
