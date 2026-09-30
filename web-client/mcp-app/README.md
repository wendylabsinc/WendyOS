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
simulator's live MuJoCo scene. The old `get_device_model` tool remains available
for compatibility, but this UI downloads no GLB files and creates no WebGL canvas.
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
