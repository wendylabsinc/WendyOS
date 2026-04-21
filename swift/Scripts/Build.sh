#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$SWIFT_DIR"

: "${VERSION:?VERSION is required}"

APP_NAME="WendyAgentMac.app"
OUTPUT_DIR="${OUTPUT_DIR:-$SWIFT_DIR/Build}"
DERIVED_DATA_PATH="${DERIVED_DATA_PATH:-$SWIFT_DIR/Build/Xcode}"
TEMP_DIR="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
ARCHIVE_PATH="${ARCHIVE_PATH:-$SWIFT_DIR/Build/WendyAgentMac.xcarchive}"
APP_PATH="${OUTPUT_DIR}/${APP_NAME}"
NOTARY_ZIP="${TEMP_DIR}/WendyAgentMac-notary.zip"
ARTIFACT_NAME="wendy-agent-macos-arm64-${VERSION}.zip"
ARTIFACT_PATH="${OUTPUT_DIR}/${ARTIFACT_NAME}"
NOTARY_PROFILE="${NOTARY_PROFILE:-wendy-notary-profile}"
ENTITLEMENTS_PATH="$SWIFT_DIR/WendyAgentMac/Support/WendyAgentMac.entitlements"

find_signing_identity() {
  if [ -n "${KEYCHAIN_PATH:-}" ]; then
    security find-identity -v -p codesigning "$KEYCHAIN_PATH"
  else
    security find-identity -v -p codesigning
  fi
}

if [ -z "${SIGNING_IDENTITY:-}" ]; then
  SIGNING_IDENTITY=$(find_signing_identity | awk -F '"' '/Developer ID Application/ { print $2; exit }')
fi

if [ -z "${SIGNING_IDENTITY:-}" ]; then
  echo "Missing SIGNING_IDENTITY and could not auto-detect a Developer ID Application identity" >&2
  exit 1
fi

if [ ! -f "$ENTITLEMENTS_PATH" ]; then
  echo "Missing entitlements file: $ENTITLEMENTS_PATH" >&2
  exit 1
fi

sign_path() {
  local path="$1"
  local entitlements_path="${2:-}"
  local command=(
    codesign
    --force
    --sign "$SIGNING_IDENTITY"
  )

  if [ -n "${KEYCHAIN_PATH:-}" ]; then
    command+=(--keychain "$KEYCHAIN_PATH")
  fi

  command+=(
    --options runtime
    --timestamp
  )

  if [ -n "$entitlements_path" ]; then
    command+=(--entitlements "$entitlements_path")
  fi

  command+=("$path")

  "${command[@]}"
}

xcbeautify_or_cat() {
  if command -v xcbeautify >/dev/null 2>&1; then
    if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
      xcbeautify --renderer github-actions
    else
      xcbeautify
    fi
  else
    cat
  fi
}

mkdir -p "$OUTPUT_DIR" "$DERIVED_DATA_PATH"
rm -rf "$ARCHIVE_PATH" "$DERIVED_DATA_PATH" "$APP_PATH" "$NOTARY_ZIP"
mkdir -p "$DERIVED_DATA_PATH"
rm -f "$ARTIFACT_PATH"

xcodebuild archive \
  -workspace WendyAgent.xcworkspace \
  -scheme WendyAgentMac \
  -configuration Release \
  -destination 'generic/platform=macOS' \
  -archivePath "$ARCHIVE_PATH" \
  -derivedDataPath "$DERIVED_DATA_PATH" \
  ARCHS="arm64" \
  ONLY_ACTIVE_ARCH=NO \
  CODE_SIGNING_ALLOWED=NO \
  CODE_SIGNING_REQUIRED=NO \
  -skipMacroValidation \
  | xcbeautify_or_cat

ditto "$ARCHIVE_PATH/Products/Applications/$APP_NAME" "$APP_PATH"

while IFS= read -r nested_code; do
  sign_path "$nested_code"
done < <(find "$APP_PATH/Contents" \
  \( -name "*.app" -o -name "*.framework" -o -name "*.xpc" -o -name "*.appex" -o -name "*.dylib" \) \
  -print | sort -r)

sign_path "$APP_PATH" "$ENTITLEMENTS_PATH"

codesign --verify --deep --strict --verbose=2 "$APP_PATH"

ditto -c -k --sequesterRsrc --keepParent "$APP_PATH" "$NOTARY_ZIP"

xcrun notarytool submit "$NOTARY_ZIP" \
  --keychain-profile "$NOTARY_PROFILE" \
  --wait

xcrun stapler staple -v "$APP_PATH"
xcrun stapler validate "$APP_PATH"
spctl -a -vv --type exec "$APP_PATH"

ditto -c -k --sequesterRsrc --keepParent \
  "$APP_PATH" \
  "$ARTIFACT_PATH"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "app_name=$APP_NAME"
    echo "app_path=$APP_PATH"
    echo "artifact_name=$ARTIFACT_NAME"
    echo "artifact_path=$ARTIFACT_PATH"
  } >> "$GITHUB_OUTPUT"
fi

echo "Created macOS app artifact: $ARTIFACT_PATH"
