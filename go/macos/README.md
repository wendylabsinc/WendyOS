# Wendy for macOS

A SwiftUI app linked to this repository's Go CLI packages through a C archive. It lists nearby LAN and authenticated mesh devices, connects to Wendy Cloud, displays running apps and service states, streams logs, and displays a camera wall of simultaneous H.264 feeds using each camera’s native resolution and frame rate.

Connected devices show their Wendy Cloud enrollment status in device Settings. An unenrolled WendyOS device offers **Enroll device**, where you explicitly choose a saved organization account and device name, or sign in to another organization. Enrollment uses that account without changing the global account selection and checks the resulting device organization before reporting success. Failed status reads remain unknown. Wendy Lite offers Bluetooth enrollment from its Settings tab.

When Cloud explicitly rejects a duplicate or invalid name before issuing credentials, you can edit the name and press **Enroll** again. Editing the name or organization clears that rejected attempt's error. Timeouts, device-side failures, and other uncertain outcomes still require checking enrollment status before another attempt.

On macOS, choose **+ → Set up nearby device** in the toolbar. On iOS, choose **Nearby → Set up nearby device** to discover unregistered devices advertising the setup service. Select a device, choose a saved organization account, and enter a device name. Wi-Fi configuration is optional and collapsed by default. Setup uses encrypted BLE L2CAP on PSM 130, separate from management and mesh connections. It supports WendyOS ACME enrollment and Wendy Lite enrollment with provisioned trust roots and device-bound signed time.

The setup sheet remains open when advertisements disappear. **Retry connection** opens a fresh encrypted session. Once a Cloud mint may have happened, the app retains that attempt for the same Bluetooth address until the app exits and prevents another mint. **Check enrollment status** verifies the expected device UUID and organization using fresh Cloud presence; a saved Lite configuration alone is not success. A device may start enrollment without Wi-Fi, but it needs a supported network path to complete issuance and appear online. If the app quits during an uncertain attempt, inspect the organization's Cloud device list before starting again.

The macOS and iOS apps embed packet-tunnel Network Extensions. Select mesh or cloud devices in the VPN controls to resolve their Wendy DNS names and route TCP connections from other apps. To use a device's shared internet connection, choose it under **Internet via mesh**. The donor must advertise an enabled uplink and run an agent with operator lease protocol `wendy-mesh-internet/2`. Device-only routing remains the default.

## Build and open

Requirements:

- macOS 26 or later and Xcode with the macOS SDK.
- The Go version required by the repository's `go.mod`.
- XcodeGen, pkg-config, and libusb. On Homebrew, `brew install xcodegen pkg-config libusb`.

From the repository root:

```sh
go/macos/Scripts/build.sh
open go/macos/Build/DerivedData/Build/Products/Debug/WendyDesktop.app
```

The default build is unsigned. Device discovery, cloud access, cameras, and logs work without activating the VPN. macOS will only activate the Network Extension from a properly signed and provisioned app.

The build produces native binaries for the build Mac. It links `libwendydesktop.a` into the app and `libwendyvpn.a` into the extension. It also bundles the repository's `wendy` executable for the CLI's detached mesh worker and includes libusb, with no Homebrew runtime dependency. Generated binaries and DerivedData stay in the ignored `Build/` directory.

`project.yml` is the source for the checked-in Xcode project. Run `xcodegen generate` from this directory after changing it. The Xcode build phases build Go automatically and serialize concurrent archive builds. The initial build can take several minutes.

## Site models on macOS and iOS

### Automatic camera alignment

**Auto-align cameras** is enabled by default in Digital Twin. While the view is
open and the app is active, it locates saved cameras using fresh video and the
selected room's visual reference map. It checks one camera at a time, rechecks
successful automatic positions after a minute, and backs off failed attempts
from 30 seconds to five minutes. Each camera shows its alignment status.
The toggle pauses the work; closing Digital Twin, changing gateways or models,
opening manual calibration, and putting the app in the background cancel it.
Manual placements and manually paused camera feeds are preserved.

Automatic and manual alignment share the same two-frame stability, image
coverage, held-out reprojection, and metadata revision checks. An approximate
position using an assumed lens needs at least eight independent matches.
A measured-lens pose still needs twelve, and joint lens calibration needs sixteen.
Failed checks keep the existing metadata; automatic retries cannot guarantee a
position from an unrecognizable view. A missing visual reference map still needs
an iPhone room scan. This runs in the app, not as an unattended gateway service.

Room capture exports RoomPlan's detected geometry without extending furniture
bounds. Glass can be missing from that capture. Automatic camera alignment does
not reconstruct missing cabinet surfaces or infer obstacle-free space there.

### Camera color

Enable **Camera color** in the twin settings menu or Cameras sheet on iPhone,
or beside **Fog of war** on macOS. Up to eight aligned visual cameras can color
the model together, independently of the selected preview. Per-camera status
explains missing calibration, stale frames, resolution changes, and unavailable
feeds. Turning the switch off stops subscriptions owned by the color overlay.

This remains an experimental view-only projection. It uses the saved geometry,
handles RoomPlan meshes without texture coordinates and nearby mounting-wall
overlap, and preserves fog. Missing geometry and moving objects can still cause
projection artifacts. Source models and site exports are unchanged.

### Fog of war exploration

Open an existing **Site Model** and enable **Fog of war** below the site title.
The imported house starts dark. Once Go2 has a confirmed, live position in that
model, nearby areas become bright. Areas it leaves remain dim, retaining the
exploration history. The counter shows approximate explored map area in square
metres. Select Go2 and use the existing floor destination preview and movement
controls as before. Fog does not start a search, issue orders, or change any
navigation checks. Rotation search still requires accepting its popup;
enabling fog does not invoke it.

**Reset** clears exploration and immediately reveals the current valid position.
Turning fog off restores the original model materials and pauses recording;
turning it back on retains history in the same view. History is local to the
current view and resets on a different site, model revision, scale, gateway,
robot connection, or alignment. It is not saved in `.wendysite` exports.
Unconfirmed suggestions and manual placement previews never reveal the map.
Missing or stale telemetry removes live visibility while retaining history;
loss of alignment clears history because its coordinates are no longer trusted.

Visibility is a demo approximation: a 2.5 m horizontal circle, with line-of-sight
tests against the imported mesh 35 cm above the robot's reported floor. A grid
uses 25 cm cells, growing the cells for models larger than 32 m to stay within
128 by 128 cells. The model uses the same world-space mask at every height, so
this is intended for single-level captures. The area counter counts revealed
grid cells within the model bounds, not verified walkable floor or a percentage
of the house. Saved geometry can be incomplete or out of date, and this display
does not measure actual camera or LiDAR coverage. Robot markers, cameras, routes,
and localization evidence stay visible. This version follows the single active
Go2 supported by the existing map; it does not autonomously explore or combine
multiple robots.

`Tests/SiteFogTests.swift` covers history, bounds, rejected poses, stale telemetry,
placement previews, scaled wall occlusion, rendered pixel brightness, reset,
context changes, and material restoration. Run the macOS test scheme with:

```sh
cd go/macos
xcodegen generate
xcodebuild -project WendyDesktop.xcodeproj -scheme WendyDesktop \
  -configuration Debug -derivedDataPath Build/DerivedData \
  CODE_SIGNING_ALLOWED=NO -destination 'platform=macOS' test
```

The renderer tests use a synthetic room and issue no robot commands. They write
`wendy-fog-revealed.png`, `wendy-fog-remembered.png`, and
`wendy-fog-restored.png` to the test process's temporary directory.

### Importing and viewing sites

Connect to a gateway, then open **Site Model** in its device tabs. **Pair Cameras**
opens the existing SensorLink and IP camera setup. **Refresh** reloads the gateway's
camera inventory. Cameras with stable identities can be previewed before a model
is available.

Import a textured USDZ scan, or choose **Sites > Reconstruct from Photos** on
supported hardware. iPhones and iPads with RoomPlan support also offer **Scan
Room**, which produces room geometry without photographic textures. **Add Camera**
previews the feed, then uses an existing valid pose or starts automatic alignment
against the room's visual references. It saves no placeholder location. If references
are missing, capture them on iPhone; if matching fails, the camera stays unpositioned.
**Place Manually** remains available in the camera menu. Selecting a positioned
camera opens its live feed through the app's authenticated Go connection.

**Sites > Share Digital Twin… > AirDrop** sends the model, saved camera placements,
and visual references as a `.wendysite` file through the system share sheet.
On the receiving device, use **Sites > Import Model or Site** to open it.
**Sites > Export Site** saves the same archive to a folder.
Import that file on the other platform while connected to the same Cloud gateway.
The file contains no credentials, and importing it creates a separate local copy.
There is no automatic cross-device site sync. Sites live in Application Support's
`WendySites` directory, inside the application sandbox on iOS.

On iPhone, the twin fills the workspace. The Cameras button opens a sheet for
previews, alignment status and camera management. The settings menu holds
automatic alignment, pairing, refresh and fog controls. Robot controls open
separately, and Stop remains available on the map when a robot is paired.

