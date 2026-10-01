# Wendy for ChatGPT

## Add Wendy to your personal marketplace

With the Wendy CLI installed, run:

```sh
wendy mcp setup chatgpt
```

1. **Quit and reopen ChatGPT Desktop.**
2. **Open Plugins and select Personal.**
3. **Open Wendy and select the plus button to install it.**
4. **Start a new conversation with Wendy enabled.**

Ask: "Help me get started with my first device or a simulator."
The package includes the local MCP gateway, device workspace, and bundled skills.
Local access needs no Cloud account. Add `--simulators` for simulator access or
`--device <selector>` for device inspection. See the
[user setup guide](../../docs/guides/chatgpt.mdx) for project permissions and
troubleshooting.

## Start without an existing installation

For a user who has not used Wendy, start with the skills-only
[Get started with Wendy](../wendy-get-started/README.md) package. It has no CLI
or MCP dependency and guides them from their goal to the right connection.
Wendy helps users build and run apps on robots and small computers, or try a
simulator before their hardware arrives.

For local use, the assistant checks the user's computer and installs the CLI
only when needed. On macOS/Linux:

```sh
curl -fsSL https://install.wendy.dev/cli.sh | bash
wendy mcp setup chatgpt
```

On Windows, use `winget install WendyLabs.Wendy --source winget`, then the same
setup command. This requires a CLI build containing `mcp setup chatgpt`; check
its help if an older published CLI is installed. A web-only assistant gives
the user the command instead of installing into its own remote environment.

Setup writes a private policy at `~/.wendy/chatgpt/gateway.json`, extracts the
plugin and its skills into `~/.codex/plugins/sources/wendy-robots`, and adds it
to `~/.agents/plugins/marketplace.json`. It preserves unrelated marketplace
entries and existing policy grants. The generated MCP command uses absolute
binary and policy paths, so no shell environment variable is needed. Follow
the printed instructions to restart ChatGPT desktop and install the local
entry. The command makes the plugin available; it does not automatically
install it into a live chat or edit the host's plugin cache.

Default setup can start with no targets. It permits the device gallery and
preferences, with no Cloud source, simulator management, host operation,
project access, or broader developer server enabled. Add permissions for the
user's chosen task:

```sh
wendy mcp setup chatgpt --device workshop.local:50052
wendy mcp setup chatgpt --simulators
wendy mcp setup chatgpt --simulators --workspace /path/to/project
wendy mcp setup chatgpt --host-operations
# Optional broader developer tools, as a separate MCP process:
wendy mcp setup chatgpt --developer-tools
```

Devices added by setup are read-only, with installed-app inventory visible.
Simulator permissions do not grant physical-device cameras or app control.
`--workspace` authorizes project reading/writing and deployment to the policy's
granted devices and permitted simulators. Re-run it with the same project when
adding deployment targets. Existing policy permissions survive ordinary setup
reruns. Disable the separate developer MCP with `--developer-tools=false`.
It has broader access than the gateway and does not inherit its grants or
selected device. Restart the MCP connection after policy or tool changes.

## Local and hosted access together

A local gateway can use both LAN/USB targets and Cloud selectors in the same
policy. Repeat `--device` for each target. Only Cloud targets need their saved
Cloud login; local access does not require `wendy auth login`. Configured
`cloud_sources` also remain supported for authorized Cloud inventory discovery.

For browser/mobile access, deploy the authenticated HTTP gateway described
below and register it in ChatGPT. Hosted users connect that available plugin
through its account flow without installing a local CLI. The hosted operator
can package a registered connection with:

```sh
wendy mcp setup chatgpt --connection hosted --app-id '<registered-MCP-app-id>'
# Offer both entries in the personal marketplace:
wendy mcp setup chatgpt --connection both --app-id '<registered-MCP-app-id>'
```

The hosted package is `wendy-robots-cloud`. It references the supplied app via
`.app.json` and includes the same onboarding skill, with no local `mcp.json`.
This separates it from the Desktop-only package. Use an app ID belonging to
the intended account/workspace; setup checks its format, not account ownership
or remote availability. A URL, tunnel ID, or invented app ID is not a substitute
for registration. These are personal/workspace packaging commands, not public
submission or hosted deployment. Public submission still requires the stable
HTTPS endpoint and review, and the existing Cloud credential-delegation work
described below remains a separate production dependency.

