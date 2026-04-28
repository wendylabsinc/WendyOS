#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$SWIFT_DIR"

DEV_BUILD=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dev)
      DEV_BUILD=1
      ;;
    *)
      echo "Unknown argument: $1" >&2
      echo "Usage: $0 [--dev]" >&2
      exit 1
      ;;
  esac
  shift
done

if [[ "$DEV_BUILD" -eq 1 ]]; then
  VERSION="${VERSION:-0000.00.00-000000-dev}"
else
  : "${VERSION:?VERSION is required}"
fi

if [[ "$VERSION" =~ ^([0-9]{4})\.([0-9]{2})\.([0-9]{2})(-([0-9]{6}))?([-.].*)?$ ]]; then
  APPLE_MARKETING_VERSION="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}"
  APPLE_CURRENT_PROJECT_VERSION="${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]}${BASH_REMATCH[5]:-000000}"
else
  echo "VERSION must start with YYYY.MM.DD and may include -HHMMSS and a suffix, got: $VERSION" >&2
  exit 1
fi

APP_NAME="WendyAgentMac.app"
BUILD_CONFIGURATION="Release"
OUTPUT_DIR="${OUTPUT_DIR:-$SWIFT_DIR/Build}"
DERIVED_DATA_PATH="${DERIVED_DATA_PATH:-$SWIFT_DIR/Build/Xcode}"
TEMP_DIR="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
APP_PATH="${OUTPUT_DIR}/${APP_NAME}"
NOTARY_ZIP="${TEMP_DIR}/WendyAgentMac-notary.zip"
ARTIFACT_NAME="wendy-agent-macos-arm64-${VERSION}.zip"
ARTIFACT_PATH="${OUTPUT_DIR}/${ARTIFACT_NAME}"
NOTARY_PROFILE="${NOTARY_PROFILE:-wendy-notary-profile}"
ENTITLEMENTS_PATH="$SWIFT_DIR/WendyAgentMac/Support/WendyAgentMac.entitlements"

if [[ "$DEV_BUILD" -eq 1 ]]; then
  BUILD_CONFIGURATION="Debug"
fi

BUILT_APP_PATH="${DERIVED_DATA_PATH}/Build/Products/${BUILD_CONFIGURATION}/${APP_NAME}"

find_signing_identity() {
  if [ -n "${KEYCHAIN_PATH:-}" ]; then
    security find-identity -v -p codesigning "$KEYCHAIN_PATH"
  else
    security find-identity -v -p codesigning
  fi
}

if [ -z "${SIGNING_IDENTITY:-}" ]; then
  if [[ "$DEV_BUILD" -eq 1 ]]; then
    SIGNING_IDENTITY=$(find_signing_identity | awk -F '"' '/Apple Development/ { print $2; exit }')
  else
    SIGNING_IDENTITY=$(find_signing_identity | awk -F '"' '/Developer ID Application/ { print $2; exit }')
  fi
fi

if [ -z "${SIGNING_IDENTITY:-}" ]; then
  if [[ "$DEV_BUILD" -eq 1 ]]; then
    echo "Missing SIGNING_IDENTITY and could not auto-detect an Apple Development identity" >&2
  else
    echo "Missing SIGNING_IDENTITY and could not auto-detect a Developer ID Application identity" >&2
  fi
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

  if [[ "$DEV_BUILD" -ne 1 ]]; then
    command+=(
      --options runtime
      --timestamp
    )
  fi

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

mkdir -p "$OUTPUT_DIR"

rm -rf "$APP_PATH"
rm -rf "$NOTARY_ZIP"
rm -f "$ARTIFACT_PATH"

if [[ "$DEV_BUILD" -eq 1 ]]; then
  echo "Preserving derived data for incremental dev build: $DERIVED_DATA_PATH"
else
  rm -rf "$DERIVED_DATA_PATH"
fi

xcodebuild build \
  -workspace WendyAgent.xcworkspace \
  -scheme WendyAgentMac \
  -configuration "$BUILD_CONFIGURATION" \
  -destination 'generic/platform=macOS' \
  -derivedDataPath "$DERIVED_DATA_PATH" \
  MARKETING_VERSION="$APPLE_MARKETING_VERSION" \
  CURRENT_PROJECT_VERSION="$APPLE_CURRENT_PROJECT_VERSION" \
  CODE_SIGNING_ALLOWED=NO \
  CODE_SIGNING_REQUIRED=NO \
  -skipMacroValidation \
  | xcbeautify_or_cat

ditto "$BUILT_APP_PATH" "$APP_PATH"

while IFS= read -r nested_code; do
  sign_path "$nested_code"
done < <(find "$APP_PATH/Contents" \
  \( -name "*.app" -o -name "*.framework" -o -name "*.xpc" -o -name "*.appex" -o -name "*.dylib" \) \
  -print | sort -r)

sign_path "$APP_PATH" "$ENTITLEMENTS_PATH"

codesign --verify --deep --strict --verbose=2 "$APP_PATH"

if [[ "$DEV_BUILD" -ne 1 ]]; then
  ditto -c -k --sequesterRsrc --keepParent "$APP_PATH" "$NOTARY_ZIP"

  xcrun notarytool submit "$NOTARY_ZIP" \
    --keychain-profile "$NOTARY_PROFILE" \
    --wait

  xcrun stapler staple -v "$APP_PATH"
  xcrun stapler validate "$APP_PATH"
  spctl -a -vv --type exec "$APP_PATH"
fi

ditto -c -k --sequesterRsrc --keepParent \
  "$APP_PATH" \
  "$ARTIFACT_PATH"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "apple_marketing_version=$APPLE_MARKETING_VERSION"
    echo "apple_current_project_version=$APPLE_CURRENT_PROJECT_VERSION"
    echo "app_name=$APP_NAME"
    echo "app_path=$APP_PATH"
    echo "artifact_name=$ARTIFACT_NAME"
    echo "artifact_path=$ARTIFACT_PATH"
  } >> "$GITHUB_OUTPUT"
fi

echo "Created macOS app artifact: $ARTIFACT_PATH"
