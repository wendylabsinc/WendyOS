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

- Nearby discovery through Android NSD, Cloud discovery, search, manual addresses,
  authenticated connections, and explicit recovery when a pinned identity changes.
- Browser sign-in, multiple saved accounts, organization selection, fleet devices
  grouped by site, organization metadata, members, invitations and access grants.
- Device details and resource use; app start, stop, deletion, ports, live logs and
  embedded web apps over the shared authenticated loopback forwarder.
- Live H.264, JPEG and thermal cameras. H.264 uses Android MediaCodec; thermal
  frames use the shared Go measurement decoder.
- Campaign creation and settings, associated clips, manual recordings, recorded
  video and image playback, numeric plots, event details and verified ZIP export.
- Device-local model discovery, downloads and startup, streaming chat and explicit
  tool approvals. Android uses the in-process MCP tools, as iOS does.
- Wi-Fi, mesh settings, topology, remote Bluetooth peripherals, audio defaults,
  sensor/IP camera pairing, Unitree pairing, cloud enrollment and OS updates.
- Nearby setup through Android Bluetooth GATT. The native Bluetooth adapter also
  supplies scanning and L2CAP to the shared authenticated mesh implementation.
- An opt-in Android VPN service for selected Cloud devices. The service uses the
  shared split-tunnel engine and asks Android for VPN consent. Only selected
  routes and Wendy DNS enter the tunnel. The foreground notification stops it.
- Wendy's existing geometric app mark, with adaptive and monochrome launcher icons.

Device and Cloud operations are wired to real backend methods. Successful
compilation does not establish that every hardware-dependent workflow works.

## Parity still required

This is not full Mac/iOS feature parity. The following need further implementation:

| Existing feature | Android status |
| --- | --- |
| Local 3D site documents, USDZ imports, RoomPlan capture, photo reconstruction, recognition viewfinder and lens calibration | Not implemented. The Sites tab currently groups Cloud fleet devices by their assigned site. |
| Digital-twin robot localization, navigation and motion controls | Not implemented. Robot account pairing is available. |
| Remote campaign push notifications | Not implemented. The existing registration API targets APNs; Android needs an FCM backend contract and application configuration. |
| Recorded audio playback, ROS point-cloud rendering, timeline pinch/pan | Not implemented. These original files remain in verified recording exports. |
| Full organization editing, role management, access revocation and invite sharing UI | Partial. Metadata, member addition, invite creation and access grants are available. |
| Automatic internet gateway VPN and local mesh route picker | Not implemented. The current VPN picker selects Cloud device routes. |
| USB flashing and host simulator management | Desktop workflows, also absent from the iOS app. |

Physical-device checks are still required for BLE GATT/L2CAP, cloud sign-in,
camera decoding, VPN traffic and destructive device operations. No deployment,
app deletion, firmware update or robot movement is part of the build tests.

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
Unicode, errors, device parsing, and H.264 framing across transport boundaries.
Instrumentation tests exercise the actual JNI library, native NSD discovery,
stale session rejection and Activity recreation. They require a running emulator
or connected Android device. They do not sign in or modify discovered devices.