This desktop workspace adds Wendy to ChatGPT's global navigation and conversation
extensions. It opens on a device gallery with static renders of existing Wendy device assets, then lets
you inspect devices, preview cameras, share selected frames, read telemetry, and
operate approved apps through Wendy Cloud or a direct connection.

The UI follows ChatGPT's theme with Wendy typography and logos. Controls are
neutral; green and red identify online devices and running or stopped apps.
App running state and verified readiness remain separate.

The gateway is part of the Go CLI. It uses an explicit robot ID on every operation
and opens a separate authenticated device connection per call. The existing
general-purpose `wendy mcp serve` connection and its selected device are separate.

## Run the private pilot

The plugin packages a single `wendy` skill with all end-user workflows, including
`wendy-onboarding`. Its setup entry guides users through installing and verifying
their first physical device, or creating a local simulator when hardware is not
available. The existing device workspace prefers fullscreen; setup also works
without the UI.

Edit skills in `plugins/wendy-agentic-coding/skills`, then run
`python3 scripts/sync-agent-skills.py` from the repository root. The script updates
this package and the matching bundle embedded in the CLI. Both gateway transports
serve `skills/list`, `skills/get`, and the listed `skill://wendy/wendy/` resources
with SHA-256 digests. The HTTP endpoint retains its authentication requirements.

