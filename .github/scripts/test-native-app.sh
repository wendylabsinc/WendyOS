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
    # These navigation assertions exercise a compact iPhone layout. The newest
    # installed runtime may support only iPhone Duo, whose layout differs.
    read -r runtime device_type < <(xcrun simctl list runtimes --json |
      python3 ../../.github/scripts/select-ios-simulator.py)
    echo "iOS simulator: $device_type on $runtime"
    device=$(xcrun simctl create 'Wendy regression tests' "$device_type" "$runtime")
    trap 'xcrun simctl delete "$device"' EXIT
    xcrun simctl boot "$device"
    xcrun simctl bootstatus "$device" -b
    args+=(-scheme WendyMobile -destination "platform=iOS Simulator,id=$device"
           -skip-testing:WendyMobileTests/PhysicalRoomCaptureTests
           -skip-testing:WendyMobileTests/MobileTests/testLiveMeshDiscoveryManagementAndResume
           -skip-testing:WendyMobileTests/CloudChatTests/testKeychainSaveReplaceRemoveKeepsProvidersSeparate)
    # RoomPlan/LiDAR startup, memory pressure and signed-in live mesh checks
    # require a physical device and credentials, outside this simulator job.
    # The Keychain round trip needs a signed app: this unsigned simulator build
    # has no Keychain entitlement, so SecItem calls fail with -34018.
    # These opt-in UI tests need a live device, credentials, or a separately
    # started local fixture. Keep unexpected skips fatal in the selected suite.
    args+=(-skip-testing:WendyMobileUITests/DeviceAppearanceUITests
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testDeviceBottomRowsClearFloatingBars
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testCloudModelSetupWithoutDeviceModel
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testCameraFullscreenAndSupportedControls
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testActivityClipSelectionArchiveAndRestore
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testLiveDeviceLogsAndReturnToInventory
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testAdvertisedSecureBoardOffersSignInOnSignedOutPhone
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testManageOrganizationPresentsSignInBrowser
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testDeviceAppLifecycleLogsAndWebPage
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testDeviceAppLifecycleAndLogsOverPrivateSocket
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testAppAndLogsRecoverAfterDeviceDisconnects
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testInstalledSimpleAPILifecycleOverLAN
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testRunningHelloPythonDashboardOverCloud
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testSwitchingDevicesIsolatesControlsLogsAndWebTunnel
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testLiveCampaignDraftAndAskWendySettings
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testLiveDevicePagesAndLoadingTimes
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testLiveDeviceForegroundReconnect
           -skip-testing:WendyMobileUITests/DevicesLensUITests/testLiveGalleryPlaybackAndWarmReopen)
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