Camera identities include the gateway's tenant and asset for Cloud connections.
LAN connections use the gateway's hardware serial and enrollment organization;
these placements are scoped separately from Cloud placements. Numeric camera
IDs and network addresses are never persisted as camera identities. Feeds without
a stable identity cannot be attached to the model.

Camera menus offer **Calibrate and Align**. For a geometry-only capture, this
starts a fresh iPhone LiDAR scan. Walk through the area visible to the fixed camera,
including textured surfaces at different depths. The scan stores sparse image
feature descriptors and metric scene points with the model, not full photographs.
The map is bound to the exact USDZ bytes and model revision. Exporting the site
includes these references so alignment also works on macOS.

Alignment accepts live JPEG and H.264 feeds. H.264 alignment decodes independent
keyframes on demand and rejects stale frames or frames from a previous stream. It matches the camera image to
scan features, estimates the optical pose and, when necessary, square-pixel lens
intrinsics with radial distortion. Fits must pass image coverage, nonplanarity,
held-out reprojection error, and a second live-frame pose check before saving.
The minimum consensus is 16 points for joint lens/pose calibration, 12 for
measured-lens pose fitting, or 8 for an approximate pose with an assumed lens. If measured alignment cannot be solved, automatic alignment
tries an estimated pinhole lens with a centred principal point and zero distortion.
The estimate must pass separate-point and second-frame checks, with looser limits
of 5 px RMS, 30 cm position variation and 10 degrees rotation variation. Its FOV
and pose are approximate. The app labels estimates and draws orange cones, while
measured lenses use green cones. An estimate cannot overwrite a measured lens.
A measured lens allows pose alignment against a flat surface.

Pixel-error limits refer to a 640-pixel long image edge and scale with capture
resolution. For example, 5 px at 640×480 is 12.5 px at 1600×1200. Saved RMS
values remain in the original capture's pixels; metric position and angular
stability limits do not change with resolution.

Estimated alignment validates each focal-length candidate before choosing one.
When the references have enough depth variation, it also refines focal length
and pose together, keeping the principal point centred and distortion zero.
Comparable fits that differ by more than 30 cm or 10 degrees are rejected as
distance-ambiguous. Later attempts re-estimate an assumed lens rather than
locking retries to an earlier guess. A measured lens remains fixed.

Camera markers and viewing cones that intersect a nearby RoomPlan wall are drawn
on its room-facing surface, with clearance for the marker. This display adjustment
preserves height and viewing angle, checks the actual mesh so openings stay clear,
and only applies within 50 cm of a wall. The saved calibration remains unchanged.

On iPhone, **Phone Screen Calibration** shows a 7×10 checkerboard to the fixed
camera. Move the phone around the camera image and tilt it in both directions.
The app captures distinct views automatically, uses haptics for feedback, and
requires at least 12 accepted views. It checks image coverage, tilt diversity,
parameter uncertainty and four splits of held-out views before saving the lens.
The room does not need to change and no printed target is required. New lens
measurements invalidate old poses derived from a different lens, so align again
once the lens has been saved. This screen workflow has synthetic coverage and
still needs validation with the physical phone and camera.

For measured alignment, sets below 60 points undergo four validation splits,
with limits on pose, intrinsics, and edge-ray variation between fits. Possible
image matches are reported separately from points that agree on the geometry.
Flat walls, repeated textures, large viewpoint changes, and poor LiDAR depth can
prevent a solution. The workflow and fit limits still need physical validation
against a fresh scan; synthetic solver tests do not establish real-world accuracy.
Calibration writes use the gateway metadata API with revision checking. Site poses
expire after seven days and must be refreshed if a camera moves. The display does
not detect physical movement automatically.

The model remains static scan geometry; this does not perform continuous 3D
reconstruction. The rendered cone shows the calibrated view direction and angular
field of view, not measured range or occlusion-aware coverage.
The existing thermal player is available for thermal camera feeds; spatial gas
and audio overlays are not implemented. Room scanning needs a physical supported
iPhone or iPad and cannot be verified in the simulator.

## Concurrency

Both apps compile in Swift 6 mode. App and test code must not construct
unstructured Swift tasks. Every Xcode build runs `Scripts/check-concurrency.py`
to enforce this rule.

SwiftUI `.task` owns each view's asynchronous work. Synchronous button handlers
submit actions to `AsyncActions`; its task group cancels and joins those actions
when the view disappears. State-driven work uses `.task(id:)`. App-level cleanup
and event polling belong to the persistent menu bar label on macOS and the root
scene on iOS. Shutdown drains camera cleanup before releasing the Go backend.

Blocking C calls run through `BlockingWork` on dedicated dispatch queues, outside
Swift's cooperative executor. Cancellation skips queued calls and suppresses
stale results. A call already executing in C is awaited until it returns, so its
resources cannot be freed while it still uses them. Explicit stop and close
operations run even after cancellation. This does not make a blocking Go call
interruptible unless its Go API provides cancellation.

Delegate and KVO callbacks feed async streams or `AsyncCallback`. Media seeking,
chat generation, model downloads, and push-registration timeouts run beneath
structured owners. The concurrency tests cover cancellation, late callbacks,
action-scope cleanup, and restarting an action scope.

## Device installation

Choose **Add > Install Wendy on a device**, or use the install button in the empty device view.
The guide distinguishes SD readers, USB NVMe enclosures, and USB recovery with storage
left in the board. It includes the docs' bundled videos, an Orin Nano J14 pin diagram,
and board-specific Dragonwing switch instructions for both entering EDL and returning to normal boot.
Offline 3D guides reuse the existing Orin Nano, Raspberry Pi 5, and IQ-9075 assets with
camera presets, connector highlights, a J14 jumper overlay, and the modeled SW2-3 actuator.
IQ-8275 retains its own S2-8 diagram; the IQ-9075 model is not presented as an IQ-8275.
The pin diagram follows NVIDIA's Orin Nano carrier specification SP-11324-001 v1.3,
figure 1-4 and table 3-4. Custom carriers need their vendor's instructions.

Flashing runs inside the app in a SwiftTerm terminal using the bundled CLI. Its release
and drive pickers, networking setup, elevation, erase confirmations, and live progress
remain interactive. The app passes explicit storage/rootfs choices and never supplies
`--force` or chooses a drive automatically. Other devices, Wendy Lite, and existing-host
agent installs remain available through the full installer picker.

A running installer stays alive if its view is hidden. Closing its sheet or quitting
Wendy is blocked until it exits; **Stop** sends an interrupt after confirmation.
A failed or interrupted write never advances automatically to the boot step. After the
installer exits successfully, follow the physical boot steps and verify an explicit
address, board and installed version. Cloud enrollment can also be required. A successful
write alone does not mark first boot or an application as ready.

## iPhone and iPad

The `WendyMobile` target is the iOS version of this Go-backed app. It does not use
or depend on the old Companion app. It requires iOS 26 and an Apple Silicon Mac
for simulator builds. Xcode builds the same Go backend as an iOS C archive and
links it directly into the SwiftUI app; no CLI executable or subprocess is needed.

Open `WendyDesktop.xcodeproj`, select **WendyMobile**, and choose an iPhone or iPad
simulator. To build from the repository root:

```sh
xcodebuild -project go/macos/WendyDesktop.xcodeproj \
  -scheme WendyMobile -destination 'generic/platform=iOS Simulator' \
  -derivedDataPath go/macos/Build/iOSDerivedData \
  CODE_SIGN_IDENTITY=- CODE_SIGNING_ALLOWED=YES build
```

For a physical device, select your Apple development team for `WendyMobile` in
Xcode and use an iPhone or iPad run destination. The bundle ID is `dev.wendy.mobile`.
The device build can also be compile-checked with `-destination 'generic/platform=iOS'
`CODE_SIGNING_ALLOWED=NO`; that unsigned output cannot be installed on a phone.

The mobile interface includes nearby/Cloud discovery, manual connections, accounts,
Sites, organization management, apps, activity, logs, live cameras, pairing,
campaigns and clips, device-local chat, and recorded data. Compact layouts use
stacked panes and touch controls. Recorded video, images, events and plots share
the episode cursor; the timeline supports playback, seeking, ruler panning and
pinch zoom, with a capped scrollable channel list.

Accounts and device identity pins live in the app's Application Support directory,
separate from the Mac CLI. Browser sign-in stays inside an in-app Safari sheet.
Chat runs the Wendy MCP server in-process, with bundled skills, tool approvals,
and context-scoped MCP request attribution. Models still run on the selected edge
device, not on the phone. The iOS app does not expose host shell execution.

LAN, authenticated Cloud, and mesh transports are supported. On iOS the mesh
broker runs in-process. App-to-extension broker IPC uses mutually authenticated
loopback TLS, with an ephemeral identity in the private App Group container;
this avoids iOS Unix socket path limits. When VPN starts, the app drains its broker and the packet
tunnel extension owns the shared profile and transport until VPN stops. The
extension can continue while the app is suspended. Both targets require the
`group.dev.wendy.mobile` App Group and `packet-tunnel-provider` entitlement in
their provisioning profiles. Existing local configuration is copied into the
shared container on first launch with these entitlements. USB flashing and host
simulator management remain desktop-only. Keep
the app in the foreground for device operations; camera subscriptions stop when
it enters the background and resume when the camera screen becomes active.

Run the mobile runtime tests using an installed simulator destination:

```sh
xcodebuild -project go/macos/WendyDesktop.xcodeproj \
  -scheme WendyMobile -destination 'platform=iOS Simulator,name=iPhone 17 Pro,OS=26.4' \
  -derivedDataPath go/macos/Build/iOSDerivedData \
  CODE_SIGN_IDENTITY=- CODE_SIGNING_ALLOWED=YES test
