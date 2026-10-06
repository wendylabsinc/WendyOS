# Wendy robots in ChatGPT

## Approved experience

The user chose talking to a robot, seeing its camera, and using its apps as the
first experience, and approved implementing the proposal on 2026-09-29.

Open Wendy from the sidebar or beside a conversation. Select an authorized
robot from configured targets or Cloud inventory, inspect its apps and cameras, capture a timestamped image for
ChatGPT, and start or stop an allowed app. Expose approved app MCP tools without
giving the conversation unrestricted shell, installation, or network access.

## First implementation

- A robot-focused MCP gateway using the existing Go agent clients and Cloud
  connection routing. Every device operation includes a stable robot ID.
- Explicit deployment configuration maps robot IDs to existing device selectors
  and lists apps and app tools the operator permits. Never accept a model-provided
  address, port, shell command, or credentials.
- Stdio supports a private Secure MCP Tunnel with the operator's Wendy login.
  Streamable HTTP requires bearer authentication and explicit subject grants.
  OAuth deployments validate access tokens through a configured authorization
  server's introspection endpoint, with issuer, audience, expiry, and scopes.
  Development tokens are an explicit separate mode, never an authentication fallback.
- Each device operation uses an explicit target and its own connection. Live
  camera previews and app web views retain bounded connections until stopped or
  expired. There is no shared current device or implicit default target.
- An embedded MCP Apps panel registers global and thread entrypoints. The UI
  selects robots, shows apps/cameras, captures images, and shares selected robot
  and image context with the model. All actions go through host tools/call.
- App tool exports use stable, configured names and schemas. Discovery and
  invocation verify the configured contract; unreviewed tool changes fail closed.
- Camera activation is explicit. Live preview streams frames to the UI; a frame
  attached to ChatGPT is labeled as a still image. Intercom, physical robot
  movement, public-directory publication and Cloud OAuth delegation remain separate.

## Desktop implementation approved on 2026-09-30

The desktop workspace expands this pilot with a fleet sidebar entrypoint, a
device thread entrypoint, static Wendy device illustrations, device mentions, settings,
and explicit camera and event actions. The user also approved installation,
development, deployment, and fleet workflows described in
`specs/2026-09-30-openai-mcp-desktop-plan.md`.

`wendy.data.notification` uses MCP Events webhook subscriptions for immediate
Wendy Data detection and named-event notifications. ChatGPT must create the
subscription and verify its callback before monitoring is active. A persistent
gateway outbox delivers signed notifications and resumes after restart;
ordinary MCP1 `wait_for_device_event` remains a bounded tool-result alternative.
Cloud episode-ingestion notifications are separate. Camera images are shared
only by explicit capture or attachment.

Implementation verification must distinguish protocol fixtures from an actual
ChatGPT desktop session and device execution.

The implemented workspace opens on a device card gallery, with no preselected
device. It uses static renders of the existing Wendy models, official logos and
Geist fonts. Controls use neutral host colors. Online sidebar dots
are green. Applications have cards with running/stopped status icons and
permission-aware play/stop controls. Unknown readiness is omitted from the UI;
the backend retains it when there is no application evidence.

Logs and Metrics show bounded readable samples. Camera preview uses one device
stream and a decoder per session, with latest-frame buffering, a 15-second idle
lease and a 10-minute maximum. Preview images remain in UI-only metadata until
the user attaches a still frame. Changing devices, tabs or camera, hiding the
page, or clicking Stop releases the session.
Camera reads have background priority and cancellation. A last-sequence hint
omits unchanged JPEGs while renewing the lease. The decoder is capped at 5 fps
and the app reads at most 4 frames per second.

Local simulator tools reuse Wendy's existing VM lifecycle and robot viewers.
Go2 and G1 retain their MuJoCo scene contract. ROSMaster R2 uses its existing
procedural robot model and Ackermann observer state, adapted to the same canvas
geometry and world poses. Only read-only scene/status GETs are used; controller
ownership and credentials are excluded. ROSMaster appears in the simulator
profile picker and has the same embedded View simulation action.
The gateway checks the viewer's simulator identity, profile and health before
returning its loopback URL. The UI draws the scene directly with a canvas and
calls the app-only `simulator_scene_read` tool with a session ID and `geometry`
or `state`. Scene data stays in UI-only `_meta.scene_data`. Geometry loads once
per scene; pose batches load at most four times per second with one request active
and background priority. The gateway captures observer poses at up to 30 Hz,
retaining at most 90 frames or 1 MiB per view. The widget does not read resources
outside its scope.
Large geometry responses can use opt-in gzip in UI-only `_meta.scene_data_gzip`,
with a 32 MiB expanded response limit. The tested G1 geometry tool result shrinks
from 11.4 MB to 4.8 MB. Initial geometry loading allows 45 seconds for host
transfer; pose reads retain a 15-second limit. Older widgets receive JSON.
The renderer replays actual capture timestamps and only interpolates when both
capture and physics gaps are at most 100 ms. It shows intermediate recorded
poses, including brief foot lifts between host polls, instead of blending widely
separated snapshots. A slow render cannot jump past the next captured pose.
Late batches hold playback; the browser queue is bounded to 180 poses and lost
history is explicit. History clears on world changes and suspension. Hidden
views use app-only `simulator_scene_pause` to stop capture; a 5-second idle lease,
session expiry and close also cancel it. Physics remains unchanged.
Background queue dispatch no longer adds a fixed 75 ms wait to every request.
View simulation requests fullscreen immediately and opens a focused viewer,
with camera controls and canvas fitted to desktop and narrow viewports. Host
refusal or lack of fullscreen support falls back to the focused inline view.
Back to simulators releases the session and restores simulator management.
Failed loading stops the loading status and exposes the underlying error in
expandable details. It does not need
browser access to localhost or a nested iframe. A browser viewer remains available.
Simulator access has its own local-only policy switch and scope,
without enabling OS installation.

