# Wendy robots for ChatGPT

This desktop workspace adds Wendy to ChatGPT's global navigation and conversation
extensions. It opens on a device gallery with existing Wendy 3D assets, then lets
you inspect devices, preview cameras, share selected frames, read telemetry, and
operate approved apps through Wendy Cloud or a direct connection.

The UI follows ChatGPT's theme with Wendy typography and logos. Controls are
neutral; green and red identify online devices and running or stopped apps.
App running state and verified readiness remain separate.

The gateway is part of the Go CLI. It uses an explicit robot ID on every operation
and opens a separate authenticated device connection per call. The existing
general-purpose `wendy mcp serve` connection and its selected device are separate.

## Run the private pilot

Build this checkout. An older installed CLI does not have the gateway command.

```sh
npm --prefix web-client/mcp-app ci
npm --prefix web-client/mcp-app run build
go build -o /tmp/wendy-chatgpt ./go/cmd/wendy
cp plugins/wendy-chatgpt/gateway.example.json /tmp/robot-gateway.json
chmod 600 /tmp/robot-gateway.json
```

Edit the copied policy's `device`, robot name and allowed apps. Use an explicit
LAN address, `vm:<name>`, or the saved Cloud selector for your robot:

```text
cloud://<cloud-host>:443/org/<organization-id>/asset/<asset-id>
cloud://<cloud-host>:443/tenant/<tenant-uuid>/asset/<asset-uuid>
```

The gateway uses Wendy credentials already present on its host. Log in with
`wendy auth login` if needed. Keep personal policies and credentials out of the
plugin package. Unset `WENDY_AGENT_SOCKET`; it would override device routing.

Configure a local MCP connection or ChatGPT's private Secure MCP Tunnel with:

```json
{
  "command": "/tmp/wendy-chatgpt",
  "args": ["mcp", "gateway", "--config", "/tmp/robot-gateway.json"]
}
```

The portable `plugin.json` and `mcp.json` in this directory are for a local plugin
install. That manifest expects the new CLI as `wendy` on PATH and
`WENDY_ROBOT_GATEWAY_CONFIG` in the MCP process environment. Alternatively edit
the installed copy's command and args to use the absolute paths above. Restart
the MCP connection after changing the binary, policy, or exported tools. Then
refresh Wendy in ChatGPT Plugins and open its workspace in a new conversation.
ChatGPT caches trusted tools and UI resources. New tools cannot be called from a
workspace that still has an older tool catalog. The current resource is
`ui://wendy/device-workspace-v3.html`.

