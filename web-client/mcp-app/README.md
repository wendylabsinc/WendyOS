# Wendy desktop workspace

React/TypeScript MCP App embedded in the Go gateway. Build from this directory:

```sh
npm ci
npm run check
npm run build
```

`build.mjs` bundles the official MCP Apps bridge, OpenAI Extensions, React,
static device illustrations, styles, fonts and logos into
`go/internal/cli/mcp/desktop_app.html`. Commit the generated HTML with source
changes, then rebuild the CLI. Runtime scripts do not load from a CDN.

`node scripts/chatgpt-panel-preview.mjs` from the repository root serves a local
host fixture. It is a development preview, not proof of ChatGPT host behavior.
The gateway verification script and Go tests exercise the actual transports.

The gateway exposes both a global fleet entrypoint and a conversation entrypoint.
The root view does not select a device. Tool calls use explicit device IDs;
responses from an older selection cannot replace the current device view.
Host theme variables style neutral controls. Green and red convey device and app
status. Camera frames enter model context only through explicit attachment.

Device cards use bundled static images from Wendy's existing marketing assets.
`src/assets/devices/manifest.json` records source paths and hashes. Robot images
are still renders of the existing robot meshes. Unknown hardware uses a generic
illustration; simulators carry a VM badge. These images are separate from the
simulator's live MuJoCo scene. The fleet downloads no GLB files and creates no
WebGL canvas. Display-model GLBs are not stored in this repository.
Geist's license is in `src/assets/Geist-LICENSE.txt`; logos come from Wendy's brand
assets.

Fleet rendering uses discovery metadata without opening background connections
to every device. Opening a device inspects identity, apps and cameras concurrently
on one authenticated connection. Refresh preserves display identity only; access
and presence always come from fresh discovery. A small request queue prioritizes
user actions over preferences and cancels reads left behind during navigation.

Metrics request a bounded recent history and graph each OTLP series separately.
Charts preserve timestamps, units and cumulative/interval semantics; a single
sample is shown without inventing a trend. Refresh sample loads new data while
keeping the previous sample visible. Logs use time, severity and message columns.

See [gateway setup](../../plugins/wendy-chatgpt/README.md) for scopes, local
simulators, app web views and ChatGPT connection refresh instructions.

## Live simulator view

Simulators appear in the Simulators tab, separate from the Devices grid and
device sidebar. The Devices count excludes simulator entries across all pages.

Opening a Go2 or G1 simulation renders its MuJoCo scene directly in the MCP App.
ROSMaster R2 also renders here. Its existing procedural robot model supplies the
geometry, while its observer status supplies the rear-axle pose, steering and
wheel rotation. The gateway adapts its read-only `/api/scene` and `/api/status`
responses without changing or restarting the R2 runtime. It explicitly selects
geometry and pose fields, excluding controller ownership and credentials.
The component reuses `go/simulator/go2/go2_sim/viewer.js`, including its Z-up
geometry, world-pose interpolation, camera follow and GPU resource disposal.
It loads geometry once per scene. The gateway captures observer poses locally
at up to 30 Hz with one request active and retains at most 90 poses or 1 MiB.
The widget receives ordered batches through the host tool bridge at up to 4 Hz,
with one request in flight and background priority. It requests only frames
after its last sequence and replays capture timestamps, including short motions
between host polls. Interpolation is allowed only when both capture and physics
time gaps are at most 100 ms. Longer gaps hold the preceding pose and then show
the next actual pose. A slow rendering frame cannot skip over a captured pose.
Playback waits for late batches rather than extrapolating or advancing past
recorded history. Its queue is bounded to 180 poses; lost history is reported.
Background requests yield to user actions without an extra fixed 75 ms delay.
Hidden or offscreen views suspend rendering and call `simulator_scene_pause` to
stop observer capture. A 5-second idle lease, expiry and close also cancel it.

Camera preview reads use the same background queue, are canceled on Stop or
navigation, and include the last received sequence. The gateway renews the lease
without retransmitting JPEG bytes when that sequence has not changed. The
decoder remains capped at 5 fps; tool reads remain capped at 4 Hz.

`simulator_viewer` with `embedded: true` opens a 30-minute observer session on a
random loopback port. Its URL, bearer token, resource URI and expiry are returned in UI-only
`_meta.scene_session`, outside model-visible content. The session serves only
`GET /api/scene` and `GET /api/scene/state`; it exposes no simulator control
endpoint. The component calls the app-only `simulator_scene_read` tool through
`app.callServerTool`, passing `session_id` and `part: "geometry" | "state"`.
The gateway returns observer data only in `_meta.scene_data`, outside the model's
context. State calls with `history: true` and `after_sequence` return timestamped
`frames`, a `sequence` cursor and a `dropped` flag. Older state calls and scene
resource reads keep their single-snapshot contract, but ChatGPT's
widget scope rejects them, so the widget uses tools instead.
New widgets request `encoding: "gzip"` for geometry. Large scenes arrive as
base64 gzip in `_meta.scene_data_gzip`; decoding retains the 32 MiB expanded limit
and cancellation. This reduces the tested G1 scene from 11.4 MB to a 4.8 MB tool
result without changing its geometry. Initial geometry calls allow 45 seconds
for host transfer; pose reads retain their 15-second limit. Older widgets keep
the JSON contract.

View simulation requests the host's fullscreen display mode immediately and
opens a focused viewer with the canvas and camera controls fitted to the viewport.
Back to simulators closes the session and restores management. If fullscreen is
declined or unsupported, the same focused viewer opens inline.
Browser requests to localhost are unnecessary. The gateway rechecks local
authorization, ownership and expiry on each scene read. Hiding, replacing or
leaving the view releases it with the app-only `simulator_scene_close` tool.
Expiry closes it even if the UI disappears without cleanup. Older clients can
still use the bearer-protected HTTP observer route at up to 20 Hz.

Only authorized local gateways can create sessions. GPU support and tool
bridge support vary by host, so browser fallback stays available. A local browser check does not
establish support in every ChatGPT host. Rebuild and restart the gateway, then
refresh the plugin's tools to load the new schema, bundle and resource CSP.
