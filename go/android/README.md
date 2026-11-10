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
- Data: every device's saved recordings, read without changing the connected
  device, with kind filters, per-device cards with trigger labels, an episode
  picker grouped by day, export and playback *emulator* (empty state only).
- Device page: Apps, Cameras, Data, Location and Settings segments; More
  device tools (appearance, logs, device info and update progress, app
  activity, Digital Twin, campaigns, device management); custom device
  appearances (symbol, emoji or photo on a color).
- App page: start and stop, open, logs, ports, version and restart; deploy
  instructions with the device selector.
- Device management: rename, remove from Cloud, reset local enrollment and OS
  update through the shared Go backend.
- Ask Wendy sheet: the connected device's chat, or a device chooser *emulator*.
- Robots lens: the iOS "under construction" page *emulator*.
- Apps, logs, cameras, settings, Wendy Lite boards, campaigns, recordings,
  chat and the model library, nearby setup over Bluetooth GATT, organization
  management and the mesh VPN with Android Always-on VPN carry over from the
  earlier parity work (PR #2801).

Device and Cloud operations call the shared Go backend. Successful
compilation does not establish that hardware-dependent workflows work.

## Parity still required

| iOS feature (PR #2715) | Android status |
| --- | --- |
| Activity lens: Alerts, Events and Archived, swipe to archive, read receipts | Not ported. Android shows the earlier connection and app-state Activity list. |
| Ask Wendy with Cloud models, remembered AI devices and model suggestions | Not ported. The sheet uses the connected device's chat; the bridge already exposes `StartHostedChat`. |
| Camera settings (exposure, focus and other controls) | Not ported. The bridge exposes `CameraControls`. |
| Sites lens: 3D site models, RealityKit view, coverage, site cameras, robot panel, calibration and reference capture | Not ported. The Sites lens shows the fleet by site. 3D viewing waits on the TinyUSDZ build and a RoomPlan sample. |
| Device recognition viewfinder | Not ported. Android offers Bluetooth setup and connect by address. |
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
