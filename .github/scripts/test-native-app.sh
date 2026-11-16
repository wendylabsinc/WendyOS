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
    read -r runtime device_type < <(xcrun simctl list runtimes --json | python3 -c '
import json, sys
available = [r for r in json.load(sys.stdin)["runtimes"]
             if r["isAvailable"] and not r.get("isInternal", False)
             and ".iOS-" in r["identifier"] and int(r["version"].split(".")[0]) >= 26]
assert available, "No supported iOS simulator runtime is installed"
runtime = max(available, key=lambda r: tuple(map(int, r["version"].split("."))))
phone = next(d for d in runtime["supportedDeviceTypes"] if d["productFamily"] == "iPhone")
print(runtime["identifier"], phone["identifier"])
')
    device=$(xcrun simctl create 'Wendy regression tests' "$device_type" "$runtime")
    trap 'xcrun simctl delete "$device"' EXIT
    args+=(-scheme WendyMobile -destination "platform=iOS Simulator,id=$device"
           -skip-testing:WendyMobileTests/PhysicalRoomCaptureTests
           -skip-testing:WendyMobileTests/MobileTests/testLiveMeshDiscoveryManagementAndResume)
    # RoomPlan/LiDAR startup, memory pressure and signed-in live mesh checks
    # require a physical device and credentials, outside this simulator job.
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
