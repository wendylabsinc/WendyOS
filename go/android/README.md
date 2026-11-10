# Wendy for Android

Native Kotlin and Jetpack Compose companion alongside `go/macos`. The APK embeds
the same Go backend used by the Mac and iOS apps. It does not need a desktop
computer or a separately running CLI. Android 10 or newer is required.

## Build and run

Open `go/android` in Android Studio. Install Android SDK 36, NDK
`29.0.14206865`, JDK 17 or 21, and the Go version declared in the repository's
root `go.mod`. Set `ANDROID_HOME` to your SDK and `ANDROID_NDK_HOME` to your NDK,
or install that NDK version under the SDK's `ndk` directory. On Apple Silicon,
the build script also recognizes Homebrew's `android-ndk` installation.

From this directory:

```sh
./gradlew :app:assembleDebug
./gradlew :app:installDebug
```

The APK is `app/build/outputs/apk/debug/app-debug.apk`. The default build includes
`arm64-v8a` and `x86_64`. To build only for an ARM phone or Apple Silicon emulator:

```sh
WENDY_ANDROID_ABIS=arm64-v8a ./gradlew :app:installDebug
```

The Gradle wrapper and dependency versions are pinned. The wrapper checks the
Gradle distribution's SHA-256. Native outputs, SDK paths, build caches and signing
keys stay out of Git. Release builds require your own signing configuration.
Windows hosts can build the Kotlin sources, but the native build script requires
macOS or Linux.

## Implemented workflows

Screens follow the lens-based iOS app from PR #2715 (`cn/mobile-app`), drawn
with Material 3. "Verified" below means compiled and unit tested; items marked
*emulator* were also opened on an Android 16 emulator. Nothing here has been
run on a real phone against a real WendyOS device yet.

- First launch: choose a primary lens (Devices, Robots, Sites) *emulator*.
- General Home: lens tiles, the organization button that opens Settings, the
  notifications bell, the floating Ask Wendy button, and the widget deck
  (favorite devices, camera feed, campaign recording, data gallery, app web
  portal, needs you, happening now, quick actions) with add, configure,
  resize, reorder and delete. Layouts are saved per account and organization
  *emulator* (quick actions only).
- Settings sheet: organization list, primary lens, organization management,
  connection settings, System/Light/Dark appearance, accounts and add account
  (Wendy Cloud or Development Cloud) *emulator*.
- Devices lens: Home (device count with presence bar, Local and Cloud
  inventories, setup, needs attention, recent data), Data and Activity tabs,
  with Ask Wendy above the tab bar *emulator*.
- Recordings catalog (iOS `RecordingCatalog`): one library per reachable
  device, nearby routes first, episode details (labels, kinds, campaigns,
  previews) loaded in batches of 64, and real thumbnails through
  `RecordingThumbnail`. Data, Activity and the Data gallery widget share it.
- Data: kind menu, per-device cards with trigger labels and coverage, an
  episode picker grouped by day with label filters, export (with a
  confirmation for several episodes) and playback that starts on open.
- Activity: Alerts (campaign recordings, agent updates, crash loops) with
  swipe to archive and read receipts, Events grouped by day, and Archived
  alerts kept on the phone for 30 days.
- Home widgets use the iOS four-column packing; the gallery shows thumbnails,
  camera widgets play inline or full screen, campaign widgets record a clip
  (`HomeCampaign`), and the editor has the iOS device and resource pickers.
- Device page: Apps, Cameras, Data, Location and Settings segments; More
  device tools (appearance, logs, device info and update progress, app
  activity, Digital Twin, campaigns, device management); custom device
  appearances (symbol, emoji or photo on a color).
- Cameras: live tiles with resolution, last-frame time and stale warning,
  full screen, and per-camera controls through `CameraControls`.
- App page: start and stop, open, logs, ports, version and restart; deploy
  instructions with the device selector. The dashboard browser follows
  `MobileAppBrowser`.
- Device management: rename, remove from Cloud, reset local enrollment and OS
  update through the shared Go backend.
- Ask Wendy: the device's model server, or Cloud inference with the person's
  own OpenAI or Anthropic key (Android Keystore, unlocked phone only, never
  sent to the device), remembered AI devices, suggestions and unread replies.
- Nearby setup in the four iOS steps: Device, Connection, Organization and
  Enrollment.
- Robots lens: the iOS four tabs, shown as under construction as on iOS.
- Device recognition (Devices, "+", Recognize a device): a CameraX viewfinder
  matches board appearances with OpenCV SIFT and a RANSAC homography (a port
  of `VisualRecognition.cpp`, with the same bundled Seeed XIAO photos) and
  reads printed device names with ML Kit text recognition. Tapping a match
  lets you choose the device and connect; "Remember a device's appearance" saves up to 12 references on the
  phone. OpenCV is pinned to 4.10: 4.11 and later bundle KleidiCV, whose SVE2
  code crashes the Android emulator on Apple silicon, which reports SVE2 it
  cannot run. Unlike iOS there is no ARKit, so outlines follow the 2D image.
- Sites lens: local sites with devices and robots, a setup checklist, 3D
  models (USDZ or `.wendysite` import, rename, default, save to and load from
  a device through `SiteModels`), live cameras, missions and activity.
  Models are read by a Kotlin USD reader (usda and usdc crate layers) and
  drawn with OpenGL ES. Fixed cameras are added by gateway identity, placed
  by tapping the model, previewed and aligned.