The Devices grid and device sidebar omit simulator entries. Simulators remain
in their dedicated tab. The catalog reports the simulator count before
pagination so the Devices total excludes simulators on every page, including
explicitly configured VM entries.

The local development loop also exposes directory listing, UTF-8 file reads,
and revision-checked writes within approved workspace roots. `projects:write`
is separate from project reads and `apps:deploy`. File tools are local stdio only.
A workspace can opt into running local simulators with `allow_simulators`;
explicit VM policies still take precedence. This supports authoring code in
ChatGPT and deploying through the existing durable jobs without granting an
arbitrary host path or shell. A simulator-only policy requires no physical device.

Running apps with an HTTP entitlement have an Open app control. A bounded
loopback proxy routes their declared port through the device connection. Its
access cookie, exact Host/Origin checks and expiration protect the local view.
The tool does not start the app or accept arbitrary addresses or ports.

The current resource URI includes the built workspace's content hash. Tool visibility,
subject scopes and settings capability discovery agree. Existing plugin
connections need a metadata refresh and a fresh conversation after an upgrade.
The UI reports this requirement when a host still has an older trusted catalog.

## Composer device mentions

In a desktop host that supports OpenAI composer mentions, users can search Wendy
devices by name or stable ID and attach one to their prompt. `search_devices`
advertises `openai/extensions.mentions/search` with app visibility, a required
string `query` that accepts empty text, and an output schema for
`structuredContent.items`. Items are MCP resource links to
`wendy://devices/{id}`. Search includes authorized configured devices, Cloud
enrollments including offline devices, and permitted running local simulators.
Return at most 100 matches; users can narrow the query for larger fleets.

The resource reader checks authorization again and returns the device name and
`robot_id` for subsequent tools. Mention search and resource reads do not connect
to the device, activate cameras, or select a shared current device. Connection
state remains unknown until inspected. Incomplete discovery is reported in
result metadata without exposing private backend errors. Tests cover the wire
contract, filtering, bounds, scope isolation and revoked Cloud inventory.

## Pilot tools

- list_robots: configured robots and live Cloud inventory authorized for this
  principal, with optional offline devices, name/ID search and pagination.
- open_robot: render the robot panel, optionally with an explicit robot ID.
- inspect_robot: current device version, app states, and cameras.
- capture_robot_image: fresh finite snapshot with robot/camera/time metadata.
- start_robot_app, stop_robot_app: operate on configured apps or all installed
  apps when the operator enables `allow_all_apps`; inspect state afterward and
  keep application readiness unknown without application evidence.
- Configured app tools: explicit robot routing and the app's published schema.

## Cloud discovery correction

The user reported that the pilot listed only its two configured test devices.
Live Cloud inventory sources are now bound to an explicit endpoint and
organization or tenant. Grants name the sources each subject may read. Listing includes online
devices by default, supports offline inventory, search and pagination, and keeps
configured aliases and action policies for existing targets. Cloud presence does
not prove an agent connection. New discoveries allow inspection, including app
states; camera capture is opt-in per source and app control remains explicit.
The `allow_all_apps` option permits installed-app control on a robot or Cloud
source, including future installs, subject to grant and token scopes. It defaults
to false. Explicit robot policies take precedence over source options. Local
simulators have a separate `allow_simulator_device_access` opt-in for cameras
and installed apps; local simulator authorization and operation scopes still
apply. Shared HTTP sessions cannot access those local simulator entries.
Discovery failures report an incomplete inventory instead of presenting the
configured test devices as the user's complete account.

## Deployment dependencies

Wendy Cloud relay access requires the existing operator authentication and tunnel
signing credentials. The ChatGPT OAuth resource and client must be provisioned
in an authorization server before the OAuth HTTP mode can be deployed. This
repository cannot assume those registrations or the Cloud delegation service
already exist. A private tunnel is the initial real-account connection path.

## Verification