```

The tests initialize and call the embedded Go bridge in the iOS sandbox, exercise
the H.264 parser, and check recorded-frame timestamp lookup. Network operations
still require a reachable device or a Cloud account.

## Fleet, pairing, campaigns, chat, and recorded data

- **Sites** is a fleet-wide sidebar destination. Select a saved account and organization to see all enrolled devices grouped by their Cloud site, including offline devices. Online state comes from broker presence; failed presence reads remain unknown. Filter by device, site, or hardware, and connect from a tile. Refresh uses the window toolbar.
- **Pairing** on a connected device discovers Wendy Lite/SensorLink sources and ONVIF IP cameras on that device's network. Lite pairing reuses CLI tenant checks and stable device identities. Unenrolled sources are shown but cannot be paired. Older Agents may fall back to discovery on this Mac, with an explicit notice. IP camera credentials go to the selected Agent, and the password field clears after submission. Forgetting a pairing requires confirmation.
- **Campaigns** provides a visual When / On / Then editor. Choose object detection, image classification, or an existing application event, any camera or one actual source, and notification plus recording or recording alone. Review the capture window, confidence, cooldown, and upload policy before deploying. Deployment immediately arms a new campaign and refuses to overwrite an existing name. Clips stay local by default. The default detector is DETR ResNet 50 with the person label; model revisions are pinned and the device downloads the runtime/model on first use. Wildcard cameras apply to the selected device, including cameras added later, not the entire fleet. Cloud notification grants still need to be enabled for the campaign by an owner/admin in Cloud Apps. Deployment does not prove notification delivery or successful capture.
- **Chat** uses Wendy Chat's model discovery and streaming provider with a transport fixed to the selected device. The picker discovers downloaded models across running managed Ollama runtimes and retains the selected model’s port. When Cloud does not yet offer the model TCP port, Chat streams HTTP through non-TTY exec in the selected device’s running managed Ollama container. This fallback uses the authenticated Agent connection, preserves request cancellation, and does not retry access denials. Custom servers still require Cloud port forwarding. If both routes fail, downloaded models remain visible with a warning beside the message box, a Retry connection button, and expandable technical details; chat stays disabled until an endpoint is reachable. Custom OpenAI-compatible server ports remain available in connection settings. Downloads show a progress bar with bytes and percentage for the current file, and an indeterminate bar during preparation and inference checks. Mesh/Cloud uses the authenticated tunnel; LAN uses the selected device's address and requires a reachable server port. Common ports are Ollama 11434, llama.cpp 8080, and LM Studio 1234. Environment API keys and hosted-provider settings are never inherited. Stop cancels the generation; changing devices cancels the request. Assistant replies render Markdown and hide model thinking blocks, including Qwen GGUF templates that omit the opening tag. Conversations are held in memory. Each device session starts the bundled Wendy MCP with all tool groups and bundled skills. Read-only tools run directly; changes require approval of the displayed function and arguments. Ollama uses a per-request 32,768-token context for the tool catalog.
- **Data** opens recorded Wendy Data episodes with a shared cursor, a custom pannable and zoomable timestamp ruler, channel timeline, numerical plot, events, and sample inspector. Camera playback is available for checksummed sealed MP4 clips whose frame schedule matches the capture index; JPEG/PNG application frames also follow the cursor. Samples align on the episode's CLOCK_BOOTTIME timeline, including negative pre-roll. Unknown clocks, other boot IDs, unsupported payloads, and download limits are reported rather than assigned invented timestamps. The **Record** dialog selects actual camera, microphone, ROS 2 topic/domain, and telemetry sources for an immediate device-local capture; it also stops and seals the active recording. Campaigns offer these additional sources alongside camera selection. Audio and ROS 2 start at the trigger, without pre-roll.

  Recorded PCM WAV segments replay at 0.5–2× on the episode cursor with a peak-amplitude plot. ROS 2 SQLite and MCAP bags use their recorded host clock samples to align bag receipt times; missing mappings, clock jumps, and out-of-range timestamps are reported. LaserScan and PointCloud2 have an orbitable 3D preview in sensor coordinates, without TF transforms. IMU, GPS, vectors, twists, and basic numeric messages expose measurement channels; unsupported messages retain metadata and original export payloads. LiDAR previews retain at most 4,096 points per frame and 1,000,000 points per episode. Sensor downloads are capped at 512 MiB per file and 1 GiB total, with a shared 20,000-sample viewer limit.

  **Export > Full episode** writes a ZIP of every original manifest file, checks each SHA-256 digest, and retains ROS bag directories/metadata, WAV segments, MP4 clips, indexes, and clock evidence. Extract the archive to open its files in other tools or replay a ROS bag. Export does not apply preview limits. CSV exports loaded measurements with integer episode nanoseconds; PLY exports the current LiDAR preview in meters. Both preview exports identify their limited scope. These recording and sensor viewer controls are currently in the macOS Data pane.

All of these operations use concrete C structs/enums and session-bound Go functions. JSON/YAML wire details stay in Go. No CLI subprocess is used for these UI operations.

Each campaign shows a count of its recordings currently stored on the device. Click the count to list clips by date, duration, size, and state, then open a clip in the Data viewer. Association comes from the recording's saved campaign trigger, not its display name. A recording with multiple camera tracks counts as one clip. Counts include recordings in progress, exclude removed recordings, and show a `+` if some manifests could not be read.

## Sign and enable the VPN

Configure your Apple development team for both targets. Provision `dev.wendy.desktop` with System Extension and Network Extension capabilities, and `dev.wendy.desktop.network` with the packet-tunnel system-extension entitlement. The exact entitlements are in `Resources/`.

```sh
WENDY_DEVELOPMENT_TEAM=YOUR_TEAM_ID go/macos/Scripts/build.sh
```

You can also select the team and provisioning profiles in Xcode. Developer ID distribution requires the corresponding profiles and notarization.

Place the signed app in `/Applications`, open it, sign in if needed, and select devices in the VPN popover. Turn on the **VPN** toggle, then approve the system's extension and VPN prompts. The app reports when macOS is awaiting approval or requires a restart. Disable the VPN before replacing the installed build.

## Authentication and device selection

The Go adapter uses the same `~/.wendy` configuration, selected organization, token refresh, device identity verification, and pin checks as the CLI. The account menu starts the existing OIDC/PKCE browser sign-in flow. Production and development sign-in are available. Settings (⌘,) lists saved accounts and their emails. Organization management has its own account and organization selectors. Device discovery uses the CLI's active context; change that context with the CLI and refresh the device lists.

Nearby discovery includes the CLI's authenticated mesh worker when the selected organization has an operator certificate. LAN discovery also works without cloud sign-in. Mesh participation inherits the CLI's certificate expiry and selected-organization checks. A device can appear as LAN before a mesh route becomes available; refresh to see newly established mesh routes.

Cloud selections include the endpoint, organization, and device ID. Nearby connections preserve the discovered hostname so the CLI can try available addresses. A failed connection keeps the original diagnostic behind **Technical details**; a pin mismatch offers **Forget saved identity and retry**. This explicitly runs the CLI’s unpin operation for the failed device, then reconnects with normal certificate validation. Reachability failures never offer a trust reset.

## Organization management

Choose **Organization** in the sidebar. Account, organization, teammate count and actions share one row. Refresh is in the window toolbar and supports ⌘R. The native pane uses typed C functions linked to Go and the same authenticated Cloud gRPC services as the CLI.

- The Team pane lists names, emails and assigned roles, including custom roles. Select a person to inspect direct device rules and assigned role permissions. Add existing accounts by email, change roles, or remove members.
- **Add teammate → Manage invitations** lists existing invitations. **Create invite link** creates shareable links with a role and expiry, lists existing invitations, and revokes active links. Copy a new link before closing the sheet; Cloud returns its token only once. The app does not send invitation emails.
- The teammate inspector lists direct device access rules. Add an allow or deny on a device or all devices, or remove an existing rule. Deny wins. Direct rules are not a computed effective-access matrix including group membership.

Cloud permissions determine which controls are available. The server independently authorizes every action. Membership, role, invite creation and access-rule mutations use operator-signed requests bound to the selected tenant. Selecting another organization cannot silently switch accounts; choose its saved session or sign in first. Read failures remain visible rather than appearing as empty results. Removing teammates, revoking invitations and deleting rules require confirmation in the app.

## Device interface

The sidebar has Nearby and Cloud navigator tabs, a local search field and an email account footer. Selecting a device immediately switches its detail panel to a connecting state, with progress on the selected row. Failed connections remain on that device with retry controls; late connection results cannot replace a newer selection. Activity defaults to name order; click its table headers to sort by name, CPU or memory. Listening ports appear beside the table. Apps and Activity offer Start, Stop, and a destructive Delete App action in their context menus. Deletion confirms the app and device, retains images and persistent volumes, and refuses a stale device session. Open appears only for apps with an HTTP entitlement. Camera tiles expose circular playback and full-screen controls on hover.

## Network behavior

- The extension installs a `/32` route for each discovered device address in `10.99.0.0/16`, plus the DNS resolver `100.64.0.53`.
- Split DNS applies only to `mesh.wendy.dev` and `cloud.wendy.dev`. Names are `device-<device-id>.mesh.wendy.dev` or `device-<device-id>.cloud.wendy.dev`. The VPN popover lists mesh devices without selection controls.
- The address is a deterministic hash of the full selector. Unlike the older companion's numeric mapping, this supports UUID devices and cloud endpoint/organization identity. Hash collisions are allocated in selector order so all discovered devices can be included. Conflicting DNS names are rejected.
- DNS supports UDP and TCP. A queries for available devices receive their address, AAAA queries receive no data, and unknown device names receive NXDOMAIN. Public names in the matched zones, including the cloud dashboard apex, are refused by this resolver. macOS 26 DNS failover lets them use the system resolver. Other zones are not selected by the split-DNS settings.
- TCP flows terminate in gVisor, then pass through an authenticated loopback relay to the existing mesh or cloud port dialer. Automatic device routes use the authenticated mesh directory. The internal relay also supports explicit cloud routes for existing callers.
- Cloud app ports require Cloud's signed `tcp-<port>` tunnel services. Older Cloud servers return an unsupported-service error, which appears in the VPN popover. Rebuilding the desktop cannot add a missing Cloud service, and authorization failures never trigger a fallback.
- IPv4 ICMP echo requests to selected device addresses trigger an authenticated agent version RPC over the pinned mesh/cloud connection, as in `wendy cloud ping`. An echo reply is emitted only after that device responds. The VPN reuses one probe connection per selected device, but every echo requires a new RPC response. The first echo includes connection establishment; later echoes measure the RPC over that connection. A failed probe discards the connection so the next echo reconnects. Stopping the VPN, updating routes, or quitting the app closes these connections. Identifier, sequence and payload are preserved. This checks agent reachability, not an application port or raw ICMP forwarding on the remote OS. Probes time out after five seconds, with eight workers and a bounded queue. Invalid checksums, unselected destinations, fragmented echoes and packets larger than the 1400-byte tunnel MTU are dropped.
- On macOS, a fresh random relay credential travels in the ephemeral VPN start options and is not saved in the VPN profile. The macOS extension never receives the cloud access token or operator private key. On iOS, the extension reads the app profile from the provisioned App Group container and owns its broker and relay.
- The relay accepts only selected addresses, valid ports, and its current credential. It limits concurrent flows. Stopping the VPN closes active relay connections. A heartbeat removes the tunnel if the app exits or crashes.
- For selected device addresses, UDP application traffic, ICMP types other than echo requests, IPv6 application routing, and automatic mesh/cloud failover are not implemented. TCP half-close is retained when the underlying CLI transport supports it; the existing cloud byte-pipe transport closes the connection when half-closed.
- The VPN includes all devices in the authenticated mesh directory, without device or gateway selection. New devices are picked up automatically. Keep the macOS app open while the VPN runs. The iOS extension owns its transport independently of the app.
- USB gadget links join automatically through the device's scoped IPv6 endpoint, `[fe80::5741:1%interface]:43024`, using the same authenticated mesh protocol. This requires an agent with USB mesh support, Ethernet mesh participation enabled on the device, and the mesh kernel prerequisites. The USB donor must have an independent internet connection and internet sharing enabled. Device-side USB listeners bind only the well-known gadget address on `usbN`; they do not depend on DHCP or mDNS.

- Normal internet stays on the normal connection. Every five seconds the extension checks authenticated TLS reachability to independent Cloudflare and Google endpoints over the provider's underlying network, using literal IPv4 and IPv6 addresses to avoid the VPN DNS resolver. Two consecutive failures enable mesh internet backup; two consecutive successes restore normal internet. One blocked probe endpoint does not trigger failover if another works.
- Backup chooses the lowest mesh route metric among devices advertising a live signed internet-sharing offer and authorized route. It retains a working gateway until recovery or failure, tries another donor on failure, and backs off failed donors for 30 seconds. Metric is a mesh path cost, not a measured internet speed. If no donor works, device routing stays active and discovery keeps retrying.
- During backup the extension installs default IPv4/IPv6 routes using the donor's leased addresses and forwards complete IP packets, including TCP, UDP and ICMP, inside the mesh IPv6 tunnel. A donor without IPv6 captures and drops IPv6 internet packets. The inner MTU is 1280 bytes and the outer mesh MTU is 1320.
- Backup sends public DNS through the donor's mesh DNS proxy while device names retain local answers. System DNS failover is disabled during backup and restored with the split routes when normal internet recovers. The donor's NAT and destination restrictions apply. The authenticated v2 lease binds an operator installation UUID to its mesh source address.
- Switching internet paths changes the source address, so existing internet connections may need to reconnect. This is automatic backup, not a kill switch. Device directory changes rebuild the data relay and may also reconnect active device flows.

The two companion repositories informed the split-DNS and system-extension design. `H264VideoDecoder.swift` and `CameraDisplayView.swift` were adapted from `wendy-sandbox/desktop-native`; their Annex-B decoder also derives from the iOS companion. This app has no build dependency on either checkout.

## Validation

From the repository root:

```sh
go test -race ./go/macos/... -count=1
xcodebuild -project go/macos/WendyDesktop.xcodeproj \
  -scheme WendyDesktop -destination 'platform=macOS,name=My Mac' \
  -derivedDataPath go/macos/Build/DerivedData CODE_SIGNING_ALLOWED=NO test
