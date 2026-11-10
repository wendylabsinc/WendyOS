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

Screens follow the iOS `WendyMobile` app on `main`, drawn with this app's design
system. "Verified" below means compiled and unit tested; items marked
*emulator* were also run on an Android 16 emulator. Nothing here has been run on
a real phone against a real device yet.

- Devices: nearby (Android NSD) and Cloud lists, search, pull to refresh,
  connect by address, connected and connecting state, readable names.
- Device: Overview (first app's webpage, live camera previews, apps,
  shortcuts), Cameras and Apps, with Digital Twin, Campaigns and Chat in the
  overflow menu and Settings behind a gear, as on iOS. Pages use a back stack;
  covered pages keep their state and pause polling and streams. Connection
  failures stay on the device page and retryable ones reconnect on their own.
- Apps: device resources, temperature and power, hidden apps, app details with
  usage, GPU, ports, services, start, stop, logs and removal.
- Logs: app picker, warning filter, search, follow, copy and reconnection.
- Cameras: H.264, JPEG and thermal streams with play and pause, status, error
  details, and thermal minimum and maximum markers.
- Settings: enrollment (Cloud name rules, Wi-Fi check, no second credential),
  device info with agent updates, Wi-Fi (saved networks, security, hidden
  networks, traffic), Wendy Mesh (all seven settings, saved together, and the
  mesh graph), Bluetooth, audio defaults, campaign notification permissions,
  pairing (Wendy Lite, IP cameras, Unitree robots) and logs.
- Wendy Lite boards: cameras, SensorLink temperature, console logs, Wi-Fi and
  enrollment.
- Campaigns: list with clip counts, campaign pages with recordings and
  settings, the iOS editor with Hugging Face model and category pickers that
  pin the model revision, and the toolbar event log with unread markers.
- Recordings: episode and channel pickers, synced video, images, numeric
  charts, recorded audio, ROS point clouds (rotate, zoom), the iOS timeline
  (zoom, fit, drag to pan, pinch, seek) *emulator*, ZIP export, and manual
  recordings.
- Chat: per-device conversation, Markdown replies, device cards, approvals,
  connection settings, and the model library with capacity checks and
  automatic accelerator choice.
- Nearby setup over Bluetooth GATT, following the iOS flow, with a one-time
  invitation when a strong setup advertisement is nearby.
- Accounts and organization: saved accounts, organization picker, sign-in,
  teammates and roles, direct device access rules, invite links (copy or
  share) and invitation revocation.
- Sites: the organization's fleet by site, with counts, online-only filter and
  search *emulator*.
- Activity: connections and app state changes per device.
- One mesh VPN switch, matching iOS. All mesh devices join automatically; the
  shared controller switches to a mesh uplink when normal internet fails.

Device and Cloud operations call the shared Go backend. Successful
compilation does not establish that hardware-dependent workflows work.

## Parity still required

| iOS feature | Android status |
| --- | --- |
| Sites and digital twin: local 3D site documents, USDZ import and sharing, RoomPlan capture, photo reconstruction, recognition viewfinder, lens calibration, site reference capture | Not implemented. Needs a 3D stack decision (see the parity PR). The Digital Twin pane explains this. |
| Robot localization, navigation and motion in the twin | Not implemented. On iOS these live only in the twin and need the site's floor geometry. Robot pairing is available. |
| Remote campaign push notifications | Blocked. Cloud registers APNs tokens only (`wendycloud.v2.APNSService`). Android needs an FCM contract from Cloud. Campaign permissions can still be changed. |
| Always-on VPN | Not implemented. The service declares `SUPPORTS_ALWAYS_ON=false`; the VPN needs the app's account to start. |
| USB flashing and host simulator management | Desktop workflows, also absent from the iOS app. |

Physical-device checks are still required for BLE GATT/L2CAP, cloud sign-in,
camera decoding, VPN traffic, campaigns, model downloads and destructive device
operations. No deployment, app deletion, firmware update or robot movement is
part of the tests.

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