OpenAI imports these files as a submission-time snapshot. After updating the
server, run Scan Tools again and review the imported skills before submitting a
new version. See [MCP skill import](https://developers.openai.com/plugins/build/mcp-server#import-skills-from-the-mcp-server)
and [plugin onboarding](https://github.com/openai/mcp-extensions/blob/main/docs/spec.md#plugin-onboarding).

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

## Mention a device

In ChatGPT Desktop's composer, type `@`, choose Wendy, and search for a device
by name or ID. Select the device and ask, for example, "Show the apps on this
device." The mention supplies its name and stable `robot_id` to the conversation.
Devices with the same name have distinct resource links and IDs in their
descriptions. An empty search lists up to 100 devices; type more of the name or
ID to narrow a larger fleet.

Search includes configured devices, authorized Cloud enrollments including
offline devices, and permitted running local simulators. Selecting a mention
checks access again. Search and selection do not connect to the device or
activate its cameras.

After updating the gateway, restart its MCP process and refresh Wendy's tools
in ChatGPT. The host must support
[composer at-mentions](https://github.com/openai/mcp-extensions/blob/main/docs/spec.md#composer-at-mentions).
The extension currently specifies desktop support only.

Check the protocol and device resource reads without connecting to a device:

```sh
python3 scripts/verify-chatgpt-gateway.py --wendy /tmp/wendy-chatgpt \
  --config /tmp/robot-gateway.json --mentions-only
```

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
      "allow_camera": false,
      "allow_all_apps": false
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
start/stop is disabled by default for discovered devices. Set `allow_all_apps`
to true on a Cloud source to permit control of all installed apps on its
discovered devices, including apps installed later. Camera access requires
the source's `allow_camera` option and the caller's camera scope.
Permission flags do not establish hardware capabilities, and Cloud presence
does not verify a working Agent connection.

`allow_all_apps` also applies to individual `robots` entries and defaults to
false wherever it is omitted. When enabled, it lists all installed apps and
permits start/stop without adding their names to `apps`. Each operation first
checks the device's current inventory. A missing app or failed inventory check
prevents the operation. App tool exports still require explicit robot policies
and reviewed descriptors.

Explicit robot entries retain their own names and permissions when they also
appear in Cloud inventory. Their policies take precedence over Cloud source
options, including `allow_all_apps` and `allow_camera`. A caller must have the
explicit robot ID in its `robots` grant; a Cloud source grant cannot bypass it.
For other discovered devices, the caller needs that source in its
`cloud_sources` grant. App control always requires `apps:control` in the
subject's grant and, over OAuth HTTP, in the access token. Camera access requires
`cameras:capture` in both. These options do not expand token scopes or grant
deployment, shell, or app-tool export access.

To grant camera access to just one discovered device, add an explicit robot
entry using its existing catalog ID and device selector, set `allow_camera` to
true, and add that ID to the subject's `robots` grant. Set `list_all_apps` to
true to retain read-only inventory of its installed apps. With `allow_all_apps`
left false, only names in `apps` are eligible for start/stop, and the caller
still needs `apps:control`. The example policy keeps this limited access.

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

Add `--empty` to preview the first-run getting-started screen with no devices
or simulator permissions. Fixture buttons only update the fixture host's
displayed conversation context; they do not send a real ChatGPT message.

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

For per-user Cloud credential delegation, use the separate
[Cloud account-linking mode](cloud-account-linking.md). It replaces operator
credentials with encrypted sessions for each linked account, serves the gateway
OAuth flow, and discovers only that account's Cloud inventory. Production service
configuration, client/callback registration, and a real ChatGPT connection check
remain required before deployment. The packaged local MCP plugin is unchanged.

For hosted distribution, change the installed `mcp.json` server to
`{"type":"streamable-http","url":"https://<your-host>/mcp"}`. Register the
actual MCP endpoint through OpenAI's plugin builder and complete review before
public distribution. The plugin package deliberately contains no invented app ID.

## Simulators and app web UIs

### Develop and deploy from ChatGPT

Use [gateway.simulator.example.json](gateway.simulator.example.json) for a local
simulator development session without physical hardware. Create an empty project
directory on the gateway laptop, replace the example workspace's `path` with its
absolute path, and start the gateway with that policy. The existing local plugin
or private tunnel connection can use it. Restart the MCP process and refresh the
plugin's tool catalog after updating the CLI or policy.

This policy permits ChatGPT to create and operate local simulators, read and edit
that project directory, and deploy it to running local simulators. It does not
enable OS installation. The build runtime must be installed on the gateway host.
The workspace remains on that host; a hosted HTTP gateway cannot edit the user's
laptop files.

Try "Create a generic Wendy simulator, write a small Python web app in the
Simulator app workspace, deploy it there, and check its startup logs."
For robot work, choose Go2 or G1 and have ChatGPT inspect the simulator and camera
output while iterating on the app.

The tool sequence is:

1. `simulator_list`, then `simulator_create` if needed and `simulator_start`.
   For Go2/G1, use `simulator_viewer` to check the live scene.
2. `list_robots` to resolve the running simulator's `robot_id`, usually
   `sim-<name>`. Explicitly configured VMs retain their configured IDs.
3. `list_workspaces` to choose an approved project and see its permitted
   `robot_ids` and file permissions. `list_workspace_files` lists a directory;
   `read_workspace_file` returns text and its SHA-256 revision.
4. `write_workspace_file` creates or replaces a UTF-8 file up to 256 KiB. Use
   `expected_sha256: "missing"` for a new file, or the revision returned by a
   read for an edit. A stale revision fails without changing the file. Parent
   directories are created as needed. Author the app source and its Wendy
   configuration/build files before validating.
5. `validate_device_project` with the workspace and exact `robot_id`, then
   `start_device_deployment` with those IDs and a fresh stable `request_id`.
   Poll `get_deployment_job`. Reuse that request ID after an uncertain response;
   use a new ID for a new deployment after editing. Cancellation can leave an
   already started app running.
6. `inspect_robot` and `read_device_logs` to check the app and actual output.
   For an HTTP app, `open_robot_app` returns its declared web UI. Deployment
   completion alone does not establish readiness.

Project file tools are local stdio only, require a workspace grant, and separate
`projects:read` from `projects:write`. Paths stay inside the approved root;
absolute paths, parent traversal, escaping symlinks, and `.git` paths are rejected.
Deployments require `apps:deploy` independently of file editing. Setting
`allow_simulators` on a workspace opts that project into running local simulators
that the caller may manage, including ones created later. It does not permit
Cloud deployment or override an explicitly configured VM's grants and workspace
`robots` list. Physical targets use that list and their separate robot grants.

For a private local gateway, set `"allow_simulators": true` and add
`"simulators:manage"` to its local subject's scopes. The Simulators view can
create, start, stop and inspect laptop VMs using the existing Wendy simulator
backend. Go2 and G1 use the real MuJoCo runtime and its verified viewer. The
viewer opens in the browser, with an optional embedded view. This does not grant
disk installation or other host operations, and these tools are unavailable
through the shared HTTP gateway. Creating a simulator leaves it stopped; the
UI then explicitly starts it. Initial setup can take several minutes.

Camera and installed-app control on discovered simulators require the separate
top-level `"allow_simulator_device_access": true` option. It defaults to false.
When enabled, it covers running local simulators created later and their future
installed apps. The local simulator policy above must also permit the session,
and the caller still needs `cameras:capture` for cameras or `apps:control` for
start/stop. Explicit simulator entries in `robots` keep their own policies.
This option does not expose local simulators through the shared HTTP gateway or
grant arbitrary host files, shell commands, OS operations, or deployments.

If a downloaded VM image has an older agent, refresh simulator status and use
**Finish setup**. This updates only that running simulator's agent from the
official stable channel, verifies the uploaded binary and robot capability,
then provisions the simulation. Compatible agents are left unchanged.

The local `open_robot_app` tool opens a running app's declared HTTP entitlement.
It routes through the authenticated device connection, binds to loopback, and
expires after 30 minutes. It does not accept arbitrary URLs or ports or start
the app. The returned URL is usable only on the gateway laptop. The Apps view
shows Open app when the app advertises a port and the caller has `apps:tools`.

## Events and deployment

The gateway implements [MCP Events](https://developers.openai.com/plugins/build/mcp-events)
over MCP 2.0 (`2026-07-28`) while retaining its MCP1 tools and bounded task waits.
`server/discover` advertises events to accounts with `events:read`.
`events/list` exposes `wendy.data.notification`, filtered by required `robot_id`
and optional exact `campaign` and `event` names. `events/subscribe` verifies the
host's signed HTTPS callback before activating delivery; `events/unsubscribe`
stops it. A successful host subscription lets ChatGPT receive the notification
and follow the user's chosen response instructions. It does not deploy a model
or activate a camera.

This event contains actual immediate Wendy Data notification intents emitted
by `notify: {on: detection}` or `notify: {on: event, event: <name>}`. It includes
the original notification ID, occurrence time, campaign, event, source, model,
revision and count. Images and episode recordings are not sent in the webhook.
Cloud ingestion's `episode_committed` notification intent is not included;
that consumer is separate from the device's immediate notification flow.

Update the **device Agent as well as the gateway**. The new Agent retains the
latest 512 notifications in a private journal. Older Agents are explicitly
rejected when they cannot identify this notification stream. Read retained
notifications with `read_device_notifications`; `replay: true` includes history.
A gap means some requested history has expired. Raw app events and the older
`list_device_events` / `wait_for_device_event` tools remain separate.

Subscriptions have a maximum/default lifetime of 24 hours, with shorter
requested lifetimes honored. Delivery uses a durable outbox, stable event IDs,
bounded retries, signed callback verification, and access checks before reads
and sends. Callback connections validate public addresses at dial time and do
not follow redirects. Gateway state is stored under `state_directory` or the
user config directory, namespaced by device routing. Keep that directory across
restarts. Subscription files contain webhook secrets and, for HTTP users, the
credential needed to recheck authorization; files are mode 0600. One process
owns delivery for each routing namespace.

To set up YOLO, grant the intended account `triggers:write`, `cameras:capture`,
and `events:read`, with camera access enabled on the target. These scopes also
apply to authorized Cloud discoveries; no arbitrary address is accepted.
Call `deploy_yolo_detector` with an explicit device and public Hugging Face
repository, for example:

```json
{
  "robot_id": "<id from list_robots>",
  "name": "people",
  "model_ref": "<owner>/<repository>",
  "model_file": "<YOLO detection export>.onnx",
  "labels": ["person"],
  "threshold": 0.5,
  "rate": 1
}
```

`model_file` is optional when the repository has exactly one ONNX file.
`revision` defaults to `main` and resolves to an immutable commit before
installation. The CPU runtime supports YOLOv8/YOLO11 float32 ONNX detection
exports with a static batch-one input and embedded class names. It rejects
PyTorch checkpoints, repository Python, external tensor files, and unsupported
segmentation or postprocessed outputs. Choose `source_id` when more than one
healthy camera exists. The tool records one-second detection episodes locally
with manual upload. The plan requests 128 MiB retention, but the current Agent
enforces only its device-wide storage quota; the tool reports that distinction. `inspect_yolo_detector` reports inference and notification errors;
`stop_yolo_detector` disables inference without deleting recordings.

After inspection confirms the detector is running, ask ChatGPT to monitor
`wendy.data.notification` using the returned `robot_id` and `campaign` filters.
Rescan the plugin's tools and events after updating the gateway. Verify the
host calls `server/discover` and `events/list`, completes signed callback
verification, accepts a matching notification, and responds in the subscribed
chat. Local protocol tests do not establish that ChatGPT delivery has occurred.
The private tunnel must forward these custom methods; tunnel-client 0.0.14
forwards stdio methods but does not offer a `stateless` configuration flag for
its main channel. Authenticated HTTP can serve the MCP2 endpoint directly.

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
