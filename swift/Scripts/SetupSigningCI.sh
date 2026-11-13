#!/bin/bash
set -euo pipefail

required=(
  DEVELOPER_ID_CERTIFICATE
  DEVELOPER_ID_KEY
  WENDY_AGENT_MAC_PROVISIONING_PROFILE
  WENDY_AGENT_MAC_NET_PROXY_PROVISIONING_PROFILE
  NOTARY_APPLE_ID
  NOTARY_TEAM_ID
  NOTARY_PASSWORD
)

for var in "${required[@]}"; do
  if [ -z "${!var:-}" ]; then
    echo "::error::Missing required macOS signing secret: ${var}"
    exit 1
  fi
done

TEMP_DIR="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
KEYCHAIN_PATH="${TEMP_DIR}/wendy-signing.keychain-db"
KEYCHAIN_PASSWORD="$(openssl rand -hex 24)"
CERT_PATH="${TEMP_DIR}/developer-id-application.cer"
KEY_PATH="${TEMP_DIR}/developer-id-application.key"
APP_PROVISIONING_PROFILE_PATH="${TEMP_DIR}/WendyAgentMac.provisionprofile"
NET_PROXY_PROVISIONING_PROFILE_PATH="${TEMP_DIR}/WendyAgentMacNetProxy.provisionprofile"
DEVELOPER_ID_CA_PATH="${TEMP_DIR}/developer-id-g2-ca.cer"
DEVELOPER_ID_CA_URL="https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer"
DEVELOPER_ID_CA_SHA256="f16cd3c54c7f83cea4bf1a3e6a0819c8aaa8e4a1528fd144715f350643d2df3a"
NOTARY_PROFILE="${NOTARY_PROFILE:-wendy-notary-profile}"
SIGNING_SETUP_COMPLETE=0

cleanup() {
  rm -f "$CERT_PATH" "$KEY_PATH" "$DEVELOPER_ID_CA_PATH"
  if [[ "$SIGNING_SETUP_COMPLETE" -ne 1 ]]; then
    rm -f "$APP_PROVISIONING_PROFILE_PATH" "$NET_PROXY_PROVISIONING_PROFILE_PATH"
    if [[ -f "$KEYCHAIN_PATH" ]]; then
      security delete-keychain "$KEYCHAIN_PATH" || true
    fi
  fi
}
trap cleanup EXIT

printf '%s' "$DEVELOPER_ID_CERTIFICATE" | base64 --decode > "$CERT_PATH"
printf '%s' "$DEVELOPER_ID_KEY" | base64 --decode > "$KEY_PATH"
printf '%s' "$WENDY_AGENT_MAC_PROVISIONING_PROFILE" \
  | base64 --decode > "$APP_PROVISIONING_PROFILE_PATH"
printf '%s' "$WENDY_AGENT_MAC_NET_PROXY_PROVISIONING_PROFILE" \
  | base64 --decode > "$NET_PROXY_PROVISIONING_PROFILE_PATH"

security create-keychain -p "$KEYCHAIN_PASSWORD" "$KEYCHAIN_PATH"
security set-keychain-settings -lut 21600 "$KEYCHAIN_PATH"
security unlock-keychain -p "$KEYCHAIN_PASSWORD" "$KEYCHAIN_PATH"

curl -fsSL "$DEVELOPER_ID_CA_URL" -o "$DEVELOPER_ID_CA_PATH"
ACTUAL_DEVELOPER_ID_CA_SHA256=$(shasum -a 256 "$DEVELOPER_ID_CA_PATH" | awk '{ print $1 }')
if [ "$ACTUAL_DEVELOPER_ID_CA_SHA256" != "$DEVELOPER_ID_CA_SHA256" ]; then
  echo "::error::Apple Developer ID intermediate certificate checksum mismatch"
  exit 1
fi
security import "$DEVELOPER_ID_CA_PATH" -k "$KEYCHAIN_PATH"

security import "$CERT_PATH" \
  -k "$KEYCHAIN_PATH" \
  -T /usr/bin/codesign \
  -T /usr/bin/security \
  -T /usr/bin/xcrun
security import "$KEY_PATH" \
  -k "$KEYCHAIN_PATH" \
  -T /usr/bin/codesign \
  -T /usr/bin/security \
  -T /usr/bin/xcrun
security set-key-partition-list \
  -S apple-tool:,apple:,codesign: \
  -s \
  -k "$KEYCHAIN_PASSWORD" \
  "$KEYCHAIN_PATH"

# codesign requires the identity's keychain to be on the user search list even
# when --keychain names it explicitly. Preserve existing entries; deleting the
# ephemeral keychain after the build removes only this added entry.
KEYCHAIN_SEARCH_LIST=("$KEYCHAIN_PATH")
while IFS= read -r keychain; do
  keychain="${keychain#*\"}"
  keychain="${keychain%\"*}"
  if [[ -n "$keychain" && "$keychain" != "$KEYCHAIN_PATH" ]]; then
    KEYCHAIN_SEARCH_LIST+=("$keychain")
  fi
done < <(security list-keychains -d user)
security list-keychains -d user -s "${KEYCHAIN_SEARCH_LIST[@]}"

SIGNING_IDENTITY=$(security find-identity -v -p codesigning "$KEYCHAIN_PATH" | awk '/Developer ID Application/ { print $2; exit }')
if [[ ! "$SIGNING_IDENTITY" =~ ^[[:xdigit:]]{40}$ ]]; then
  echo "::error::Could not find a Developer ID Application identity in the imported certificate"
  exit 1
fi

xcrun notarytool store-credentials "$NOTARY_PROFILE" \
  --apple-id "$NOTARY_APPLE_ID" \
  --team-id "$NOTARY_TEAM_ID" \
  --password "$NOTARY_PASSWORD" \
  --keychain "$KEYCHAIN_PATH"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "keychain_path=$KEYCHAIN_PATH"
    echo "signing_identity=$SIGNING_IDENTITY"
    echo "notary_profile=$NOTARY_PROFILE"
    echo "app_provisioning_profile_path=$APP_PROVISIONING_PROFILE_PATH"
    echo "net_proxy_provisioning_profile_path=$NET_PROXY_PROVISIONING_PROFILE_PATH"
  } >> "$GITHUB_OUTPUT"
fi

SIGNING_SETUP_COMPLETE=1
echo "Imported Developer ID signing identity"
