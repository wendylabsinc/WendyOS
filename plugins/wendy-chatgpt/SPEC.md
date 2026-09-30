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
device thread entrypoint, existing Wendy 3D assets, device mentions, settings,
and explicit camera and event actions. The user also approved installation,
development, deployment, and fleet workflows described in
`specs/2026-09-30-openai-mcp-desktop-plan.md`.

`wait_for_device_event` returns a detection as a tool result. A host must have
started that wait and must deliver its result to the conversation. Ordinary MCP
notifications do not promise to start a model turn. Camera capture remains a
finite, explicit action, separate from an event's detection-time evidence.

Implementation verification must distinguish protocol fixtures from an actual
ChatGPT desktop session and device execution.

The implemented workspace opens on a device card gallery, with no preselected
device. It uses the existing Wendy models, a shared WebGL renderer, official
logos and Geist fonts. Controls use neutral host colors. Online sidebar dots
are green. Applications have cards with running/stopped status icons and
permission-aware play/stop controls. Unknown readiness is omitted from the UI;
the backend retains it when there is no application evidence.

Logs and Metrics show bounded readable samples. Camera preview uses one device
stream and a decoder per session, with latest-frame buffering, a 15-second idle
lease and a 10-minute maximum. Preview images remain in UI-only metadata until
the user attaches a still frame. Changing devices, tabs or camera, hiding the
page, or clicking Stop releases the session.

Local simulator tools reuse Wendy's existing VM lifecycle and MuJoCo viewer.
The gateway checks the viewer's simulator identity, profile and health before
returning its loopback URL. The UI opens the viewer in a browser and offers an
embedded view. Simulator access has its own local-only policy switch and scope,
without enabling OS installation.

Running apps with an HTTP entitlement have an Open app control. A bounded
loopback proxy routes their declared port through the device connection. Its
access cookie, exact Host/Origin checks and expiration protect the local view.
The tool does not start the app or accept arbitrary addresses or ports.

The current resource is `ui://wendy/device-workspace-v3.html`. Tool visibility,
subject scopes and settings capability discovery agree. Existing plugin
connections need a metadata refresh and a fresh conversation after an upgrade.
The UI reports this requirement when a host still has an older trusted catalog.

## Pilot tools

- list_robots: configured robots and live Cloud inventory authorized for this
  principal, with optional offline devices, name/ID search and pagination.
- open_robot: render the robot panel, optionally with an explicit robot ID.
- inspect_robot: current device version, app states, and cameras.
- capture_robot_image: fresh finite snapshot with robot/camera/time metadata.
- start_robot_app, stop_robot_app: operate only on configured apps; inspect state
  afterward and keep application readiness unknown without application evidence.
- Configured app tools: explicit robot routing and the app's published schema.

## Cloud discovery correction

The user reported that the pilot listed only its two configured test devices.
Live Cloud inventory sources are now bound to an explicit endpoint and
organization or tenant. Grants name the sources each subject may read. Listing includes online
devices by default, supports offline inventory, search and pagination, and keeps
configured aliases and action policies for existing targets. Cloud presence does
not prove an agent connection. New discoveries allow inspection, including app
states; camera capture is opt-in per source and app control remains explicit.
Discovery failures report an incomplete inventory instead of presenting the
configured test devices as the user's complete account.

## Deployment dependencies

Wendy Cloud relay access requires the existing operator authentication and tunnel
signing credentials. The ChatGPT OAuth resource and client must be provisioned
in an authorization server before the OAuth HTTP mode can be deployed. This
repository cannot assume those registrations or the Cloud delegation service
already exist. A private tunnel is the initial real-account connection path.

## Verification

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
