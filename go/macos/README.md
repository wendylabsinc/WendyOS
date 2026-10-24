# Wendy for macOS

A SwiftUI app linked to this repository's Go CLI packages through a C archive. It lists nearby LAN and authenticated mesh devices, connects to Wendy Cloud, displays running apps and service states, streams logs, and displays a camera wall of simultaneous H.264 feeds using each camera’s native resolution and frame rate.

The app embeds a packet-tunnel Network Extension. Select mesh or cloud devices in the toolbar VPN popover or the macOS menu bar, then enable the VPN to resolve their Wendy DNS names and route TCP connections from other Mac apps.

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

LAN and authenticated Cloud transports are supported. The detached mesh worker,
system-wide VPN extension, USB flashing, and host simulator management are not
ported to iOS. Tools requiring those desktop facilities cannot run there. Keep
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
- **Chat** uses Wendy Chat's model discovery and streaming provider with a transport fixed to the selected device. Choose its OpenAI-compatible server port and an advertised model. Mesh/Cloud uses the authenticated tunnel; LAN uses the selected device's address and requires a reachable server port. Common ports are Ollama 11434, llama.cpp 8080, and LM Studio 1234. Environment API keys and hosted-provider settings are never inherited. Stop cancels the generation; changing devices cancels the request. Assistant replies render Markdown and hide model thinking blocks, including Qwen GGUF templates that omit the opening tag. Conversations are held in memory. Each device session starts the bundled Wendy MCP with all tool groups and bundled skills. Read-only tools run directly; changes require approval of the displayed function and arguments. Ollama uses a per-request 32,768-token context for the tool catalog.
- **Data** opens recorded Wendy Data episodes with a shared cursor, a custom pannable and zoomable timestamp ruler, channel timeline, numerical plot, events, and sample inspector. Camera playback is available for checksummed sealed MP4 clips whose frame schedule matches the capture index; JPEG/PNG application frames also follow the cursor. Samples align on the episode's CLOCK_BOOTTIME timeline, including negative pre-roll. Unknown clocks, other boot IDs, unsupported payloads, and download limits are reported rather than assigned invented timestamps. This is episode replay, not a live ROS/point-cloud viewer.

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

- The extension installs a `/32` route for each selected synthetic address in `10.99.0.0/16`, plus the DNS resolver `100.64.0.53`.
- Split DNS applies only to `mesh.wendy.dev` and `cloud.wendy.dev`. Names are `device-<device-id>.mesh.wendy.dev` or `device-<device-id>.cloud.wendy.dev`. The UI lists exact names and addresses.
- The address is a deterministic hash of the full selector. Unlike the older companion's numeric mapping, this supports UUID devices and cloud endpoint/organization identity. Conflicting addresses or names are rejected instead of silently reassigned.
- DNS supports UDP and TCP. A queries for selected devices receive their address, AAAA queries receive no data, and unselected names receive NXDOMAIN. Public names in the matched zones, including the cloud dashboard apex, are refused by this resolver. macOS 26 DNS failover lets them use the system resolver. Other zones are not selected by the split-DNS settings.
- TCP flows terminate in gVisor, then pass through an authenticated loopback relay to the existing mesh or cloud port dialer. Routes pin a transport; selecting Cloud does not silently switch to LAN.
- A fresh random relay credential travels in the ephemeral VPN start options. It is not saved in the VPN profile. The extension never receives the cloud access token or operator private key.
- The relay accepts only selected addresses, valid ports, and its current credential. It limits concurrent flows. Stopping the VPN closes active relay connections. A heartbeat removes the tunnel if the app exits or crashes.
- UDP application traffic, ICMP/ping, IPv6 application routing, and automatic mesh/cloud failover are not implemented. TCP half-close is retained when the underlying CLI transport supports it; the existing cloud byte-pipe transport closes the connection when half-closed.
- Keep the app open while the VPN runs. Route selection changes take effect when **Apply selection** is clicked. Normal internet traffic remains on the Mac's existing network.

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

Cloud authentication does not activate the VPN. Select at least one routable device, then enable the VPN. The Network view explains an empty selection, an unsigned build, or pending macOS approval. The default unsigned build cannot install its Network Extension; use an Apple-signed and provisioned build as described above.

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