ChatGPT account/workspace access to plugin authoring and Secure MCP Tunnel must
be available. This repository does not create a registered OpenAI app or publish
to the directory. Follow the current [connection instructions](https://developers.openai.com/plugins/deploy/connect-chatgpt).

For the private tunnel route, create a tunnel in
[Platform tunnel settings](https://platform.openai.com/settings/organization/tunnels)
and associate it with the intended ChatGPT workspace. Install OpenAI's
`tunnel-client` using the download link there. Configure its runtime key securely
as `CONTROL_PLANE_API_KEY`, then run:

```sh
tunnel-client init --sample sample_mcp_stdio_local --profile wendy-robots \
  --tunnel-id '<your-tunnel-id>' \
  --mcp-command '/tmp/wendy-chatgpt mcp gateway --config /tmp/robot-gateway.json'
tunnel-client doctor --profile wendy-robots --explain
tunnel-client run --profile wendy-robots
```

In ChatGPT, enable Developer mode under Settings, Security and login. At
[Plugins](https://chatgpt.com/plugins), select the plus button, choose Tunnel
under Connection, and select that tunnel. Keep the tunnel client running.
Creation needs Platform Tunnels Read and Manage permissions; using it needs
Read and Use. See [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels).

Stdio uses one configured `local_subject` for the entire process. Limit this pilot
tunnel to its intended owner. Use authenticated HTTP with per-user grants for
shared access; workspace tunnel membership alone does not distinguish callers
inside a stdio MCP process.

Try these prompts:

- "Open Wendy and show my robots."
- "Capture a frame from the front camera and describe it."
- "Check the companion app, then set its message to Hello from ChatGPT."

The last prompt requires the companion tool exports described below.

## Discover the rest of your Cloud devices

Explicit `robots` entries set aliases and app permissions. To discover devices
as they join your account, add a Cloud source and grant its ID to the caller:

```json
{
  "cloud_sources": [
    {
      "id": "my-cloud",
      "endpoint": "<cloud-host>:443",
      "organization_id": 2,
      "allow_camera": false
    }
  ]
}
```

Merge this field into your policy and add `"cloud_sources": ["my-cloud"]` to the
intended subject's existing grant. Replace the endpoint and organization with
your own saved Cloud selector's values. For UUID-based Cloud accounts, use
`"tenant_uuid": "<tenant-uuid>"` instead of `organization_id`. The gateway uses
that exact saved login; changing the CLI's default account does not change it.
Restart the gateway after editing the policy.

`list_robots` refreshes Cloud inventory on every call. It includes online Cloud
devices and all explicitly configured robots by default. Use `include_offline:
true` for the full inventory, `query` to find a name or ID, and `offset` with
`limit` up to 200 to follow `next_offset`. The panel's refresh button updates
inventory, and its offline checkbox loads all pages. A discovery error returns
warnings and `discovery_complete: false`; it is not an empty account.

Discovered devices can be inspected, including their installed app states. App
start/stop and exported tools still require an explicit robot policy. Camera
access requires the source's `allow_camera` option and the caller's camera
scope. Existing configured robots retain their own permissions and names.
Permission flags do not establish hardware capabilities, and Cloud presence
does not verify a working Agent connection.

## Run and export an app

The included companion app has no motion or audio effects. It stores a message
and reports its own readiness and uptime.

```sh
/tmp/wendy-chatgpt run --yes --detach --build-type docker \
  --device '<explicit-device-selector>' --prefix Examples/RobotCompanion
/tmp/wendy-chatgpt mcp export-robot-tool --config /tmp/robot-gateway.json \
  --robot workshop --app com.wendylabs.robot-companion \
  --tool get_status --name companion_status
/tmp/wendy-chatgpt mcp export-robot-tool --config /tmp/robot-gateway.json \
  --robot workshop --app com.wendylabs.robot-companion \
  --tool set_message --name companion_message
```

Review each returned object and add it to that robot's `exports` array. The
gateway requires explicit read-only, destructive, idempotent, and open-world
annotations. It freezes the full descriptor, validates arguments, and compares
the live app descriptor before every invocation. A changed contract requires
operator review and a gateway restart. It does not expose an arbitrary tool-name
dispatcher or discover new robot controls into a conversation automatically.

App tool calls use `{ "robot_id": "workshop", "arguments": { ... } }`.
App MCP traffic travels through the authenticated Wendy Agent `StreamMCP` RPC.
The companion listens only on device loopback. See the [app README](../../Examples/RobotCompanion/README.md).

## Verify the connection

```sh
python3 scripts/verify-chatgpt-gateway.py \
  --wendy /tmp/wendy-chatgpt --config /tmp/robot-gateway.json --robot workshop
```

This checks MCP initialization, tool discovery, both panel entrypoints, the UI
resource, the robot catalog, and a fresh device inspection. Optional flags make
the effects explicit:

- `--camera 0 --image-out /tmp/robot-frame.jpg` captures one frame to a new private file.
- `--cycle-app com.wendylabs.robot-companion` stops then starts the test app and verifies both states.
- `--call '{"name":"companion_status","arguments":{}}'` invokes an approved export.
- `--preview-camera 0` verifies advancing live frames and closes the session.
- `--open-app <app-name>` checks the running app's advertised web UI over a private loopback proxy.

`RUNNING` alone is not application readiness. Use the app's own status tool and
inspect its output. Camera capture needs `gst-launch-1.0` on the gateway host
with the plugins used by Wendy's camera snapshot pipeline. It closes the capture
stream after receiving the image. Live preview uses a separate continuous stream,
with bounded JPEG frames returned to the UI only. Preview stops on tab/device
changes, when the page is hidden, after 15 seconds without a reader, or after
10 minutes. Sharing a frame explicitly attaches a still image to ChatGPT.
A virtual camera listed by the agent may still
have no producing application.

Run the panel's local host fixture with `node scripts/chatgpt-panel-preview.mjs`
and open `http://127.0.0.1:8790`. Its default fixture has no real camera. Set
`WENDY_GATEWAY_URL` and `WENDY_GATEWAY_TOKEN` on that process to use a local HTTP
gateway; the token stays on the host and is never sent into the panel iframe.

Regression checks:

```sh
go test ./go/internal/cli/mcp ./go/internal/cli/commands
go test -race ./go/internal/cli/mcp -run 'RobotGateway|ConnectAppMCP|MCPProxy'
npm --prefix web-client/mcp-app run check
go test ./go/internal/agent/data ./go/internal/agent/services
```

## Host the gateway

For a multi-account service, add an `http` object using
[http.example.json](http.example.json), replace the sample domains, and provision
the resource and introspection client in your authorization server. Map its exact
`sub` values to grants. Remove `local_subject` from an HTTP-only policy.

```sh
/tmp/wendy-chatgpt mcp gateway --config /path/to/gateway.json --listen 127.0.0.1:8788
```

Place it behind TLS at the configured `resource_url`. `/healthz` is a process
check, not a robot readiness check. The MCP endpoint is `/mcp`. Protected resource
metadata is at `/.well-known/oauth-protected-resource/mcp`.

The authorization server must support OAuth authorization code + PKCE S256,
resource-bound access tokens and the ChatGPT client registration/callback flow.
The resource server introspects every request and checks `active`, `sub`, exact
`iss`, matching `aud`, `exp`, `nbf`, bearer token type and scopes. Its introspection
endpoint must share the issuer origin and accept HTTP Basic client authentication.
Sender-constrained tokens are rejected because this gateway has no DPoP verifier.
See [OpenAI's authentication guide](https://developers.openai.com/plugins/build/auth).

For a local HTTP test only, replace `oauth` with
`"development_tokens": [{"subject":"local-owner","token_env":"WENDY_GATEWAY_TOKEN"}]`
and use `http://127.0.0.1:8788/mcp`. Supply a random token of at least 32 bytes in
that environment variable. Development tokens are for test clients; ChatGPT's
production OAuth connection does not accept a custom API key.

The gateway's operator credentials establish Cloud connectivity. HTTP OAuth
identity authorizes a configured subset of those targets. This pilot does not
implement automatic delegation from a Wendy Cloud user's login to Cloud relay
credentials. Provisioning that identity mapping and credential lifecycle is a
required step before public multi-tenant deployment. No introspection endpoint
or production client registration is assumed to exist in Wendy Cloud today.

For hosted distribution, change the installed `mcp.json` server to
`{"type":"streamable-http","url":"https://<your-host>/mcp"}`. Register the
actual MCP endpoint through OpenAI's plugin builder and complete review before
public distribution. The plugin package deliberately contains no invented app ID.

## Simulators and app web UIs

For a private local gateway, set `"allow_simulators": true` and add
`"simulators:manage"` to its local subject's scopes. The Simulators view can
create, start, stop and inspect laptop VMs using the existing Wendy simulator
backend. Go2 and G1 use the real MuJoCo runtime and its verified viewer. The
viewer opens in the browser, with an optional embedded view. This does not grant
disk installation or other host operations, and these tools are unavailable
through the shared HTTP gateway. Creating a simulator leaves it stopped; the
UI then explicitly starts it. Initial setup can take several minutes.

The local `open_robot_app` tool opens a running app's declared HTTP entitlement.
It routes through the authenticated device connection, binds to loopback, and
expires after 30 minutes. It does not accept arbitrary URLs or ports or start
the app. The returned URL is usable only on the gateway laptop. The Apps view
shows Open app when the app advertises a port and the caller has `apps:tools`.

## Events and deployment

Configured triggers and `events:read` allow event history and a bounded
`wait_for_device_event`. MCP task augmentation lets a supporting host receive
the later result without keeping the initiating HTTP request open. The host
must start the wait and deliver its result to ChatGPT. An ordinary MCP
notification does not guarantee an unattended model turn. Devices need the
updated event journal RPC and an app that emits the configured event.

Project and deployment tools operate on approved local workspaces and explicit
targets. Fleet planning does not deploy. Deployment jobs expose progress and
cancellation; process success alone does not prove application readiness.
OS installation remains subject to the existing drive fingerprint and erase
authorization flow. These capabilities need separate policy grants; see the
[desktop plan](../../specs/2026-09-30-openai-mcp-desktop-plan.md).

Two-way audio remains separate. ChatGPT voice chat does not itself route its
microphone or speaker to a robot.

House controls can be reviewed app-tool exports on the same gateway. Add each
home integration's identity, permissions and action semantics before exporting
it. No home controls or movement tools are enabled by this package.

The panel uses the [MCP Apps UI bridge](https://developers.openai.com/plugins/build/chatgpt-ui)
and the documented [global and thread extensions](https://developers.openai.com/plugins/build/extensions).