```

Go tests cover the gRPC bridge, stream cancellation under backpressure, DNS over UDP and TCP through the packet stack, TCP forwarding and half-close, relay authentication, route validation, and shutdown. Organization tests cover signed request targets, cross-tenant/account refusal and typed allocation ownership. Swift tests cover chunked Annex-B parsing, bounded malformed video input, typed C result ownership, raw video buffers, structured logs, stream generations, and concise error presentation.

A real macOS VPN activation still needs the signed provisioning setup above. Passing packet-stack tests does not establish that a signed extension has been approved or that a particular remote device is reachable.

### Camera wall

Open a device’s Cameras tab to play its feeds together in a grid. Tiles fit the window automatically. Hover over a player for circular pause and full-screen controls. Paused players keep their play control visible. Each camera has its own decoder, error state, retry action, and stream generation. Leaving the wall or switching devices stops its subscriptions.

### Typed C FFI

The build compiles Go with `-buildmode=c-archive` into `libwendydesktop.a` and links it into the macOS executable. Swift and cgo compile against the same [Bridge.h](Sources/Shared/Bridge.h) and [BridgeTypes.h](Sources/Shared/BridgeTypes.h). There is no JSON dispatcher, string method name, generic response envelope, or base64 video at the FFI boundary.

The desktop interface provides named functions:

- `WDInitialize`, `WDAccount`, `WDNearby`, `WDCloud`
- `WDLogin`, `WDCancelLogin`
- `WDConnect`, `WDForgetIdentityAndReconnect`, `WDDisconnect`
- `WDApps`, `WDOpenApp`, `WDStartApp`, `WDStopApp`, `WDCameras`
- `WDResources`, `WDPorts`
- `WDStartLogs`, `WDStopLogs`
- `WDStartCamera`, `WDStopCamera`, `WDStopCameras`
- `WDStartVPN`, `WDStopVPN`, `WDShutdown`
- `WDPollEvents`

Results contain concrete C structs and counted arrays. Status, transport, app state, log level, and event kind have explicit types. Camera events carry raw H.264 bytes. Log events retain nanosecond timestamps, severity, app/service metadata, and multiline bodies. Swift maps event tags into an enum with associated values.

Input pointers are borrowed only for the synchronous call. All returned memory is C-allocated, with no Go pointers crossing the boundary. Each result has a matching typed destructor, such as `WDAppsFree` or `WDEventsFree`; it frees all nested strings, arrays, and byte buffers. Swift copies the result into owned values and calls the destructor before returning to its caller.

The VPN extension links `libwendyvpn.a` and uses [VPNBridge.h](Sources/Shared/VPNBridge.h). `WVPNStart` takes a typed `WDVPNConfig`; packet I/O uses byte pointers and lengths. The separate Apple app-to-extension IPC uses a binary property list because the Network Extension runs in a separate process. The Go relay's existing network protocol is independent of this FFI.

The bundled CLI executable remains the worker for persistent mesh sessions.

### Account and VPN status

The sidebar shows saved cloud account status and the selected context, refreshes on app activation, and shows browser sign-in progress. Successful login refreshes account metadata and the cloud roster.

Cloud authentication does not activate the VPN. Enable the VPN to include all mesh devices and automatic internet backup. The Network view shows devices, signing requirements, and pending macOS approval. The default unsigned build cannot install its Network Extension; use an Apple-signed and provisioned build as described above.

The app's VPN switch is on only when Network Extension reports `connected`. Installation, connecting, reconnecting, and disconnected states do not light the switch. Startup waits for the OS result, with a 60-second timeout, and displays the last disconnect error. Status and profile-change notifications also refresh changes made in System Settings. A visible Bluetooth mesh directory does not establish that the system VPN is connected. Wi-Fi-off startup needs a separate signed-device check: macOS has rejected the tunnel with “No network available” while Bluetooth mesh traffic still worked. The VPN profile now names the actual local relay host, `127.0.0.1`, rather than a display label; that change alone does not verify Bluetooth-only system routing.


### Activity and app controls

Activity uses the same host CPU counters and app/service aggregation as `wendy device top`.
Name is the default sort order. Click the App / Service, CPU, or Memory header to change sorting;
click again to reverse it. Services stay grouped beneath their app. Select a row to see listening
ports in the right inspector. Right-click an app in Apps or Activity to start or stop it.
Start waits for the agent’s acknowledgement and then detaches. Errors stay visible in an alert.

The sidebar has Nearby and Cloud navigator tabs above a plain device list, with search inside
the sidebar. Network routes use selectable tiles. Refresh applies to the selected source.
The account footer displays the email from the signed-in session when available.
Connection attempts show a spinner on the pending device and keep the current connection alive.
Selecting another device cancels the pending attempt; stale queued attempts cannot replace it.

Apps with a declared HTTP entitlement show **Open** beside **Logs**. It opens the
app's webpage in the default browser. LAN devices use their connected hostname;
mesh and cloud devices use a loopback tunnel over the authenticated connection,
without requiring the system VPN. Tunnels close when the device disconnects or
changes. Start stopped apps before opening their pages.

Arrow keys move selection through the device sidebar, Apps, Activity, and Team
using the visible row order. Organization has one page refresh button; the
unrelated device discovery refresh is hidden while it is open. Successful cloud
form submissions dismiss before refreshing the page and cannot submit twice.

Organization opens directly to Team. Device access stays in each teammate's
inspector. The Invitations button opens a sheet for reviewing and revoking links
or creating a new invitation.

### Keyboard navigation on macOS

The Navigation menu lists the window's keyboard shortcuts. Command-F focuses device
search. Return or Down Arrow moves from search to the device list; Up and Down Arrow
select devices. Escape clears search, then returns focus to the list. Command-0
focuses the list and reveals the sidebar if hidden.

Command-1 through Command-7 open Apps, Cameras, Activity, Campaigns, Chat, Data and
Settings in that order. Command-Shift-[ and Command-Shift-] cycle device pages.
Command-[ returns from a settings subpage. Command-Option-1 through Command-Option-4
reach Nearby, Cloud, Organization and Sites. Command-K opens Connect by Address.
Escape closes the address dialog, app logs, mesh graph and mesh connection details.
Navigation commands leave open modal dialogs in place. Command-R refreshes the active
pane, including device settings.

### Model library and campaign models

Chat's **Browse models** opens public Hugging Face GGUF search, with Qwen3.8 Flash Next and official Qwen3 0.6B, 1.7B, 4B and NVIDIA Nemotron 3 Nano 4B defaults. File sizes and quantizations come from live repository metadata. The library checks the selected device's available RAM, reported GPU memory and container storage. It ranks starter models with comfortable estimated capacity first, warns about tight/oversized/unknown capacity, and rechecks before a download. Estimates budget weights plus 25% and 4 GiB for a 32,768-token context; they are not measured peak memory. Discrete GPU memory is separate from system RAM; Jetson uses shared memory. Runtime installation reserves an additional 12 GiB of storage in its estimate.

**Download & Run** installs or starts a dedicated Ollama app on a Linux arm64/x86-64 device, or uses an explicitly selected existing Ollama server. The runtime image is pinned to the multi-platform Ollama 0.40.10 manifest. Compute is selected automatically: NVIDIA GPU when CUDA is reported, including JetPack 5/6 runtime profiles, and CPU otherwise. JetPack 7 Thor uses the standard NVIDIA runtime. The app reuses a compatible managed runtime or automatically selects an available port, streams download progress, runs a one-token readiness check, then selects the model in Chat. The managed server listens on the chosen device port; downloads remain in its persistent volume. Cancellation keeps downloaded files and any already-installed runtime. Gated models, split GGUF files and projection-only weights are not supported. GPU/runtime compatibility is ultimately verified by model loading and generation.

Campaigns support DETR ResNet 50/101 and YOLOS Tiny detection, MobileNet V2 and ViT classification, and compatible custom Hugging Face repositories. The picker reads the repository's label vocabulary and pins its revision. Select up to 32 target labels; matching any label triggers the campaign. **Record a clip and send a notification** combines capture and notification; **Record a clip** keeps capture only. Notification delivery still requires the campaign's Cloud grant. Image classification requires an updated Agent advertising `data-inference-classification`; the desktop refuses to deploy classification to older Agents. Its worker uses safetensors and never enables repository Python code.

## Cloud notifications

On macOS and iOS, **Settings > Notifications > Receive notifications** requests
Apple's notification permission and registers this installation's APNs token with
Wendy Cloud for the selected signed-in account. Chat's campaign notification review
also exposes this control. Campaign creation with `notify: true` automatically
presents a separate Cloud-grant approval in chat after deployment; it does not rely
on the language model choosing a follow-up tool. Denial leaves the campaign intact
and does not grant Cloud permission.

The receiver uses `wendycloud.v2.APNSService/RegisterDeviceToken` and
`UnregisterDeviceToken` through the existing authenticated Cloud connection. It
passes the app's actual bundle ID (`dev.wendy.desktop` or `dev.wendy.mobile`), token,
and APNs environment; the server derives the recipient from the authenticated
user. Tokens stay in memory and are requested again at launch. Opt-in preferences
are scoped to the saved account, Cloud endpoint, and email. The UI reports success
only after Cloud confirms registration. Registration is not proof of delivery.

For live push delivery, enable Push Notifications for these app IDs in the Apple
developer account and sign with matching profiles. `WENDY_PUSH_ENVIRONMENT` defaults
to `development` for Debug and `production` for Release; the iOS profile and this
setting must match. macOS reads the actual signed entitlement. Unsigned builds
cannot register for remote push. Simulator registration depends on APNs support
in the simulator host; use a provisioned device to verify delivery.

The Cloud backend inspected for this change allows only the existing Companion
app topics. Its APNS registration allowlist must also accept `dev.wendy.desktop`
and `dev.wendy.mobile`, and its APNs signing key must cover their app IDs. That
server change and deployment are not included in this repository. The app reports
Cloud rejection instead of using a Companion topic or pretending registration
succeeded. No test token or mock Cloud grant is registered during verification.

Campaign Cloud identities use `sh.wendy.campaign.<name>`, matching their runtime
recording identity. The old `campaign:<name>` form is rejected by Cloud's
notification API because `app_id` does not permit colons. Existing campaigns need
an updated device agent and a new explicit permission approval: enabling a grant
registers the canonical app/asset assignment before updating its permission. Old
grants are not silently copied, and changing the desktop app alone cannot fix a
running older agent's sender.

### Local simulators on macOS

Choose **Simulators** in the sidebar to manage the same `~/.wendy/vms` registry
used by the Wendy CLI. Create a WendyOS VM, Unitree Go2/G1, SO-101 arm,
Rosmaster R2, or ESP32-C6 simulator using WASM or native firmware. Creation
leaves it stopped. Start runs the CLI's boot, provisioning and device-protocol
checks. Power state in the list does not establish robot readiness.

Robot scenes render natively with SceneKit using the runtime's compiled MuJoCo
meshes and live body poses. Drag to orbit, scroll to zoom, follow the robot, or
recenter the camera. Pause/resume and reset use each robot's control protocol.
The view stops polling and releases its renderer when closed. Faults and stale
connections remain visible. ESP32-C6 and generic WendyOS use labeled board
schematics; these are not live GPIO, peripheral, or physical-board simulations.

An older pinned runtime produces an **Update robot runtime** button beside the
error. An agent capability mismatch offers **Update agent**. These actions
apply only to the simulator that failed, even if another row is selected.
The simulator menu also offers these updates and **Prepare simulation** for a
running VM whose robot has not been provisioned. Updates use the existing CLI
implementation. Missing host dependencies and failed downloads remain errors;
the app does not install QEMU or esp-emu implicitly. Finder launches can find
Homebrew tools in `/opt/homebrew/bin` and `/usr/local/bin`.

**Open device** connects WendyOS simulators to the existing Apps, Cameras, Data
and other device panes. For ESP32-C6, use the displayed `wendy run --device vm:NAME`
command to deploy firmware/apps through the CLI's Lite transport. The native
Linux device panes do not implement the Lite protocol. Deletion confirms the
specific simulator and refuses a running VM. This pane is excluded from iOS.

### Wendy Lite devices

Nearby discovery includes Wendy Lite boards over LAN and USB. Cloud Lite devices
connect through the authenticated WendyCom relay. Selecting a Lite device opens
Cameras, Device, Wi-Fi, and Logs. SensorLink cameras support MJPEG and H.264;
the device page shows firmware, board, and architecture. Logs stream the Lite
console. Wi-Fi configuration replaces the saved network list and restarts the
board after confirmation. Use Reconnect after the restart.

Lite devices do not expose the WendyOS container, activity, chat, campaign, or
agent-update pages and cannot be selected for VPN routing. Pair a Lite source
with a WendyOS device to use its cameras in campaigns and recordings.

### Thermal cameras

The macOS and iOS camera walls consume a common thermal measurement plane from
the agent's raw capture stream. The agent identifies adapters using the camera's
USB identity and supported modes. Generic USB camera names and Y16 alone do not
establish thermal or temperature capabilities.

Thermal viewers request adaptive delivery from the agent. Each viewer starts at
5 frames per second, slows when its connection backs up, and gradually increases
when writes remain fast. Queued frames are replaced with the newest complete
frame. This changes that viewer's delivery rate; shared camera capture and
full-rate recording or analytics consumers continue independently. Older agents
ignore the opt-in field. Interrupted streams retry after one, two and four
seconds; a stream without valid frames for 15 seconds is reopened. Temperature
freshness remains independent of these retries.

| Adapter | Capture | Measurement plane | Temperature support |
| --- | --- | --- | --- |
| PureThermal | Y16, 160×120 or 80×60 | Entire picture | Counts by default; explicit operator TLinear scale remains available |
| Original TOPDON TC001 | YUYV, 256×384 | Bottom 256×192 plane | Camera temperature codes in 1/64 kelvin |
| HIK-based TOPDON TC001, USB 2bdf:0102 | YUYV, 256×392 | First 256×192 plane | Sensor pixels remain counts; separate camera summary provides minimum/maximum °C |

The HIK-based revision is the variant inspected on mesh-1. Its four rows after
the sensor plane and four rows after the preview contain metadata, not image
pixels. The encoded viewer receives only the preview at rows 196–387. Raw readers
receive the entire original capture. The draft deliberately rejects other HIK
layouts, including 512×484, for thermal decoding; default capture selects the
supported 256×392 mode. An explicit request for another layout remains raw data.

`VideoDevice.thermal` advertises the adapter and preferred capture dimensions.
`RawFormat.thermal_plane` describes the measurement dimensions, byte offset, row
stride, invalid values, and optional linear conversion to kelvin. Version 1 uses
unsigned little-endian 16-bit samples. Future adapters can emit this contract
without adding vendor logic to the Apple apps. Unsupported versions, malformed
planes, and invalid conversions fail explicitly. An absent conversion means
counts. Palette colors never determine measurements.

The heatmap uses the sensor frame. On HIK hardware, markers and Celsius labels
use the camera's separate temperature summary and its reported positions. The
summary updates at roughly 2.5 Hz, independently of video. The app waits for a
changed summary after connection, preserves its age across repeated metadata,
and hides temperatures after three seconds without a changed summary. While
temperatures are unavailable, the live image keeps MIN/MAX markers without
numeric sensor counts. This includes startup, invalid summary metadata, and
unchanged summaries that reach the freshness limit. A new valid, changed summary
restores the Celsius labels. The app never
applies the summary's conversion to sensor pixels. Markers account for
letterboxing. Stopping or switching cameras clears readings; three seconds
without a new frame dims the image and hides extrema. The bridge carries the
conversion to Swift, and TC001 frames cannot inherit a PureThermal scale override.
The existing raw stream and original payload remain available for recording.

This draft does not enable the HIK vendor's separate calibrated USB stream mode,
apply emissivity corrections, or provide calibrated temperatures at arbitrary
pixels. Temperature accuracy has not
been checked against a reference instrument. New agent capabilities are needed
for generic-name TC001 discovery. Older agents retain the legacy PureThermal and
explicit TC001-name fallback.

Protocol evidence:
- [Original TC001 capture implementation](https://github.com/leswright1977/PyThermalCamera/blob/main/src/tc001v4.2.py).
- [HIK default sensor and preview layout, and distinct calibrated mode](https://github.com/Krastanov/ht-203u-linux/blob/main/THERMOMETRY.md).
- [TC001N USB identity and advertised modes](https://sam.konubinix.eu/blog/posts/making_the_topdon_tc001n_thermal_camera_stream_on_android/).
- [PureThermal radiometry example](https://github.com/groupgets/purethermal1-uvc-capture/blob/master/python/uvc-radiometry.py).

## Camera placement and field of view

Site Model has a searchable camera picker with a live preview. Selecting a
camera with a current pose for this model adds it at its reported position.
Otherwise the user can place a location marker. A location marker alone has no
inferred viewing angle. Sites > Remove Capture moves a model and its placements
to Recently Removed; Restore returns it to the site list.

Both native apps read `ListCameraMetadata` on the existing v2
`WendyCalibratedFrameService` every five seconds. `SetCameraMetadata` lets a device
adapter or localization service publish intrinsics, Brown-Conrady distortion and
poses. Metadata is keyed by the exact `VideoDevice.stable_id`, scoped to the
authenticated gateway, and stored at `/var/lib/wendy/camera-metadata.json`.

FOV comes from calibrated edge rays, including off-center principal points and
supported distortion. The model draws a wireframe cone only when both usable lens
data and a current pose exist. The two-metre drawing length is not a sensor range
or an occlusion/coverage calculation. Yaw is relative to model -Z, toward +X;
pitch is positive upward. These are not compass bearings.

Read or publish metadata with:

```sh
wendy --device GATEWAY device camera metadata list
wendy --device GATEWAY device camera metadata set request.json
```

The JSON file is a protobuf `SetCameraMetadataRequest`: `stableId`,
`expectedRevision`, and `metadata`. Use expected revision `0` only for creation;
read the returned revision before replacing a record. Omit `metadata` to remove
the matching revision. Conflicts return `Aborted`, and failed disk writes never
publish a new in-memory record. Intrinsics reuse the existing `CameraIntrinsics`
type and require provenance, a note, calibration time and capture mode. Publishers
must replace or clear calibration when crop, zoom, focus or lens changes make it
invalid. A datasheet FOV must use `PROVENANCE_ASSUMED`.

Every pose names the `modelRevision` UUID from the site's manifest, not a ROS
`map` or `base_link` frame. Position is in metres in the Y-up model frame.
Quaternion order is x,y,z,w; it rotates optical right/down/forward axes into that
frame. Poses require an observation time and expiry no more than seven days
later. A publisher must renew its evidence; clients hide expired poses. Visual
localization records also require an inlier count and reprojection RMS.

This API accepts pose evidence. The iOS device viewfinder and RoomPlan scanning
now share a local appearance recognizer, described below. Appearance recognition
does not supply a calibrated camera-body-to-optical-axis transform or an enrolled
hardware identity. These remain necessary for fully automatic camera placement.
The RoomPlan export contains geometry without reference images. Automatic
image-based registration cannot be recovered from that export alone. Older
gateways and cameras without calibration continue to provide video and location
markers, with direction/FOV shown as unknown.

### Device viewfinder

On iPhone, open Devices and tap the viewfinder toolbar button. Use the info icon
for recognition counts and processing time. **Save diagnostic frame** explicitly
saves one grayscale camera frame and its diagnostics to
Documents/RecognitionDiagnostic.json inside the app container. It stays on the
phone unless retrieved for debugging; saving again replaces the previous capture.

The camera view uses ARKit frames and local OpenCV SIFT feature matching, geometric RANSAC
verification, and two consecutive observations before displaying a card. Cards
expire after 1.5 seconds without fresh observations. Board recognition runs
independently of OCR and does not require normal AR world tracking. It checks the full image and, after a miss, a centre crop at higher
resolution without loosening geometric checks. The app does not upload camera frames. Two bundled Seeed Studio photographs provide experimental
appearance recognition for XIAO ESP32S3 and ESP32S3 Sense boards. This is
reference-image matching, not a general detector for every device or every view.
Small, blurred, occluded, reflective, or differently enclosed boards may not match.

Recognition cards stay in a fixed panel below the viewfinder toolbar; only the
outline follows the detected device. Tap a card to choose a nearby device and
open its normal authenticated connection.
The sheet displays current device information, application counts for WendyOS,
and a selectable real camera feed for WendyOS or Lite. Failed status reads remove
the connected status; video freshness is displayed separately. An appearance match
or nearby discovery alone never authenticates the visible physical device.
Existing device-name labels can also be recognized using Vision OCR. Whole-name
matches preserve all candidates when names are duplicated.

For other hardware, use **Remember a device’s appearance**, put the device inside
the guide, capture, and choose its nearby connection. This saves one reference
per device on this phone after a successful connection. Saved appearances remain
suggestions because identical hardware can match the same features. They do not
provide lens calibration or automatic identity verification. Remove references
through the stack icon. At most 12 references and 32 MiB are stored atomically in
Application Support/WendyDeviceRecognition/references.json. No cloud sync occurs.

Room scanning uses the same recognizer with RoomPlan's ARSession and shows a
recognized appearance during capture. It does not place cameras or infer their
viewing angles from an appearance-only reference. The native C++ layer supports
metric planar references and checked PnP poses, tested with a synthetic calibrated
fixture, but bundled photographs and user captures have no measured physical
plane and deliberately cannot produce a pose. That code is not evidence of
real-world camera calibration or automatic site alignment.

Builds require CMake and Ninja on the development Mac. `Scripts/build-vision.sh`
downloads checksum-pinned OpenCV 4.12.0 source and caches static libraries per
Apple platform in Build/VisualRecognition. The build bundles OpenCV/Carotene
licenses and Seeed photograph attribution. Run `bash Scripts/test-vision.sh`
for native perspective/pose/negative-image checks; app tests cover rotated board
recognition, ambiguity, reference validation, persistence and removal. Hardware
recognition still requires checking against the actual boards and lighting.

## Unitree pairing and direct control

The device Pairing pane has a Unitree tab for Go2 discovery, account login,
pairing, reconnection, and forgetting through the selected Wendy agent. Pairing
requires an authenticated agent connection. The agent retains the robot key;
the Mac does not save the Unitree password.

Nearby discovery also scans the Mac's LAN for Go2 robots without a Wendy agent.
Choose a robot's **Remote control** entry, or **Add > Connect to Unitree Go2** to
enter a serial number and IPv4 address. Login supports Global and China Unitree
accounts. Direct connections keep the robot key in memory for that connection.
G1 control is not supported by the Go2 LAN protocol.

Connecting never sends movement. Put Go2 in its normal standing mode with the
physical remote, enable movement, then hold a direction button. Releasing,
pressing Stop or Space, losing window focus, closing the view, and quitting
attempt Stop. Moves expire after 500 ms without a refresh, enforced in Go even
if the Swift UI stops responding. Speeds are 0.6 m/s forward/backward, 0.3 m/s
sideways, and 0.5 rad/s turning. A host crash or network loss can prevent Stop
from reaching the robot; keep it in sight and keep the physical remote ready.
No movement is sent during discovery or pairing. Physical robot acceptance
still requires hardware verification.

### Go2 map commands

On macOS or iOS, connect to the Go2's gateway, open **Site Model**, and use the **Go2
Command** panel. The gateway must have a connected Go2 pairing owned by the
current account. The panel includes the original Unitree Go2 meshes, also used
by the warehouse demo, with the license and upstream revision in `Resources/Go2`.

Once a fresh scan arrives, the app searches the room mesh for matching position
and heading candidates. It previews a suggestion only when the best fit is
separated from competing positions and headings. A translucent Go2 and orange
ring preview the suggestion; use **Confirm position** after checking it.
The displayed percentage is weighted surface agreement, not probability of correctness.
A fit that loses independent wall directions when one plane is removed needs a
verified camera match or corroboration from another viewpoint. Corroboration
requires new architectural samples in two directions; replayed accumulated scans
and new samples from only one plane do not count. Saved visual and LiDAR
references in possible door swing regions are excluded from localization because
the scanned door position can change. Live door obstacles still block navigation.
Weak fits are labeled and still require operator verification. Suggestions expire
after 30 seconds or robot motion; confirmation cannot command movement. **Locate Go2**
retries the search. **Place Manually** remains available for unsupported geometry
or a poor scan. Select the robot and click a floor destination
to preview the route; **Move here** or a right-click sends the order. **Escape**
or **Stop** cancels it. A green selection ring, destination rings and route show
the current order. Live joint telemetry articulates the model. The separate
rotatable preview shows a standing pose, including before pairing. On iPhone and
iPad, tap to preview and use **Move here** to send the order. **Go2** opens an
expandable sheet containing its camera, localization evidence and settings.
**Stop** remains available on the map and in the sheet toolbar. Closing the
sheet keeps telemetry and camera localization active. **Cameras** toggles the
camera panel and stops the active order.

The Site Model shows Go2's front camera beside the map on Mac and in the Go2
controls on iPhone. It uses a separate authenticated video subscription bound
to the selected robot and gateway session. Video reads never hold the motion
lock, and leaving the map closes the subscription with the robot session.

Scan matching retains height and surface orientation across six overlapping
height bands, from 25 cm to 1.5 m above the measured floor. Thin planar
neighborhoods can contribute even when individual columns lack 50 cm of vertical
returns. Dense occupied patches and isolated legs do not qualify as walls.
RoomPlan architecture anchors the fit. Movable-object geometry supplies weaker
support; horizontal tabletops are matched separately from vertical faces.
Furniture bounding boxes never declare the space beneath a desk occupied or clear.
Door leaves remain excluded.

Each supporting vertical surface needs six distinct columns spanning 60 cm and
a coherent thin plane. A suggestion needs an architectural anchor, at least 50%
weighted agreement, and two surface directions separated by 20 degrees, or one
direction plus a verified camera pose. Surface normals must agree within 15
degrees. Competing positions and headings survive refinement. A near-equal
alternative rejects the suggestion; the cost separation must exceed the larger
of 0.003 and 18% of the best cost. These are conservative matching heuristics,
not calibrated probabilities.

Go2's front camera can match the site's visual reference map. Camera calibration
and pose must pass the existing reprojection, spatial-spread and lens checks,
then agree on two distinct fresh frames. The image arrival is paired with recent
odometry within 150 ms and compensated for intervening motion. The camera is a
soft position and heading constraint, with uncertainty for its body offset.
Unverified or stale image matches do not influence localization. Visual capture
keeps sampling later viewpoints after reaching the 12,000-landmark budget,
replacing a bounded selection of earlier observations instead of stopping at
capacity. The capture still has a 200-view limit. For useful references, capture
fixed room features from near Go2's camera height during the
iPhone room scan; a phone-height reference map may have little overlap with the
robot's low view. Rotation search preserves any measured lens while finding the robot.

When rotation search needs a measured lens, it opens a calibration sheet using
the live stream and a 7-by-10-square checkerboard (6-by-9 inner corners). You can
also open **Calibrate camera** in Go2's expanded controls without starting a
rotation search. The preview remains raw video; calibration affects matching.
The current solver uses Brown–Conrady distortion, not a dedicated fisheye model. Save the
PNG and display it without stretching on a flat screen, or print it. Keep Go2
stationary and move the whole target through the image centre and edges, with
varied tilts and distances. At least 12 distinct fresh views are required; the
solver rejects repeated views, insufficient coverage and unstable measurements,
and validates on withheld views. No calibration is saved unless validation passes.
Capture starts automatically. Remove the target and choose Continue to resume
room localization; Cancel ends the search.

Measured Brown–Conrady intrinsics are saved separately from room references in
Application Support/WendyRobotLenses, keyed by robot serial, codec and image
dimensions. Images are rectified before feature extraction; matched coordinates
are mapped back to raw pixels for pose validation with the measured distortion.
A measured-lens failure never silently retries with assumed intrinsics. A different
stream mode needs its own measurement. This corrects lens geometry, but does not
guarantee a pose when fixed-room reference features are missing or ambiguous.

The matcher retains up to four distinct viewpoints, each for at most 30 seconds
from acquisition. Continuous retries do not extend that lifetime. New evidence
requires at least 20% novel occupied cells plus 20 cm of translation or about 10 degrees of
rotation. Repeated accumulated voxel-map packets cannot increase viewpoint
count. Historical agreement can distinguish alternatives but cannot make a
geometrically invalid current fit valid. This does not drive the robot to collect
more views.

A detected pose discontinuity also discards the Mac's cached scan and waits for
a new LiDAR packet. These resets clear client evidence only. Wendy does not
currently expose a command to clear Unitree's onboard accumulated voxel map, so
a new packet may still contain old observations.

Confirming a suggestion saves observed surfaces aligned to the site. The saved
robot map is bound to the robot, model revision and exact model digest, capped at
3,000 cells and expires after seven days. Only observations matched to
architecture become stable anchors. Furniture references remain weaker evidence.
Saved maps are localization targets and never navigation free space.
RoomPlan chair boxes are excluded from localization geometry because they can
represent only the seat and omit the backrest. Visual and saved robot references
within each chair's horizontal footprint, expanded by 15 cm, are excluded at all
heights when loading localization evidence. This leaves the displayed model and
live LiDAR obstacle grid unchanged. A furniture-only explanation that fits as
well geometrically prevents an unverified architectural corner from resolving
the pose solely through its stronger semantic score.
**Localization evidence** shows camera status, viewpoint and surface counts,
and an optional provisional scan overlay: green architecture, orange furniture,
blue saved surfaces and gray rejected/unmatched samples. An unconfirmed overlay
is explicitly labeled as a hypothesis.

Measured clear floor supplies contradictory evidence. Only cells with a fully
observed, obstacle-free 3 by 3 neighborhood contribute; hidden-floor departure
allowances never enter matching. A modeled wall within 7.5 cm of such a patch's
interior adds a penalty. More than 5% conflicting samples rejects a suggestion.
Unobserved floor does not imply walls or missing walls.

Localization requires fresh pose and surface observations independently of
navigable-floor validation. A position can be previewed and confirmed while
movement remains blocked by uncertain floor. Automatic matching tries up to
three times per unchanged observation set, at least five seconds apart, and
pauses after manual placement or Stop. It supports single-level rooms up to
30 m across; tracking after confirmation uses odometry. Alignment is discarded
when the site, model revision, gateway session or active view changes. Pose
jumps also require alignment again. Do not issue orders from another controller
while a map order is active. Obstacle, floor, timestamp and movement checks are
unchanged by the localization evidence sources.

Movement always requires fresh, precisely timestamped odometry on `rt/lf/sportmodestate`.
LiDAR source timestamps on `rt/utlidar/voxel_map_compressed` are used when precise.
Both four-byte and twelve-byte LiDAR packet headers are supported. Some firmware
rounds the map's Unix capture time to 1,000-second increments, such as
`1.792944e+09`, losing subsecond timing. The UI reports this separately from robot
connectivity. Packet receipt time and a ROS
bridge's receipt-stamped cloud do not establish the original capture time.

**Supervised arrival timing** is enabled by default, as requested for this firmware.
Turn it off to require verified LiDAR capture times. New, advancing gateway receipt times must stay fresh and within
200 ms of fresh, precisely timestamped odometry. Coarse epochs must still agree
within their rounding precision and cannot go backwards. The UI labels this mode;
it cannot verify sensor capture age. Switching modes stops any active order and
discards its route, scan and position candidates. Each connection starts with
supervised arrival timing enabled and visibly labeled.

The floor reference comes from a dominant upper surface in nearby voxel columns,
rather than odometry Z=0 or each column's lowest occupied voxel. Go2's processed
voxel map can contain occupancy beneath an observed walking surface. Nearby
raised objects are excluded from the ground estimate and remain obstacles.
The selected surface needs substantial support on all four sides of Go2;
competing surfaces with comparable support are rejected. Buried occupancy does
not invalidate floor observed above it. Coherent lower surfaces with no floor
above them are blocked as drops. Lower-surface evidence must span both horizontal
axes and fit a locally level top plane; three neighboring buried voxels are not
enough. Irregular lower occupancy remains unknown, including under the robot.
It never creates navigable floor and only the explicit fixed departure allowance
can tolerate it.

**Allow hidden floor beneath Go2** is off by default on every connection. An
operator may enable it after checking that Go2 stands on a clear, level floor.
It permits missing floor only in 10 cm grid cells touched by the 70 by 31 cm body envelope
rectangle at the departure pose. That footprint stays fixed for the route; it
does not follow the robot or fill unknown floor ahead. Obstacles and measured
drops remain blocked, and destinations require fully observed clearance.
Changing this option stops active motion and discards the route and candidates.

The client measures the authenticated gateway clock when connecting, using
three read-only GetClock probes and the fastest round trip below 50 ms. It
translates gateway receipts and embedded capture times with a fixed conservative
offset. This avoids treating ordinary iPhone/gateway clock skew as a telemetry
reset without extending the 750 ms freshness limit. Rejected delayed or duplicate
pose packets do not erase the last accepted pose or its continuity check. Actual
pose jumps and clock rollbacks still invalidate alignment. The latest packet
rejection and timing measurements stay locally in
`Library/Caches/Wendy/go2-observation-fault.json` for device diagnosis.

If the Go2 observation stream disconnects, or delivers no odometry packets for
five seconds, the client stops movement and reports the stream failure. It closes
the old handle and retries telemetry up to three times, after one, two and four
seconds. Recovery discards the old alignment, route and rotation search; movement
requires a new order. The 750 ms observation freshness checks still apply during
the five-second stream timeout. Leaving the view or deliberately replacing a
connection cancels recovery. If retries fail, use **Reconnect** in the Go2 panel.

Unstamped odometry can display the robot but cannot enable movement. The planner
uses observed level floor and occupied voxels, treats unknown cells as blocked,
and checks an oriented 70 by 31 cm standing envelope with no extra horizontal
stand-off, using [Unitree's standing dimensions](https://www.unitree.com/go2/).
Raised obstacles retain their voxel size and position within the 10 cm grid;
the collision check tests those voxels against the swept body, including turns.
Overhead returns above the body top are excluded from navigation obstacles,
but remain available for localization. The body top is at least 40 cm above
the measured floor and rises with the odometry base, allowing 10 cm from the
base to the top. Voxel half-height is included in the vertical overlap check.
Measured drops still block their entire floor cells. Attachments beyond the
standard body dimensions require a larger envelope. The planner
retains arrival heading at each search node and checks the actual odometry
heading during movement. Translation and rotation are separate, walking is
commanded at 0.55 m/s without tapering below the physical walking minimum. The forward stopping check
covers the command lease, a 0.5 m/s² braking assumption and 5 cm tracking tolerance.
These assumptions still need physical verification. Destinations are limited to 8 m, routes to
12 m, and each order to 30 seconds. A robot must already be upright; this UI
does not change its posture or motion mode.

This is supervised waypoint movement. Unitree's voxel map accumulates observations
and is not a safety-rated live obstacle sensor. Keep the route clear and remain
with the robot. The controller stops on stale observations, lost UI activity,
failed commands or a blocked route. The agent separately expires MOVE after
500 ms. Tests cover synthetic closed-loop arrival, obstacle/unknown-floor
rejection, source timestamps, command ordering, failed Stop, and lost leases;
they do not establish physical navigation performance.

Passive localization starts when the map opens. If matching produces no reliable
candidate, a popup offers **Start rotation**. It also appears after five seconds
with a live pose but insufficient scan structure. **Not now** keeps passive
matching active and suppresses further popups for that observation session.
The popup disappears if the pose is lost, Go2 moves, or its position is aligned.
There is no **Locate Go2** button. Accepting the popup starts a bounded search:
match, turn if ambiguous, stop, wait for a fresh view, then match again. If the
lens has no measurement, the checkerboard sheet comes first. The search stops
when a suggestion is available, after a full circle of up to twelve 30-degree turns, after five
minutes excluding calibration, or on Stop/fault. It never confirms a pose.
If a full rotation or the time limit produces no match, the panel keeps a
**Search finished without a match** result and stops automatic retries.
**Try rotation again** asks for confirmation before another search; **Place
Manually** uses the existing position-and-heading placement flow.

For local diagnosis, launch with `WENDY_LOCALIZATION_DIAGNOSTICS=1` to keep the
latest bounded match input and result in
`Library/Caches/Wendy/go2-localization-evidence.json`. This opt-in file contains
room geometry and observed surfaces, stays on the Mac, and updates at most once
every five seconds. It enables offline replay without commanding the robot.

Turns use the live odometry frame before site alignment. The controller chooses
a clear left or right turn at the navigation turning rate of 0.8 rad/s,
without tapering below that rate. Each turn has an eight-second limit, a
two-second progress watchdog, and an 8 cm drift limit. Clearance covers the whole
body and the angular command lease/braking margin. The hidden-floor
allowance stays fixed to the original footprint. Obstacles, drops, missing floor,
telemetry faults, lost UI heartbeat, Stop, or disconnect abort the search. New
geometry is still required to count another view as independent. Hardware motion
has not been verified by the build/tests.

The calibration sheet displays the existing live video decoder independently of
checkerboard detection and lens solving. Its checklist separates sample count,
image coverage and final validation, with directional guidance from measured
checkerboard coverage. At capacity it replaces older samples one at a time;
it never silently discards the entire collection and resets the counter.
On iPhone, the complete checkerboard stays fixed above the preview, checks and
current instruction. Only the status area scrolls; in landscape it sits beside
the pattern. The options menu contains Help and Save checkerboard. Opening Help
pauses sample collection. Lens validation also pauses collection while checking
the saved views. Failed checks distinguish insufficient tilt, uncertain lens
parameters, disagreement between view groups, and validation-view errors.