- Camera alignment (Sites and Digital Twin, "Align camera"): live video is
  matched to the scan's reference map and the lens and position are saved
  on the gateway (`SaveCameraAlignment`), as on iOS. `SiteLocalization.kt`
  is a Kotlin port of `SiteLocalization.cpp` on OpenCV's Java API, with the
  same thresholds, validation folds and two-frame check. The lens can be
  measured with this phone's screen or a saved checkerboard
  (`SaveCameraLens`), or the camera adjusted manually. Unplaced cameras are
  aligned in the background while their site is open, and every camera
  aligned on the gateway (by either phone) shows its view cone.
  Calibration frames come from H.264 keyframes decoded by a separate
  MediaCodec, or from JPEG frames.
- Live imagery: each aligned camera's latest picture is projected onto the
  model (iOS `SiteCameraColorRenderer`): a depth map rendered from the
  camera's pose decides what it can see, its lens distortion is applied, and
  the best-facing camera colours each surface. The wall a camera hangs on is
  left out of its depth view, as on iOS. The View menu turns camera coverage
  and live imagery on or off. This needs OpenGL ES 3; on ES 2 the model is
  drawn without imagery.
- Digital Twin (device tool): saved captures with import, share, remove and
  restore, and the gateway's cameras placed, previewed and aligned.
- Apps, logs, settings, Wendy Lite boards, campaigns, chat and the model
  library, organization management and the mesh VPN with Android Always-on
  VPN carry over from the earlier parity work (PR #2801).

Device and Cloud operations call the shared Go backend. Successful
compilation does not establish that hardware-dependent workflows work.

## Parity still required

| iOS feature (PR #2715) | Android status |
| --- | --- |
| Sites: room scanning and photo reconstruction | Not available: iOS uses RoomPlan and Object Capture on LiDAR devices. Android imports models made on iOS. |
| Sites: scan reference capture (the visual map) | Not available: iOS records it with ARKit LiDAR depth during the room scan, in the scan's coordinates. Android uses maps made on iOS, carried in `.wendysite` files. |
| Digital Twin robot map, localization, people tracking and Go2 controls | Not ported. These move a physical robot and need a robot to test against. |
| Remote campaign push notifications | Blocked. Cloud registers APNs tokens only (`wendycloud.v2.APNSService`). Android needs an FCM contract from Cloud. |
| USB flashing and host simulator management | Desktop workflows, also absent from the iOS app. |

Physical-device checks are still required for BLE GATT/L2CAP, cloud sign-in,
camera decoding, VPN traffic, campaigns, recordings, model downloads and
destructive device operations. No deployment, app deletion, firmware update or
robot movement is part of the tests.

## Architecture and lifecycle

The Compose interface uses a shared light/dark theme in `DesignSystem.kt`, Wendy's
geometric mark, and a native vector device illustration. The device overview
shows live resource measurements, with unavailable values left explicit and full
metadata in an expandable section. Shared cards, navigation, account sheets and
settings rows keep the companion screens consistent.
Network, model, setup, recording and organization screens show selected summaries;
raw metadata is only rendered inside explicit diagnostic expanders. Organization
forms resolve named people, devices and roles to their underlying IDs.

`mobile/` owns an explicit method allowlist and a bounded JSON request format.
`bridge/` is the JNI adapter. UTF-8 byte arrays cross JNI, avoiding modified UTF-8
corruption of Unicode names and chat text. There is no bridge HTTP server and no
JavaScript interface exposing device operations.

`PlatformServices.kt` implements Android NSD and Bluetooth. The Android-specific
Go adapters use these APIs instead of Avahi, BlueZ, libusb or helper processes.
Discovery advertisements remain hints. The shared backend verifies device identity
and authorization before connecting. L2CAP carries the same authenticated protocol
as the Apple client; it does not rely on Bluetooth bonding as device identity.

Android owns a private `noBackupFilesDir/wendy` configuration directory. Device
pins, OAuth tokens and certificates are not copied from the host CLI and are
excluded from Android backup and device transfer. Device operations run off the
main thread. Backgrounding invalidates device sessions and stops their streams.
Returning reopens the selected device. An explicitly started VPN retains the
embedded broker while its foreground service is running. Account changes require
stopping that VPN first.

Browser sign-in runs with a temporary foreground service so Android permits the
loopback callback, OpenID discovery and token exchange while the browser is open.
Auth events continue to be consumed in the background. The service stops on
completion, failure, cancellation or after five minutes. Its notification offers
a cancel action; ordinary device discovery and streams still stop in the background.

The native build uses `-checklinkname=0` because the existing Pion dependency's
`github.com/wlynxg/anet` package requires it on Android with Go 1.23 and newer.
See [anet's build instructions](https://github.com/wlynxg/anet#how-to-build-with-go-1230-or-later).
This flag is confined to the Android library build. The build requests 16 KB ELF
segment alignment explicitly; Gradle packages the libraries without legacy extraction.

## Validation

```sh
# From go/android
./gradlew :app:testDebugUnitTest :app:lintDebug
./gradlew :app:connectedDebugAndroidTest

# From the repository root
go test ./go/android/mobile ./go/macos/backend
```

Unit tests cover bridge rejection of malformed calls, stale initialization,
Unicode, errors, device parsing, automatic VPN route updates and cancellation, private/public DNS isolation,
H.264 framing across transport boundaries, and the iOS rules the screens port:
Wi-Fi and mesh parsing, the mesh graph, enrollment names and retry safety,
redacted errors, logs, resources, campaigns, chat Markdown and model fit,
nearby setup observation, organization snapshots and the recording timeline.
Instrumentation tests exercise the actual JNI library, native NSD discovery,
stale session rejection, Activity recreation, native private/public DNS, and idle
VPN packet-pump cancellation. The VPN lifecycle test also connects, disconnects
and reconnects a system tunnel when VPN consent has already been granted; otherwise
it skips. These tests require a running emulator or connected Android device.
`RecordingTimelineTest` drives the timeline and point-cloud gestures. They do not
sign in or modify discovered devices.
