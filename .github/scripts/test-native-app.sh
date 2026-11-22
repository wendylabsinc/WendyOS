#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/../../go/macos"
xcodegen generate
args=(-project WendyDesktop.xcodeproj -configuration Debug -derivedDataPath Build/DerivedData
      -resultBundlePath Build/RegressionResults.xcresult CODE_SIGNING_ALLOWED=NO)
case "$1" in
  macos)
    args+=(-scheme WendyDesktop -destination 'platform=macOS')
    ;;
  ios)
    # Select an installed iOS >=26 runtime, without depending on runner device names.
    # Take the first iPhone with a phone-shaped screen: the UI tests expect a
    # phone layout with a tab bar, which a foldable such as iPhone Duo (about
    # 1.45:1 unfolded) does not show. Fall back to the first iPhone if no device
    # type profile gives a screen size.
    read -r runtime device_type < <(xcrun simctl list runtimes --json | python3 -c '
import json, os, plistlib, sys
available = [r for r in json.load(sys.stdin)["runtimes"]
             if r["isAvailable"] and not r.get("isInternal", False)
             and ".iOS-" in r["identifier"] and int(r["version"].split(".")[0]) >= 26]
assert available, "No supported iOS simulator runtime is installed"
runtime = max(available, key=lambda r: tuple(map(int, r["version"].split("."))))
phones = [d for d in runtime["supportedDeviceTypes"] if d["productFamily"] == "iPhone"]
assert phones, "The iOS simulator runtime supports no iPhone"
def screen(device):
    try:
        with open(os.path.join(device["bundlePath"], "Contents/Resources/profile.plist"), "rb") as f:
            profile = plistlib.load(f)
        return int(profile["mainScreenWidth"]), int(profile["mainScreenHeight"])
    except (KeyError, OSError, TypeError, ValueError, plistlib.InvalidFileException):
        return None
def phone_shaped(device):
    size = screen(device)
    return size is not None and max(size) >= 1.9 * min(size)
for device in phones:
    print("iPhone device type:", device["name"], screen(device), file=sys.stderr)
phone = next((d for d in phones if phone_shaped(d)), phones[0])
print(runtime["identifier"], phone["identifier"])
')
    echo "iOS simulator: $device_type on $runtime"
    device=$(xcrun simctl create 'Wendy regression tests' "$device_type" "$runtime")
    trap 'xcrun simctl delete "$device"' EXIT
    args+=(-scheme WendyMobile -destination "platform=iOS Simulator,id=$device"
           -skip-testing:WendyMobileTests/PhysicalRoomCaptureTests
           -skip-testing:WendyMobileTests/MobileTests/testLiveMeshDiscoveryManagementAndResume
           -skip-testing:WendyMobileTests/CloudChatTests/testKeychainSaveReplaceRemoveKeepsProvidersSeparate)
    # RoomPlan/LiDAR startup, memory pressure and signed-in live mesh checks
    # require a physical device and credentials, outside this simulator job.
    # The Keychain round trip needs a signed app: this unsigned simulator build
    # has no Keychain entitlement, so SecItem calls fail with -34018.
    ;;
  *) echo "Expected macos or ios" >&2; exit 2 ;;
esac
xcodebuild "${args[@]}" test
xcrun xcresulttool get test-results summary --path Build/RegressionResults.xcresult > Build/regression-summary.json
python3 - <<'PY'
import json
from pathlib import Path
result = json.loads(Path("Build/regression-summary.json").read_text())
assert result["totalTestCount"] > 0, result
assert result["failedTests"] == 0 and result["skippedTests"] == 0, result
assert result["result"] == "Passed", result
PY
