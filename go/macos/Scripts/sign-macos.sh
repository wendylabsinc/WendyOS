#!/bin/bash
# Sign an existing build with matching Developer ID provisioning profiles.
set -euo pipefail
if [[ $# != 4 ]]; then
  echo "Usage: $0 source.app destination.app app.provisionprofile network.provisionprofile" >&2
  exit 2
fi
SOURCE_APP="$1"
SIGNED_APP="$2"
APP_PROFILE="$3"
NETWORK_PROFILE="$4"
IDENTITY="${WENDY_SIGNING_IDENTITY:-Developer ID Application: Wendy Labs Inc. (3YVC792H3S)}"
[[ -d "$SOURCE_APP" && ! -e "$SIGNED_APP" ]] || { echo "Source must exist and destination must be new." >&2; exit 1; }
SIGNING_WORK="$(mktemp -d)"
trap 'rm -rf "$SIGNING_WORK"' EXIT
python3 - "$APP_PROFILE" "$NETWORK_PROFILE" "$SIGNING_WORK" <<'PY'
import datetime, pathlib, plistlib, subprocess, sys
for profile, bundle, output in [(sys.argv[1], 'dev.wendy.desktop', 'app'), (sys.argv[2], 'dev.wendy.desktop.network', 'network')]:
    value = plistlib.loads(subprocess.check_output(['security', 'cms', '-D', '-i', profile], stderr=subprocess.DEVNULL))
    allowed = value['Entitlements']
    team = value['TeamIdentifier'][0]
    assert value['ExpirationDate'] > datetime.datetime.now(datetime.timezone.utc).replace(tzinfo=None), 'Expired profile'
    assert allowed['com.apple.application-identifier'] == team + '.' + bundle, 'Wrong bundle profile'
    assert 'packet-tunnel-provider-systemextension' in allowed['com.apple.developer.networking.networkextension'], 'Profile does not allow the system extension'
    entitlements = {
        'com.apple.application-identifier': team + '.' + bundle,
        'com.apple.developer.team-identifier': team,
        'com.apple.developer.networking.networkextension': ['packet-tunnel-provider-systemextension'],
        'com.apple.security.network.client': True,
        'com.apple.security.network.server': True,
    }
    if output == 'app':
        assert allowed.get('com.apple.developer.system-extension.install'), 'Missing system extension capability'
        entitlements['com.apple.developer.system-extension.install'] = True
        entitlements['com.apple.developer.aps-environment'] = allowed['com.apple.developer.aps-environment']
    with open(pathlib.Path(sys.argv[3]) / (output + '.plist'), 'wb') as file:
        plistlib.dump(entitlements, file)
PY
mkdir -p "$(dirname "$SIGNED_APP")"
ditto "$SOURCE_APP" "$SIGNED_APP"
# xcodebuild test can leave its test bundle in a Debug product.
rm -rf "$SIGNED_APP/Contents/PlugIns/WendyDesktopTests.xctest"
NETWORK_APP="$SIGNED_APP/Contents/Library/SystemExtensions/dev.wendy.desktop.network.systemextension"
[[ -d "$NETWORK_APP" ]] || { echo "Missing bundled VPN extension." >&2; exit 1; }
if [[ -n "${WENDY_BUILD_NUMBER:-}" ]]; then
  [[ "$WENDY_BUILD_NUMBER" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]] || { echo "Invalid build number" >&2; exit 1; }
  /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $WENDY_BUILD_NUMBER" "$SIGNED_APP/Contents/Info.plist"
  /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $WENDY_BUILD_NUMBER" "$NETWORK_APP/Contents/Info.plist"
fi
cp "$APP_PROFILE" "$SIGNED_APP/Contents/embedded.provisionprofile"
cp "$NETWORK_PROFILE" "$NETWORK_APP/Contents/embedded.provisionprofile"
codesign --force --options runtime --timestamp --sign "$IDENTITY" "$SIGNED_APP/Contents/Frameworks/libusb-1.0.dylib"
codesign --force --options runtime --timestamp --sign "$IDENTITY" "$SIGNED_APP/Contents/MacOS/wendy"
codesign --force --options runtime --timestamp --sign "$IDENTITY" --entitlements "$SIGNING_WORK/network.plist" "$NETWORK_APP"
codesign --force --options runtime --timestamp --sign "$IDENTITY" --entitlements "$SIGNING_WORK/app.plist" "$SIGNED_APP"
codesign --verify --deep --strict --verbose=2 "$SIGNED_APP"
echo "Signed app: $SIGNED_APP"
