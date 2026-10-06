#!/bin/bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 <WendyAgentMac.app> <signing-identity-sha1>" >&2
  exit 64
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_PATH="$1"
SIGNING_IDENTITY="$2"
EXPECTED_SIGNING_IDENTITY=$(printf '%s' "$SIGNING_IDENTITY" | tr '[:lower:]' '[:upper:]')
HOST_BUNDLE_ID="sh.wendy.WendyAgentMac"
NET_PROXY_BUNDLE_ID="sh.wendy.WendyAgentMac.NetProxy"
NET_PROXY_PATH="$APP_PATH/Contents/Library/SystemExtensions/${NET_PROXY_BUNDLE_ID}.systemextension"
HOST_PROFILE="$APP_PATH/Contents/embedded.provisionprofile"
NET_PROXY_PROFILE="$NET_PROXY_PATH/Contents/embedded.provisionprofile"
VALIDATOR="$SCRIPT_DIR/ValidateMacProvisioning.py"
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT

for required_path in "$APP_PATH" "$NET_PROXY_PATH" "$HOST_PROFILE" "$NET_PROXY_PROFILE"; do
  if [[ ! -e "$required_path" ]]; then
    echo "Missing required macOS release path: $required_path" >&2
    exit 1
  fi
done

HOST_ACTUAL_BUNDLE_ID=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$APP_PATH/Contents/Info.plist")
NET_PROXY_ACTUAL_BUNDLE_ID=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$NET_PROXY_PATH/Contents/Info.plist")
if [[ "$HOST_ACTUAL_BUNDLE_ID" != "$HOST_BUNDLE_ID" ]]; then
  echo "Unexpected host bundle identifier: $HOST_ACTUAL_BUNDLE_ID" >&2
  exit 1
fi
if [[ "$NET_PROXY_ACTUAL_BUNDLE_ID" != "$NET_PROXY_BUNDLE_ID" ]]; then
  echo "Unexpected NetProxy bundle identifier: $NET_PROXY_ACTUAL_BUNDLE_ID" >&2
  exit 1
fi

"$VALIDATOR" \
  --profile "$HOST_PROFILE" \
  --bundle-id "$HOST_BUNDLE_ID" \
  --kind host \
  --signing-identity "$SIGNING_IDENTITY"
"$VALIDATOR" \
  --profile "$NET_PROXY_PROFILE" \
  --bundle-id "$NET_PROXY_BUNDLE_ID" \
  --kind net-proxy \
  --signing-identity "$SIGNING_IDENTITY"

codesign --display --entitlements :- "$APP_PATH" > "$TEMP_DIR/host-entitlements.plist" 2>/dev/null
codesign --display --entitlements :- "$NET_PROXY_PATH" > "$TEMP_DIR/net-proxy-entitlements.plist" 2>/dev/null
"$VALIDATOR" \
  --entitlements "$TEMP_DIR/host-entitlements.plist" \
  --bundle-id "$HOST_BUNDLE_ID" \
  --kind host
"$VALIDATOR" \
  --entitlements "$TEMP_DIR/net-proxy-entitlements.plist" \
  --bundle-id "$NET_PROXY_BUNDLE_ID" \
  --kind net-proxy

for code_path in "$APP_PATH" "$NET_PROXY_PATH"; do
  if ! codesign --verify --strict \
    --test-requirement "=certificate leaf = H\"$EXPECTED_SIGNING_IDENTITY\"" \
    "$code_path"; then
    echo "Code signature does not use the expected Developer ID certificate: $code_path" >&2
    exit 1
  fi
done

codesign --verify --deep --strict --verbose=2 "$APP_PATH"
echo "Validated provisioned macOS release: $APP_PATH"