The simulator widget uses app-only tool calls because ChatGPT rejects scene
resource reads outside the widget's scope. The integration host now rejects
those reads with the same scope error. Real, already-running Go2 and G1 scenes
passed browser verification through the gateway tool handlers with browser
localhost access blocked. Checks cover geometry loading once, pose updates at
most four times per second, one active request, camera controls, visibility
suspension, recovery, failure details, fallback and cleanup. The stdio gateway
also returned Go2 geometry and poses only in UI metadata. The gateway was rebuilt
and reloaded on the existing private tunnel. After refreshing Wendy's tools and
opening the Go2 simulator view, the user confirmed that the robot visibly renders
inside ChatGPT. This host check was user-verified because the available
computer-use tool denied access to the native ChatGPT window.

The running ROSMaster R2 simulator also passed the observer browser check with
localhost access blocked. Its detailed robot visibly rendered from one small
geometry response, with pose updates, camera controls, visibility suspension,
recovery and session cleanup. The test issued no simulator control commands.

The integrated revision passed all 26 widget tests, TypeScript checking, the MCP
and CLI command suites, the Go2 bundle suite, and targeted gateway race tests.
Browser checks passed for Go2, G1, and ROSMaster, including immediate fullscreen,
mobile viewport fit, host refusal, and return-to-management cleanup. Fixture
camera checks covered canceled background reads and unchanged-frame retention.
The gateway was rebuilt and reloaded with the green Running indicator. Actual
ChatGPT verification of the new G1 transfer, ROSMaster view, and fullscreen flow
is pending the user's check after refreshing tools and opening a new conversation.

Captured-pose replay passed all 30 widget tests, the MCP and Go2 bundle suites,
and targeted gateway race tests. Regression checks preserve a brief foot lift
and landing between host polls, including late batches and slow rendering.
Observer browser checks passed for Go2, G1, and ROSMaster. Actual G1 gait playback
in ChatGPT still needs a user check after refreshing tools and opening a new view.

Use real MCP HTTP and stdio transports with fixture agent/app servers. Test
authentication, audience/scope enforcement, cross-principal robot access,
concurrent target isolation, schema drift, bounded results, cancellation, and
app state verification. Exercise the panel in a browser host fixture. Run a real
robot check once the user identifies the target; do not equate fixture results
with hardware or ChatGPT-host verification.

### Results from the pilot

On 2026-09-30, real Cloud-connected Jetsons passed gateway inspection. The
companion app passed stop/start state verification, a message write, and its own
readiness response with the written message. Another device returned a finite
1280 x 720 JPEG through the gateway. That image was black; transport success does
not establish a useful scene. An advertised ROS camera on the companion device
did not produce a frame within the capture timeout.

The pilot panel rendered in a browser host fixture. The desktop replacement uses
the official MCP Apps and OpenAI Extensions SDKs instead of the pilot's custom
bridge. Go tests cover MCP transports, fixture gRPC agents, authorization, tool
contracts, camera cleanup, task cancellation and app state. The TypeScript
workspace has a type check and a reproducible embedded bundle build.

The user's ChatGPT screenshots confirm the workspace mounted, device inspection,
app listing and still camera capture through the private tunnel. The latest
desktop revision still needs a host check after refreshing cached metadata.
OAuth linking remains unverified. No public service or directory submission was
created by this pilot.

The discovery correction passed the full MCP and CLI command test suites,
gateway race tests, and the former pilot bridge tests. A real Cloud check returned
376 enrolled records over multiple pages, with successful name searches for
the G1, AGX Orin, and Enmax01. Online counts changed during testing; they are
fresh inventory observations, not a fixed fleet size. The private gateway
binary and policy were updated and its existing tunnel runtime restarted.

The desktop revision passed the MCP, CLI commands, agent data and agent services
test suites, gateway race checks and TypeScript checking. Real Beecam preview
returned advancing 640 x 360 frames and stopped successfully. The running G1
badge reader returned HTTP 200 through its declared port over the Cloud proxy.
Its approved app allowlist was enabled without changing app states. Local VM
listing was checked; creating a new Go2 VM and rendering its moving scene in
ChatGPT remain unverified. The event RPC requires updated device agents, and
unattended model activation is not assumed.

## Sources

- https://developers.openai.com/plugins/build/extensions
- https://developers.openai.com/plugins/build/chatgpt-ui
- https://developers.openai.com/plugins/build/auth
- https://developers.openai.com/plugins/deploy/connect-chatgpt
- https://developers.openai.com/plugins/deploy/app-review

## MCP Events and YOLO

The gateway accepts authenticated, self-contained MCP2 requests at the same
stdio or HTTP endpoint as its legacy tools. Complete responses retain tool and
UI metadata. MCP1 task augmentation remains supported only through MCP1;
MCP2 task continuations are not advertised. Skills and device mentions remain
available alongside notification and inference tools.

YOLO deployment takes a Hugging Face model reference, resolves its immutable
revision, and starts a named Wendy Data campaign on an explicit authorized
camera. The first backend supports CPU YOLOv8/YOLO11 float32 raw ONNX detection
exports, with inspect and stop tools. Updated device Agents are required for
both this backend and the durable notification journal. Deployment success
alone does not establish inference readiness or an active ChatGPT subscription.

See the README for scope requirements, storage, supported model formats, and
host verification steps. No physical detector was deployed as part of these
implementation checks because no target and model reference were selected.
