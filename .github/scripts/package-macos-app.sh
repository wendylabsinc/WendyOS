#!/bin/bash
# Publishable files are created only after notarization and Gatekeeper succeed.
set -euo pipefail
[[ $# == 3 ]] || { echo "Usage: $0 source.app new-output-directory X.Y.Z" >&2; exit 2; }
SOURCE_APP="$1"
OUTPUT_DIR="$2"
VERSION="$3"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid version' >&2; exit 2; }
[[ -d "$SOURCE_APP" && ! -e "$OUTPUT_DIR" ]] || { echo 'Source must exist and output must be new' >&2; exit 2; }
for name in WENDY_SIGNING_IDENTITY KEYCHAIN_PATH NOTARY_PROFILE APP_PROVISIONING_PROFILE_PATH NET_PROXY_PROVISIONING_PROFILE_PATH; do
  [[ -n "${!name:-}" ]] || { echo "Missing $name" >&2; exit 2; }
done
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR="$(cd "$OUTPUT_DIR" && pwd)"
WORK="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/macos-app-package.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
APP="$WORK/WendyDesktop.app"
bash "$REPO_ROOT/go/macos/Scripts/sign-macos.sh" "$SOURCE_APP" "$APP" \
  "$APP_PROVISIONING_PROFILE_PATH" "$NET_PROXY_PROVISIONING_PROFILE_PATH"
ditto -c -k --sequesterRsrc --keepParent "$APP" "$WORK/notarization.zip"
NOTARY_ARGS=(--keychain-profile "$NOTARY_PROFILE" --keychain "$KEYCHAIN_PATH")
NOTARY_STATUS=0
if ! xcrun notarytool submit "$WORK/notarization.zip" "${NOTARY_ARGS[@]}" \
    --wait --timeout 30m --output-format json > "$OUTPUT_DIR/notarization.json"; then
  NOTARY_STATUS=1
  echo 'Notarization submission failed; see notarization.json' >&2
fi
# A completed request can still be Invalid. Require Apple's explicit acceptance.
if (( NOTARY_STATUS != 0 )) || ! python3 - "$OUTPUT_DIR/notarization.json" <<'PY'
import json, sys
result = json.load(open(sys.argv[1]))
sys.exit(0 if result.get('status') == 'Accepted' else 1)
PY
then
  SUBMISSION_ID="$(python3 - "$OUTPUT_DIR/notarization.json" <<'PY'
import json, sys
try:
    print(json.load(open(sys.argv[1])).get('id', ''))
except (ValueError, OSError):
    pass
PY
)"
  if [[ -n "$SUBMISSION_ID" ]]; then
    xcrun notarytool log "$SUBMISSION_ID" "${NOTARY_ARGS[@]}" "$OUTPUT_DIR/notarization-log.json" || true
  fi
  echo 'Notarization was not accepted; no release archive created' >&2
  exit 1
fi
xcrun stapler staple "$APP"
xcrun stapler validate "$APP"
codesign --verify --deep --strict --verbose=2 "$APP"
spctl --assess --type execute --verbose=2 "$APP"
# ZIPs cannot be stapled. Repackage the app after attaching its ticket.
ARCHIVE="wendy-macos-arm64-${VERSION}.zip"
ditto -c -k --sequesterRsrc --keepParent "$APP" "$OUTPUT_DIR/$ARCHIVE"
cd "$OUTPUT_DIR"
shasum -a 256 "$ARCHIVE" > "$ARCHIVE.sha256"
