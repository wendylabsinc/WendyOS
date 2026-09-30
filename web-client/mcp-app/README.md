# Wendy desktop workspace

React/TypeScript MCP App embedded in the Go gateway. Build from this directory:

```sh
npm ci
npm run check
npm run build
```

`build.mjs` bundles the official MCP Apps bridge, OpenAI Extensions, React,
Three.js, styles, fonts and logos into
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

Device models come from Wendy's existing marketing-website repository:

```sh
node import-models.mjs /path/to/marketing-website
```

The import decodes Draco at build time and simplifies display meshes so the app
does not need remote decoders or workers. `desktop_assets/manifest.json` records
source paths and hashes. `GO2-LICENSE.md` preserves model attribution. Geist's
license is in `src/assets/Geist-LICENSE.txt`; logos come from Wendy's brand assets.
One renderer draws visible cards. Unknown hardware uses a generic illustration.
These display models are separate from the simulator's live MuJoCo scene.

See [gateway setup](../../plugins/wendy-chatgpt/README.md) for scopes, local
simulators, app web views and ChatGPT connection refresh instructions.
