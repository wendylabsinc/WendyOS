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
  # System extensions are replaced only by a newer bundle version. Give each
  # local build a monotonic timestamp instead of reusing a fixed dev version.
  VERSION="${VERSION:-$(date -u +%Y.%m.%d-%H%M%S-dev)}"
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
SYSTEM_EXTENSION_ENTITLEMENTS_PATH="$SWIFT_DIR/WendyAgentMac/WendyAgentMacNetProxy/WendyAgentMacNetProxy.entitlements"
RELEASE_VALIDATOR_PATH="$SCRIPT_DIR/ValidateMacRelease.sh"
LAUNCH_SMOKE_TEST_PATH="$SCRIPT_DIR/SmokeTestMacReleaseLaunch.sh"
APP_PROVISIONING_PROFILE_PATH="${APP_PROVISIONING_PROFILE_PATH:-}"
NET_PROXY_PROVISIONING_PROFILE_PATH="${NET_PROXY_PROVISIONING_PROFILE_PATH:-}"
RUNTIME_RESOURCES_PATH="$SWIFT_DIR/WendyAgentMac/Resources/runtime"
RUNTIME_KERNEL_PATH="$RUNTIME_RESOURCES_PATH/vmlinuz-arm64"
RUNTIME_INITRAMFS_PATH="$RUNTIME_RESOURCES_PATH/initramfs-arm64.img"

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

for required_file in "$ENTITLEMENTS_PATH" "$SYSTEM_EXTENSION_ENTITLEMENTS_PATH"; do
  if [ ! -f "$required_file" ]; then
    echo "Missing entitlements file: $required_file" >&2
    exit 1
  fi
done
if [[ "$DEV_BUILD" -ne 1 ]]; then
  for required_file in "$RELEASE_VALIDATOR_PATH" "$LAUNCH_SMOKE_TEST_PATH"; do
    if [[ ! -x "$required_file" ]]; then
      echo "Missing executable release validation script: $required_file" >&2
      exit 1
    fi
  done
  for profile_path in "$APP_PROVISIONING_PROFILE_PATH" "$NET_PROXY_PROVISIONING_PROFILE_PATH"; do
    if [[ -z "$profile_path" || ! -r "$profile_path" ]]; then
      echo "Missing readable Developer ID provisioning profile: ${profile_path:-<unset>}" >&2
      exit 1
    fi
  done
fi
for runtime_artifact in "$RUNTIME_KERNEL_PATH" "$RUNTIME_INITRAMFS_PATH"; do
  if [ ! -r "$runtime_artifact" ]; then
    echo "Missing runtime guest artifact: $runtime_artifact" >&2
    echo "Run WendyAgentMac/RuntimeGuest/build.sh before packaging." >&2
    exit 1
  fi
done

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
  WENDY_AGENT_VERSION="$VERSION" \
  CODE_SIGNING_ALLOWED=NO \
  CODE_SIGNING_REQUIRED=NO \
  -skipMacroValidation \
  | xcbeautify_or_cat

ditto "$BUILT_APP_PATH" "$APP_PATH"

SYSTEM_EXTENSION_PATH="$APP_PATH/Contents/Library/SystemExtensions/sh.wendy.WendyAgentMac.NetProxy.systemextension"

if [[ "$DEV_BUILD" -ne 1 ]]; then
  cp "$APP_PROVISIONING_PROFILE_PATH" "$APP_PATH/Contents/embedded.provisionprofile"
  cp \
    "$NET_PROXY_PROVISIONING_PROFILE_PATH" \
    "$SYSTEM_EXTENSION_PATH/Contents/embedded.provisionprofile"
fi

while IFS= read -r nested_code; do
  sign_path "$nested_code"
done < <(find "$APP_PATH/Contents" \
  \( -name "*.app" -o -name "*.framework" -o -name "*.xpc" -o -name "*.appex" -o -name "*.dylib" \) \
  -print | sort -r)

sign_path "$SYSTEM_EXTENSION_PATH" "$SYSTEM_EXTENSION_ENTITLEMENTS_PATH"
sign_path "$APP_PATH" "$ENTITLEMENTS_PATH"

codesign --verify --deep --strict --verbose=2 "$APP_PATH"
if [[ "$DEV_BUILD" -ne 1 ]]; then
  "$RELEASE_VALIDATOR_PATH" "$APP_PATH" "$SIGNING_IDENTITY"
fi

if [[ "$DEV_BUILD" -ne 1 ]]; then
  ditto -c -k --sequesterRsrc --keepParent "$APP_PATH" "$NOTARY_ZIP"

  xcrun notarytool submit "$NOTARY_ZIP" \
    --keychain-profile "$NOTARY_PROFILE" \
    --keychain "$KEYCHAIN_PATH" \
    --wait

  xcrun stapler staple -v "$APP_PATH"
  xcrun stapler validate "$APP_PATH"
  spctl -a -vv --type exec "$APP_PATH"
  "$RELEASE_VALIDATOR_PATH" "$APP_PATH" "$SIGNING_IDENTITY"
  "$LAUNCH_SMOKE_TEST_PATH" "$APP_PATH"
fi

ditto -c -k --sequesterRsrc --keepParent \
  "$APP_PATH" \
  "$ARTIFACT_PATH"

if [[ "$DEV_BUILD" -ne 1 ]]; then
  ARTIFACT_VALIDATION_DIR=$(mktemp -d)
  trap 'rm -rf "$ARTIFACT_VALIDATION_DIR"' EXIT
  ditto -x -k "$ARTIFACT_PATH" "$ARTIFACT_VALIDATION_DIR"
  "$RELEASE_VALIDATOR_PATH" \
    "$ARTIFACT_VALIDATION_DIR/$APP_NAME" \
    "$SIGNING_IDENTITY"
  rm -rf "$ARTIFACT_VALIDATION_DIR"
  trap - EXIT
fi

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
