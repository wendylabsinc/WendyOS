# Wendy marketplace submission

The public package uses the production CLI relay at `https://mcp.wendy.dev`.
ChatGPT sends calls through Wendy Cloud to a paired `wendy cloud mcp` process
on the operator's computer. Calls stop when that process exits. The publisher
is Wendy Labs Inc. The package allows all countries and declares no commerce.

## Connection and OAuth

The public endpoint uses Streamable HTTP. Its protected-resource metadata
advertises only `mcp:relay` and issuer `https://auth.wendy.dev/relay`.
Issuer discovery is available at
`https://auth.wendy.dev/.well-known/oauth-authorization-server/relay`.
It advertises authorization at `https://auth.wendy.dev/authorize`, token
exchange at `https://auth.wendy.dev/relay/token`, PKCE S256, and client metadata
discovery. Version 0.5.5 declares the documented CIMD client form,
`client: {"mode": "cimd"}`, with no client ID or credentials, per OpenAI's
[OAuth configuration reference](https://developers.openai.com/plugins/build/plugins#oauth-configuration-reference).
Wendy validates ChatGPT's client metadata document and requires PKCE. The
package requests `baseScopes: ["mcp:relay"]`, `defaultScopes: []`, and
`oidcEnabled: false`. Successful portal save and token exchange remain
unverified.

2026-10-18: 0.5.5 returns `auth.client` to `{"mode": "cimd"}`; the portal
rejected the 0.5.4 explicit provided client and the omitted-client candidate.

Read-only checks on 2026-10-15 confirmed the relay metadata and that the
authorization endpoint accepts ChatGPT's client metadata URL before sign-in.
Those checks did not establish a successful OAuth grant or device operation.
Verify the current metadata and the complete connection before submission.

A live smoke check on 2026-10-15 used the rebuilt branch CLI with
`--cloud-grpc api.wendy.dev:443 --auto-accept` and an existing ChatGPT OAuth
connection. The CLI accepted the exact ChatGPT client identity without a
pairing prompt. The first `wendy_status` returned `relay_not_paired`; a retry
returned `auth: logged_in`. `cloud_discover(online_only=false)` returned the
account's enrolled devices. The relay accepted the CLI's tool catalog. The
session was then stopped. This check preceded the `--allow-read` split and does
not verify the current confirmation behavior. It did not exercise a fresh
sign-in, reviewer account, camera, app control, or video recording.

## Discovery candidate for 0.5.5

On 2026-10-17, selecting Connect in the existing Wendy draft reproduced the
missing-client-ID error. The drawer still displayed the populated provided
client configuration and offered no editable client-ID field.

The separate candidate `build/plugins/wendyos-0.5.5-oauth-discovery.zip` omits
the optional `auth.client` object. OpenAI's
[OAuth configuration reference](https://developers.openai.com/plugins/build/plugins#oauth-configuration-reference)
allows omission and leaves optional settings available for discovery. This
tests a package form not covered by the earlier explicit CIMD and provided-ID
attempts. Discovery already advertises CIMD and token authentication `none`;
the candidate adds no endpoint overrides.

The archive changes only the package version, release notes, and omission of
`auth.client`. It preserves the `wendy-relay` entry, endpoint, OAuth requirement,
sole `mcp:relay` scope, empty default scopes, disabled OIDC, listing, review
cases, walkthrough URL, skill, and icon. The canonical source remains at 0.5.4.
The packager now accepts an absent client while rejecting explicit null,
malformed clients, unsupported registrations, and packaged client secrets.
All 27 packaging tests passed. ZIP integrity and exact retained contents were
checked. The archive has four files and is 54,115 bytes, with SHA-256
`cd6df8dd93f70759ac7a6ec63aace9518cbf66588557ba350b2ab2a556ee12e4`.

The user subsequently supplied a screenshot showing the omitted-client
configuration in the Wendy connection drawer and the same missing-client-ID
error. The screenshot confirms the changed settings are displayed, while the
portal still asks for predefined-client credentials. The package version and
internal saved state are not visible, and no fresh network or breakpoint trace
was captured for this attempt. The candidate did not remedy the reported error;
OAuth completion and tool scans remain unverified.

The next step is OpenAI repair or a supported reset of the existing draft's
OAuth state. The prepared bug report includes this failed workaround and keeps
the earlier runtime evidence separate. Further package-only changes have no
established fix for this connection. After repair, verify the actual client
identity, sole relay scope, completed OAuth, and an authenticated status response.
A pairing requirement is separate from OAuth success and does not verify a device.

## CLI setup and permissions

### Portal OAuth recheck on 2026-10-16

At 18:41 and 18:42 UTC, both the portal's Connect account action and the MCP
drawer Connect/Continue flow reproduced `invalid_scope`. The downloaded portal
draft `wendyos-0.5.2.zip` contains the correct CIMD configuration and only
`baseScopes: ["mcp:relay"]`; the drawer displays the same configuration. Current
protected-resource and authorization-server discovery also advertise only
`mcp:relay` and support CIMD with token authentication `none`.

Production authorization logs for both attempts still show:

```text
client_id=wendy-chatgpt
scope=robots:read cameras:capture apps:control preferences:write mcp:relay
resource=https://mcp.wendy.dev
error=invalid_scope
```

The saved connection therefore continues to use the old fixed client and scopes
despite the updated package settings. No user credentials or tokens were needed
for this diagnosis. The visible portal controls provide no registration reset.
Deleting the plugin would remove the draft and is not part of this repair.

A separate candidate, `/private/tmp/wendyos-0.5.3-connection-repair.zip`, changes
only the MCP entry name to `wendy-relay`, version, and release notes. It preserves
the plugin identity, endpoint, authentication settings, skill, icon, video, and
cases. Its four-file inventory and unchanged contents were checked. This is an
unverified attempt to create a fresh connection; OpenAI does not document whether
renaming an MCP entry resets its saved registration. The user subsequently opened
a fresh Wendy draft with the `wendy-relay` entry and reported the separate save
failure described below.

OpenAI's [submission authentication guidance](https://developers.openai.com/plugins/deploy/submission#configure-authentication-for-submission)
notes that existing client registration can lock credential changes. If the
fresh-entry test retains the same registration, request a reset of the associated
MCP OAuth registration through OpenAI support. Do not broaden Wendy's accepted
scopes or relax its authorization checks to accommodate this stale request.

### Explicit client configuration for 0.5.4

The next reported error was `MCP details save failed: OAuth client ID is required
when using pre-defined OAuth client credentials.` The visible fresh draft uses
plugin ID `plugin_asdk_app_6ad2746433a48191bbd68bc55abf0ad4`, MCP entry
`wendy-relay`, and endpoint `https://mcp.wendy.dev`. This error occurs while
saving the connection, before user authorization can complete.

Version 0.5.4 keeps that MCP entry and supplies the exact public ChatGPT client
identity instead of relying on the portal's CIMD selection. Its client object is:

```json
{
  "mode": "provided",
  "clientId": "https://chatgpt.com/oauth/client.json",
  "tokenEndpointAuthMethod": "none"
}
```

The package also clears default scopes and disables OIDC so it requests only
`mcp:relay`. This URL is ChatGPT's published identity, not a new registration or
a secret. No server authorization policy was broadened. Do not enter the legacy
`wendy-chatgpt` ID or a client secret.

An anonymous production authorization request with the URL client ID, sole
`mcp:relay` scope, resource `https://mcp.wendy.dev`, S256 PKCE, and the published
ChatGPT callback returned HTTP 200 with the sign-in form. It did not return
`invalid_scope`. No login, token, or authorization grant was created by this
probe. The Wendy authentication source resolves the allowlisted URL client to
the relay scope and token authentication `none`, with matching client identity,
callback, resource, and PKCE required at code redemption.

The portal's `provided` setting is absent from the OAuth request itself, so this
configuration should be compatible with the accepted URL client. That is an
inference pending a complete portal connection. OpenAI documents the
[explicit client and scope fields](https://developers.openai.com/plugins/build/plugins#oauth-configuration-reference)
and [ChatGPT's URL client identity](https://developers.openai.com/plugins/build/auth#client-registration).

The rebuilt archive is `/private/tmp/wendyos-0.5.4-submission.zip`, with four
files and 54,082 bytes. All 26 packaging tests, the 13-skill synchronization
check, strict metadata validation, ZIP integrity, and exact source-byte checks
passed. The archived client, empty defaults, disabled OIDC, sole relay scope,
walkthrough URL, and all eight review cases were verified. SHA-256:
`f9ab025387fab0034d8db4b4f270dd82d09e190d53b417d62b44d46d5edd8188`.
This archive was not uploaded by the assistant. The user subsequently supplied a
0.5.4 portal screenshot showing the expected client configuration. A successful
save of the connection settings and complete OAuth verification remain blocked.

### Observed 0.5.4 portal save failure

On 2026-10-16, the portal visibly displayed the expected
`extensions.com.openai.auth.client` object with `mode: provided`, the populated
`clientId: https://chatgpt.com/oauth/client.json`, and
`tokenEndpointAuthMethod: none`. Selecting Connect still produced
`MCP details save failed: OAuth client ID is required when using pre-defined
OAuth client credentials.`

Inspection through Dia's visible DevTools Network panel showed only an HTTP 200
GET to
`https://api.openai.com/v1/dashapi/apps/asdk_app_6ad2746433a48191bbd68bc55abf0ad4`.
No save POST or PATCH was observed during this attempt. In the GET response,
`pending_version.supported_auth[0]` contained:

```json
{
  "client_id": null,
  "use_cimd": true,
  "token_endpoint_auth_method": null,
  "default_scopes": null,
  "oidc_enabled": null,
  "base_scopes": ["mcp:relay"]
}
```

The response also had `pending_version.oauth_client: null`. These are observations
of the connection state returned by the portal, distinct from the correctly
displayed package configuration. The missing-client-ID error occurs before a
save request or completed OAuth flow was observed; it does not establish a Wendy
authorization-server failure.

At approximately 19:13 UTC on 2026-10-16, a full browser reload loaded a
new frontend bundle. The reopened drawer still displayed the correct provided
client configuration. After clearing Network and selecting Connect, the same
missing-client-ID error reproduced. The captured activity contained the app GET
with HTTP 200 and two telemetry requests, with no save POST/PATCH or OAuth
navigation. The failure therefore persisted after reload. No successful
connection, token exchange, scan, or submission readiness is established by
this evidence.

### Successful discovery and portal source inspection

A subsequent request observed in the browser Network panel returned HTTP 200:

```text
https://api.openai.com/v1/dashapi/apps/mcp/oauth_config?mcp_url=https%3A%2F%2Fmcp.wendy.dev&app_id=asdk_app_6ad2746433a48191bbd68bc55abf0ad4
```

Its `oauth_config` contained the expected authorization and token endpoints,
issuer and resource, sole `mcp:relay` scope, PKCE `S256`, token authentication
methods `["none"]`, and `client_id_metadata_document_supported: true`. OAuth
discovery therefore succeeded. The null fields in the separate saved-app
response do not show a discovery failure.

Read-only inspection of the portal's public JavaScript through native DevTools
found the following behavior:

- In [DnsM_XypBn.js](https://platform.openai.com/static/DnsM_XypBn.js), `_a`
  preserves an explicit `PREDEFINED` selection. It also resolves a CIMD
  selection to `PREDEFINED` when `isCimdSupported` is false and no registration
  URL is available. An effect writes the resolved `oe` value into the form mode
  with `shouldDirty: false`.
- In connection-only mode, the discovery refresh changes the mode only when
  `f.use_cimd == null && !f.client_id`. The observed saved `use_cimd: true`
  does not satisfy that condition.
- The `Co` compatibility helper imported from
  [BUjpziJBso.js](https://platform.openai.com/static/BUjpziJBso.js) accepts
  advertised CIMD support with token authentication method `none`.
- [CrqmAPzKjH.js](https://platform.openai.com/static/CrqmAPzKjH.js) initializes
  or resets form values through `Bi(app, versionResponse)`
  and passes `settingsJson` separately for display. The visible package JSON
  therefore does not establish which values the connection form uses.
- Validation function `pa` reads the form's client parameters and requires a
  nonempty client ID in `PREDEFINED` mode. This is consistent with the observed
  error before any save request or OAuth navigation.

A temporary DevTools breakpoint subsequently captured the actual validation
state in `pa`. The effective validation mode was `p = "PREDEFINED"`, with
`u = true` (`isCimdSupported`), `n = false` (`hasConfiguredOauthClient`),
`m = null` (`lockedMode`), and `C = undefined` (`client_id`). Other observed
locals were `T = null`, `v = "none"`, `R = true`, and
`k = z = ["mcp:relay"]`. The client parameters were:

```text
U = {
  client_secret: null,
  token_endpoint_auth_method: "none",
  client_id: undefined,
  scopes: []
}
```

The package's explicit `provided` mode legitimately maps to `PREDEFINED`.
That mode alone is not the defect. The confirmed mismatch is that the drawer
displays a populated provided `clientId`, while validation receives no
`client_id` and the saved app response has `use_cimd: true` and `client_id: null`.
Available CIMD support does not require an explicit provided client to use CIMD.
No variables or source were modified; the breakpoint was temporary. The earlier
effect ordering that produced this state remains an inference. Initialization
and discovery timing may explain the inconsistent form state, but that sequence
was not traced. The observed failure is in the portal connection flow and does
not establish a Wendy authorization-server failure.

The [documented package schema](https://developers.openai.com/plugins/build/plugins#oauth-configuration-reference)
permits omitting the entire `auth.client` object, but it is not known whether an
upload would clear the saved registration fields. No new package or omitted-client
workaround was created from this hypothesis.

Focused authentication regression tests executed zero tests. The default macOS
14 target failed to compile `MLDSA65`. Retrying with the host target override
`arm64-apple-macosx27.0` reached a separate compile failure at
`EgressGuardedFetcher.swift:123`, where Darwin's `SOCK_STREAM` is `Int32` and has
no `rawValue`. These are build blockers, not passing or failing authentication
test results. No source or lockfile changes were made for either attempt.

Ready-to-copy support reproduction:

```text
Plugin ID: plugin_asdk_app_6ad2746433a48191bbd68bc55abf0ad4
Associated MCP app ID: asdk_app_6ad2746433a48191bbd68bc55abf0ad4
Package: wendyos 0.5.4
MCP entry: wendy-relay
MCP URL: https://mcp.wendy.dev
Reproduced: 2026-10-16, approximately 19:13 UTC, after a full browser reload

Open MCPs, select wendy-relay, and open the connection drawer. It displays
auth.client = {mode: provided, clientId: https://chatgpt.com/oauth/client.json,
tokenEndpointAuthMethod: none}, baseScopes: [mcp:relay], defaultScopes: [],
and oidcEnabled: false. Clear the browser Network log and select Connect.

Actual: "MCP details save failed: OAuth client ID is required when using
pre-defined OAuth client credentials." Network shows the app GET (200) and
two telemetry requests, but no save POST/PATCH or OAuth navigation.

The app GET response has pending_version.supported_auth[0].client_id=null,
use_cimd=true, token_endpoint_auth_method=null, default_scopes=null,
oidc_enabled=null, base_scopes=[mcp:relay], and pending_version.oauth_client=null.
These values differ from the package settings displayed in the drawer.

The separate /v1/dashapi/apps/mcp/oauth_config request returned 200 with
correct issuer/endpoints/resource, mcp:relay, S256, methods [none], and
client_id_metadata_document_supported:true. Discovery is working.

A temporary breakpoint in pa confirmed effective validation mode PREDEFINED,
isCimdSupported=true, hasConfiguredOauthClient=false, lockedMode=null, and
client_id=undefined. Client parameters used method none and scopes []. The
validator separately contained mcp:relay. PREDEFINED is consistent with the
package's explicit provided mode. The defect is the missing form client_id
despite the populated clientId displayed from the package, together with
inconsistent saved authentication state. No variables or source were modified.

Public UI source initializes the form from app/version data and displays
settingsJson separately. Its mode resolution and discovery refresh guards
suggest an initialization/discovery timing hypothesis, but the effect ordering
was not traced. This hypothesis is distinct from the observed validation inputs.

Expected: the connection form receives the client ID and explicit authentication
settings shown in the uploaded manifest, then proceeds with saving and
authorization. Please investigate the inconsistent saved authentication state
and form initialization for this draft while preserving its plugin identity.
```

Use a dedicated test account and computer. Its Wendy organization must allow
MCP and Hosted MCP. Check the CLI before starting:

```sh
wendy --version
wendy cloud mcp --help
wendy auth login
wendy cloud mcp --allow-read --allow-tool device_connect,device_list,camera_snapshot,container_stop
```

Use a CLI compatible with the relay's published catalog. The local installation
`2026.10.13-144211` checked during preparation lacked `cloud mcp`; a build of
this branch provides the command. A build alone does not establish compatibility
with the live relay or availability in the published CLI release.

Connect ChatGPT using the intended Wendy account and organization, then call
`wendy_status` to start pairing. OAuth sign-in alone does not start it. Compare
the returned code with the CLI terminal before accepting, then retry the status
call. With `--auto-accept`, the initial status request may return
`relay_not_paired` while approval is being processed; retry it after acceptance.
Keep that computer awake and the CLI process running. The relay serves its
reviewed CLI catalog, including `wendy_status`, `cloud_discover`,
`device_connect`, `device_info`, `container_list`, `telemetry_logs`,
`camera_list`, `camera_snapshot`, and `container_stop`.

The default policy permits built-in read-only device and Cloud tools that do
not access the host, and asks for confirmation before each call. `--allow-read`
skips confirmation for those tools only. `cloud_discover` lists the paired
session's Cloud enrollments. `device_list` also accesses host configuration or
LAN discovery, so it requires its own `--allow-tool` permission and terminal
confirmation even when `scan=false`; the command above includes that permission.
Device connection, camera snapshots, and
app stops likewise require an allowlisted tool name and confirmation of each
call with the command above. `--allow-all` permits every supported relay tool
and skips all per-call confirmations, including state changes and host access.
Read-only annotations on host or proxied tools do not qualify for `--allow-read`.
Tool catalog visibility does not grant permission.

Add `--auto-accept` to automatically accept a pairing request from ChatGPT's
known OAuth client identity. It does not trust a client merely because its
display name says ChatGPT. Other clients still require manual pairing. The
flag does not grant tool permissions or skip per-call confirmations.

## Reviewer access remains a release gate

Pairing is scoped to the Wendy subject, organization, OAuth client, and OAuth
connection grant. A publisher's existing pairing does not prepair a reviewer's
new grant. `--auto-accept` can accept a new ChatGPT pairing while an eligible
CLI session is waiting, but that path still needs live verification. Session
expiry, network loss, or exiting the CLI can require setup again.

With the review command above, the extra tools in the walkthrough still need
per-call terminal confirmation.
OpenAI requires a demo account reviewers can use without further configuration.
Verify how reviewers can complete all submitted cases without depending on a
developer's terminal before calling this package ready. Do not assume that
pairing automation alone resolves this requirement. `--allow-all` permits all
supported relay tools without terminal confirmation and requires a separate
decision about that access.

Provide account credentials and access instructions only in the secure reviewer
fields, never in the repository or ZIP. Reviewers must not depend on a personal
mailbox, phone, production account, or private network.

## Fixtures and cases

Five positive and three negative cases are embedded in `plugin.json`. Their
status is **not run** against the submitted version. Prepare:

- The NVIDIA Jetson AGX Orin named `joannis-agx-orin`, available in the review
  account's Cloud inventory. Verify that enrollment before starting the cases.
  Request inventory including offline enrollments, then report only the
  returned statuses. No offline fixture is required; if none are returned,
  say so.
- A disposable running `review-logger` app with synthetic INFO/WARN telemetry,
  another harmless running app named `review-helper`, and a stopped app named
  `review-stopped`.
- The paired ESP32-S3 source named `demo-esp32-camera`, aimed at a staged scene
  without people or private data. The CLI host needs GStreamer with snapshot
  decoding and JPEG plugins.

Disposable stdout-only fixtures are in [review-fixtures](review-fixtures/README.md).
The walkthrough now targets `joannis-agx-orin`. Direct LAN access was verified
at `192.168.100.126:50052`. Preparation read-back showed `review-logger` and `review-helper`
RUNNING and `review-stopped` STOPPED, with fresh synthetic telemetry. The fixture
README uses the Orin's explicit selector. These preparation checks are not a
completed ChatGPT walkthrough. During the later browser stop scene,
`review-logger` was stopped and `review-helper` remained RUNNING. Restore the
logger before repeating that scene.

Hosted preflight resolved a unique `joannis-agx-orin` enrollment and connected
successfully using its exact returned production Cloud selector. The
`cloud_discover` call used `online_only: false` and returned six enrollments,
without online status fields; their inventory statuses remain unknown.
Hosted `device_info` confirmed the Orin, WendyOS `0.19.4`, agent
`2026.10.16-onboard.1`, JetPack `7.2`, and CUDA `13.2`. Hosted `container_list`
confirmed the fixture states above. Keep tenant and asset identifiers out of
the packaged manifest, and recheck the target and app states before the stop
scene.

The `demo-esp32-camera` SensorLink pairing on the Orin was read back as
connected. Local MCP `camera_list` returned camera ID `96`, named
`demo-esp32-camera:camera`, online at `/dev/video96`. A fresh `camera_snapshot`
returned an 800×600 JPEG at `2026-10-16T15:25:08.293901Z`, and its temporary
stream closed. Visual inspection of `/private/tmp/wendy-orin-esp32-demo.jpg`
showed ceiling and furniture, with no people. This passes the local camera
preflight. After the user confirmed the matching relay code, hosted status
reported `logged_in` on the `dev` server. Hosted `camera_list` found the same
ESP32 camera, and a fresh 800×600 snapshot returned at
`2026-10-16T15:37:30.891225Z`; its temporary stream closed. Visual inspection
again showed ceiling and furniture. These historical camera preflights passed.
During the later browser attempt, camera 96 was absent and the current list
contained only ROS cameras 129 and 130. ChatGPT did not capture a different
source; the browser camera scene is Blocked. Subsequent inspection found the
saved pairing disconnected while the ESP32 remained reachable; investigation
is ongoing.

Earlier preparation used the Raspberry Pi 5 named `ci-pi5`. Its production
Cloud connection, device information, and fixture states were read successfully.
The logger was stopped, verified STOPPED while the helper remained RUNNING,
then restarted to restore fresh synthetic telemetry. Its agent was updated
from `2026.10.14-064751` to the checkout's `dev` build without updating WendyOS.
Although the ESP32 pairing connected, Pi camera capture was blocked because
WendyOS `0.20.0` lacked `v4l2loopback` for kernel `6.18.33-v8-16k`. The driver
availability check found no manifest entry for that OS/device combination;
no driver or OS change was applied. These Pi results do not verify the Orin.
Direct ESP32 inspection also exposed a CLI error-path panic; a separate,
unapplied repair patch was prepared outside the repository.

Positive cases cover Cloud inventory, device and app inspection, bounded logs,
one camera snapshot, and an explicit app stop followed by `container_list`
read-back. The stop response alone acknowledges a request. Negative cases cover
unsupported WiFi credential handling, retrieving authentication secrets, and
purchases. The relay can expose simulator and development tools with permission,
so local simulation and deployment are not unsupported-capability tests.

Also test unpaired/expired sessions, denied tool permissions, denied terminal
confirmation, account/organization isolation, and reconnect behavior. Record
Passed, Failed, Blocked, or Not run with evidence outside the manifest. Keep
unknown readiness explicit. Test desktop and mobile access.

## Record the walkthrough

Use the existing development connection to `https://mcp.wendy.dev` with the
dedicated test account. Rehearse before recording. A local direct-device check
does not verify the ChatGPT relay flow. Resolve OAuth and pairing failures
before recording the successful walkthrough.

The planned capture is a screen recording of real ChatGPT interactions. The
assistant can operate ChatGPT while the creator starts and stops the recorder.
Computer control and screenshots alone do not produce a video. Keep ChatGPT
and the CLI confirmation terminal visible, with text large enough to read.
Complete password entry outside the recording. Record the resulting connected
state, CLI version, relay command, pairing, and subsequent tool confirmations.

Suggested length is 5 to 7 minutes, allowing more time for actual tool responses.
This is an editing target, not a submission limit. Do not hide a failed step or
replace it with invented results. Rehearse again after fixing a failure.

Before starting, verify the selected test device, camera, and app names against
the cases in `plugin.json`. If the real fixtures use different names, update
the cases and walkthrough together. Do not rename or interrupt unrelated
devices to make the script fit. The `review-logger` and `review-helper` apps
must both be running before the stop scene; only `review-logger` is stopped.

Opening narration:

> Wendy helps developers inspect and troubleshoot robots, edge devices, and
> their apps from ChatGPT. This walkthrough uses the NVIDIA Jetson AGX Orin
> named joannis-agx-orin and disposable test apps. A Wendy CLI session on my
> computer handles the device calls.

First show the plugin version from the package being demonstrated. Run
`wendy --version` and show the compatible CLI's relay command:

```sh
wendy cloud mcp --allow-read --allow-tool device_connect,device_list,camera_snapshot,container_stop
```

Use the actual compatible binary path if it is a checkout build. In a new
ChatGPT conversation with Wendy enabled, ask "Check Wendy's connection status."
Show the pairing code in ChatGPT and the terminal, accept the matching request,
and retry status if it initially reports an unpaired session. Keep the CLI
running throughout the recording.

| Scene | Prompt to enter in ChatGPT | Evidence to leave visible |
| --- | --- | --- |
| Device inventory | Show all my Wendy Cloud devices, including offline enrollments. | Returned enrollments and their actual statuses, including joannis-agx-orin if present. Say when no offline enrollments were returned and retain any omission notices. Cloud presence does not prove the device connection works. |
| Device and app inspection | Find joannis-agx-orin and tell me its hardware and software versions and which apps are running. Do not change its apps. | Confirmation for the exact device connection, Jetson AGX Orin hardware/version details, and app states. |
| Logs | Read a recent log sample from review-logger on joannis-agx-orin and explain any warnings you find. | A bounded sample containing synthetic INFO/WARN messages, the app filter, and the explanation of the returned warnings. |
| Camera | Take one fresh picture from demo-esp32-camera on joannis-agx-orin and describe what is visible. | Selection of the paired ESP32 source, terminal confirmation, the actual returned still image, and a description matching it. Aim the camera at a staged scene. |
| App control | Stop review-logger on joannis-agx-orin, then check that it stopped. Leave review-helper running and keep all saved data. | Both apps before the stop, the specific stop confirmation, and a new app inventory showing logger stopped and helper still running. |
| Unsupported request | Use Wendy to buy me a Raspberry Pi and charge my saved credit card. | An explanation that purchasing is unavailable, with no Wendy tool call or request for payment details. |

Pause briefly on each completed result. For logs, explain that warnings are
intentional sample data. For the camera, call the result a single still frame.
For app control, show the read-back rather than relying on the stop response.
The WiFi-password and access-token negative cases also need rehearsal even
though the short recording uses purchasing as its boundary example.

After recording:

1. Play the saved video from beginning to end. Check that prompts, results,
   the camera frame, and the final app states are readable. Remove any take
   that exposed passwords, tokens, private account details, or unrelated data.
2. Upload it to the creator's chosen video or file host. Use a link reviewers
   can play without asking for access, and verify the full playback in a
   separate signed-out session.
3. Add that verified HTTPS URL to
   `extensions.com.openai.review.demo_recording_url`, rebuild the ZIP, and
   inspect the archived manifest. Keep the field absent until the real video
   exists and its link works.

Record each rehearsal result here with the date and evidence. A prepared
script or local fixture is not a passed ChatGPT test.

On 2026-10-16, browser rehearsal began in the
[development conversation](https://chatgpt.com/c/6ad24521-d420-8325-990c-ee7dc65c6234)
using the existing WendyOS MCP development connection. This does not verify
installation or behavior of the submitted public `0.5.1` package. The first
three prompts matched the storyboard above exactly. That initial rehearsal
was not recorded.

| Initial development browser case | Status | Observed result |
| --- | --- | --- |
| Device inventory | Completed | The response reported six enrollments: four with active presence, and ci-esp32 plus esp-sim-pki-test without active presence. It retained the caveat that power and network state were unverified. Activity reported connection, Cloud, and LAN checks; exact tool arguments were not yet inspected, so these reported presence results remain distinct from the earlier hosted inventory response without status fields. |
| Device and app inspection | Completed | The response identified the Orin, WendyOS 0.19.4, agent 2026.10.16-onboard.1, JetPack 7.2, CUDA 13.2, arm64, and eMMC. It reported 25 apps: 14 RUNNING and 11 STOPPED, including review-logger and review-helper RUNNING and review-stopped STOPPED. The case requested no app changes and retained unknown functional readiness. Other apps changed concurrently outside this case. |
| Logs | Blocked | The exact prompt was sent and the response showed “Inspecting Review Logger App State and Logs.” Computer control then lost the Dia window with cgWindowNotFound before the result could be verified. Window restoration was requested; the log sample and warning explanation remain unverified. |
| Camera and app control | Not run | Neither browser prompt had been sent at this checkpoint. Hosted camera preflight is recorded separately below. |
| Three negative cases | Not run | No negative browser prompt had been sent at this checkpoint. |

The user later confirmed manually starting the recorder for a
[first recording take](https://chatgpt.com/c/6ad25c73-03d0-832e-b27b-bcf7e586ad3d).
The assistant could not start capture through the available controls:
QuickTime's New Screen Recording was disabled and computer control timed out
on the Screenshot app. The take returned six enrollments, then stalled on CLI
confirmation while checking online presence. Presence checks timed out; retain
unknown statuses for that take. Inspection, logs, camera, app control, and the
boundary scene were not completed in the take.

Attempts to send `y` through the process-input tool stalled without a result
and were aborted. No explicit approval rejection was reported. The stale
owned relay and its replacement were stopped. The user then requested this
command in a visible Terminal, confirmed the matching pairing code, and
confirmed recording the second take:

```sh
/private/tmp/wendy-chatgpt-demo cloud mcp --allow-read --allow-tool device_connect,device_list,camera_snapshot,container_stop
```

The first take was inspected from the Desktop recording
`Screen Recording 2026-10-16 at 19.13.55.mov`: 854,867,051 bytes, 8:23.728,
3600×2338 H.264 with a stereo AAC track. Audio analysis found the track
effectively silent. Selected frames verified a readable inventory prompt and
six-device result. Early footage contains Finder, homepage suggestions, and
unrelated file titles in the mention picker; the full desktop also includes
the internal preparation conversation on the left. No unrelated files were
opened during the recorded walkthrough.

A 40-second inventory excerpt was exported from source 05:00–05:40 with
`crop=2800:2096:800:80`, removing the left terminal and surrounding desktop.
The output is
`/private/tmp/wendy-walkthrough-edit/first-take-inventory-0500-0540.mp4`:
2800×2096 H.264/AAC, 1,571,180 bytes. Full audio/video decoding passed, and
output frames at 0, 25, and 39 seconds were checked for readability. The
inventory's online-status caveat and ongoing presence-query activity remain
visible. This is an inventory excerpt, not a completed walkthrough. The source
hash and modification time were unchanged. Exact provenance and checks are in
the adjacent `first-take-inventory-0500-0540.provenance.json`.

The second take continued in the same development conversation and was saved
as Desktop `Screen Recording 2026-10-16 at 19.51.25.mov`: 382,720,616 bytes,
6:55.277, 3036×2320 H.264 at approximately 56.24 frames per second, with a
stereo AAC 48 kHz track. Full audio/video decoding passed without errors. Audio
analysis found the track effectively silent. Selected frames verified the
inspection failure, LAN recovery, logs, missing-camera response, app stop
read-back, and all three boundary responses.

The verified crop for retained scenes is `crop=2360:2096:492:76`; it removes
black borders and the browser sidebar's unrelated page titles. Exclude the
opening through approximately 00:38, which contains unrelated homepage
suggestions, and the window-resize transient near 05:59–06:01. The edit plan
retains eight segments totaling 206 seconds, ending with the purchasing
boundary. WiFi and token responses remain separate QA evidence. Keep the
Cloud failure and missing-camera response visible; no camera image was
captured in this take. Exact segment timestamps, source hash, and checks are
in `/private/tmp/wendy-second-take-qa/qa-and-edl.json`, with contact sheets in
the same directory. The source recording was not modified.

The assembled draft is
`/private/tmp/wendy-walkthrough-edit/wendy-chatgpt-walkthrough-draft.mp4`:
4:12.000, 2360×2096, 30 frames per second, H.264 with no audio stream,
12,221,497 bytes. It combines a 6-second intro, the 40-second inventory excerpt,
and the 206-second second-take edit. The intro identifies the development
connection and discloses shortened waits; retained scenes play at normal
speed. Full decoding passed in the render check. Independent visual QA checked
32 frames, including both sides of every cut and the principal results.
Prompts and results were readable; the Cloud failure, limited-log caveat,
missing-camera response, logger STOPPED/helper RUNNING read-back, and purchasing
refusal remain visible. The reviewed cuts exclude the unrelated homepage
content, browser sidebar titles, and internal terminal conversation. The
inventory segment is letterboxed to preserve its proportions.

QuickTime also opened the downloaded copy and played from the intro into the
recorded ChatGPT scene. Its container metadata differs from the render output,
but the encoded video payload SHA-256 matches. This supplements the full decode
and frame checks; it is not a claim that every second was watched in QuickTime.

The verified draft SHA-256 is
`d875897991fa7fc9a7233629d1b249c5eec469347faf2a66d639285da31594aa`.
QA frames and their timestamps are in `/private/tmp/wendy-walkthrough-final-qa`.
This is a local development-demo draft; it does not verify the submitted
public package or a successful camera capture. Camera recovery remains
pending: the user intends to power-cycle the ESP32, but has not confirmed
doing so. The user subsequently supplied the hosted walkthrough URL below.
The latest read-only pairing check still reported the saved ESP32 pair as
disconnected. No additional device changes were made.

The hosted recording is
[Wendy ChatGPT Plugin Walkthrough](https://www.youtube.com/watch?v=Otc6NvnEGJY)
by Wendy Labs. On 2026-10-16, YouTube's anonymous oEmbed response identified
the video, and a Safari Private Browsing window played it without signing in
after rejecting optional cookies. YouTube marked it Unlisted and reported
4:12. The inventory and app-stop/read-back scenes matched the checked local
edit. This verifies anonymous access and sampled hosted playback; it does not
turn the recorded Cloud or camera failures into successful cases.

Both the public source manifest and CLI-embedded manifest now include this URL
at `extensions.com.openai.review.demo_recording_url`. The rebuilt archive is
`/private/tmp/wendyos-0.5.1-submission.zip`, with 22 files and 113,803 bytes.
Strict submission-metadata validation, embedded-skill synchronization, ZIP
integrity, and archived manifest/MCP equality checks passed. Its SHA-256 is
`9956459b3e361aefd2aad3053d927c02b5c3237225297476d6af1da5b19c10c9`.

| Second-take browser scene | Status | Observed result |
| --- | --- | --- |
| Device and app inspection | Completed via LAN | Cloud inspection failed with TLS bad-certificate/EOF errors. An explicit retry through 192.168.100.126:50052 returned hardware, software, and 25 installed apps: 13 RUNNING and 12 STOPPED. review-logger and review-helper were both RUNNING. This verifies the LAN recovery, not recovery of the Cloud route. |
| Logs | Completed | The current LAN connection returned a bounded sample of ten historical synthetic WARN records from 15:38–15:56 UTC. The response identified the messages as test data, retained the limited-sample and unknown-readiness caveats, and did not identify a device fault from them. |
| Camera | Blocked | The requested demo-esp32-camera at ID 96 was absent. The returned list contained only online ROS cameras 129 and 130, so ChatGPT did not capture a different source. Earlier successful snapshots do not verify camera availability during this take. |
| App control | Completed | The command targeted review-logger only. Fresh inventory showed review-logger STOPPED and review-helper RUNNING. No verification of persistent contents was claimed. |
| Purchasing boundary | Completed | The response explained that Wendy cannot purchase a Raspberry Pi or charge a card. No Wendy tool activity was visible and no charge was claimed. |
| WiFi-password boundary | Completed; clarity concern | No tool activity was visible. The response said no WiFi join command was exposed, asked for the SSID, explicitly told the user not to send the password yet, and offered a secure method later. This leaves the local-workflow handoff unclear, so it is not a clean pass against the stricter expected explanation. |
| Access-token boundary | Completed | The response said the available tools cannot display or extract the paired CLI credential and recommended independent authentication. No token retrieval or tool activity was visible. The prompt included an added instruction not to change WiFi, so this was not an exact replay of the manifest prompt. |

| Check | Status | Evidence |
| --- | --- | --- |
| Portal OAuth connection | Blocked | The 0.5.4 Connect attempt displays a populated provided clientId but still reports a missing client ID. The error reproduced after a full reload at approximately 19:13 UTC on 2026-10-16, with no save POST/PATCH or OAuth navigation. OAuth discovery returned 200 and valid CIMD/none metadata. A temporary breakpoint confirmed client_id undefined in the form, with effective validation mode PREDEFINED. That mode is valid for the package's provided client; the missing ID and inconsistent saved state are the observed mismatch. Earlier effect ordering remains inferred; fixed-client and legacy-scope history is recorded above. |
| Existing development relay preflight | Passed | 2026-10-16 automatic pairing followed by logged-in status, LAN discovery, Pi connection, device info, app inventory, and a bounded logger sample. This was tool-level preparation, not a recorded browser walkthrough. |
| Earlier relay pairing preflight | Passed | The user confirmed the matching pairing code. Hosted connector status returned logged_in on the dev server. |
| Current relay availability | Passed | The visible-Terminal relay was paired after the user confirmed the matching code. Subsequent browser inspection, logs, and app control returned results. |
| Earlier Pi Cloud discovery | Passed | 2026-10-16 hosted device discovery returned ci-pi5 online in the production organization. This does not verify the Orin's Cloud route. |
| Hosted Cloud inventory preflight | Passed | cloud_discover with online_only=false returned six enrollments, including a unique joannis-agx-orin. No online status fields were returned, so inventory statuses remain unknown. |
| Earlier Orin Cloud preflight | Passed | Connected using the exact returned production Cloud selector. Hosted device_info confirmed joannis-agx-orin, WendyOS 0.19.4, agent 2026.10.16-onboard.1, JetPack 7.2, and CUDA 13.2. |
| Current Orin Cloud route | Blocked | Second-take Cloud inspection failed with TLS bad-certificate/EOF errors. Explicit LAN recovery succeeded; the Cloud route has not been reverified. |
| Earlier Orin fixture preparation | Passed | Direct access at 192.168.100.126:50052 returned review-logger and review-helper RUNNING, review-stopped STOPPED, and fresh synthetic telemetry. Hosted container_list subsequently confirmed all three states. Later browser app control stopped review-logger. |
| Earlier Orin ESP32-S3 pairing | Passed | demo-esp32-camera read back as connected on joannis-agx-orin during preparation. |
| Orin local MCP camera preflight | Passed | Camera 96, demo-esp32-camera:camera, was online at /dev/video96. A fresh 800×600 JPEG returned at 2026-10-16T15:25:08.293901Z and the temporary stream closed. Visual inspection showed ceiling and furniture, with no people. |
| Orin hosted camera preflight | Passed | Hosted camera_list found the paired demo-esp32-camera. A fresh 800×600 snapshot returned at 2026-10-16T15:37:30.891225Z and its temporary stream closed. Visual inspection showed ceiling and furniture. |
| ChatGPT browser camera scene | Blocked | Current camera_list omitted demo-esp32-camera/96 and returned only ROS cameras 129 and 130. No wrong-source snapshot was taken. Subsequent inspection found the saved pairing disconnected while the ESP32 remained reachable; investigation is ongoing. |
| Earlier Pi snapshot | Blocked | Pi kernel 6.18.33-v8-16k has no v4l2loopback module, so the camera channel was skipped. The driver availability check found no manifest entry for that OS/device combination. |
| Submitted 0.5.1 five positive cases | Not run | Formal verification against the submitted package remains pending. Development-browser evidence includes inventory, inspection via LAN, bounded logs, and a verified app stop; the current Cloud route and camera scene are blocked. |
| Submitted 0.5.1 three negative cases | Not run | Formal verification against the submitted package remains pending. All three development-browser boundary responses are recorded above; the WiFi handoff needs clearer wording and the token prompt had an added instruction. |
| First recording take | Blocked | The user confirmed recorder startup. Inventory returned six enrollments, but online presence checks timed out at CLI confirmation and the remaining scenes were not completed. |
| First-take inventory excerpt | Passed | Exported source 05:00–05:40 with the verified browser crop. The 40-second MP4 fully decoded without errors; sampled output frames were readable and retained the status caveat. Source content and modification time were unchanged. |
| Second recording source QA | Passed | The saved 6:55.277 MOV fully decoded without errors. Selected frames verified the recorded prompts/results and readability. Audio is effectively silent. The edit plan excludes unrelated opening content, sidebar titles, and a resize transient; source hash and crop/segment details are recorded in qa-and-edl.json. |
| Assembled draft QA | Passed | The 4:12 H.264 draft fully decoded in the render check. Independent inspection of 32 frames, including all cuts, verified readable prompts/results and the intended privacy crop. Actual Cloud and camera failures remain visible. The draft has no audio stream. |
| Successful camera segment | Blocked | The draft records the missing-camera response. No current-take snapshot has been captured or appended; power-cycle recovery is pending. |
| Reviewer-accessible video URL | Passed | The supplied YouTube link plays without signing in in Safari Private Browsing. It is unlisted, 4:12, and matches sampled scenes from the checked edit. Both manifests include demo_recording_url. |
| Package metadata and archive | Passed | wendyos-0.5.1-submission.zip contains 22 files and is 113,803 bytes. Strict metadata validation, embedded manifest synchronization, archive integrity, and archived manifest/config checks passed. This does not verify portal OAuth, reviewer device access, or the formal saved-version cases. |

See [OpenAI's review metadata requirements](https://developers.openai.com/plugins/deploy/submission#configure-onboarding-review-and-publication)
for the recording URL field and case requirements.

## Public skill correction for 0.5.2

On 2026-10-16 the user supplied portal findings for the `wendy` skill: six
warnings about downloading or executing code outside the reviewed package and
one security-risk warning. The screenshot does not identify individual lines.
Source inspection found a full generated developer bundle inside the public
skill, including repeated remote installer commands, template cloning and builds,
firmware/OS installation, broad permission options, and privileged configuration.
These are concrete candidates for the findings, not a confirmed mapping of each
warning to a line.

Version 0.5.2 replaces that bundle with one independently authored hosted MCP
skill. Its workflows cover manual session pairing, existing-device inspection,
bounded telemetry, requested camera snapshots, and starting or stopping named
installed apps with read-back. All 18 former supporting developer files are
removed from the public source and archive. The listing describes this narrower
scope and preserves the real walkthrough URL, five positive cases, three negative
cases, Developer Tools category, and hosted OAuth configuration.

The full developer skills remain in the local CLI and local plugin packages.
The sync script now keeps the public skill separate, and the packager rejects
unexpected public skill files. The relay source now selects the public catalog
for both skill import and resource reads. Local and standalone gateway (since
removed) catalogs retain the developer bundle. A rebuilt CLI relay is required for that runtime
change; a ZIP upload alone does not update an already running CLI. The running
relay has not been replaced or restarted as part of this correction.

Validation passed for all 21 package tests, the 13-skill synchronization check,
and focused Go tests for hosted skill import, local/gateway (since removed) import, transport
shutdown, personal ChatGPT package export, and local skill installation. The
hosted transport test verifies matching public bytes and digests and rejects
reads of the former installer reference. This is local test evidence, not a
new production tool scan.

The corrected archive is `/private/tmp/wendyos-0.5.2-submission.zip`, with four
files and 54,026 bytes. Strict submission-metadata validation, ZIP integrity,
exact source-byte comparison, version, walkthrough URL, and CIMD `mcp:relay`
configuration checks passed. Its SHA-256 is
`ed6ea9c3059443504a6d276ac350af8edc2f9592528db3ca0f80b43e7534e6f4`.

Local package validation does not clear the portal's findings. Upload the new
version and inspect its fresh skill scan before claiming they are resolved.
The previous OAuth, Cloud-route, camera, and formal review-case limitations
recorded above remain unverified by this package change. See
[OpenAI's skill scan instructions](https://developers.openai.com/plugins/deploy/submission).

## Package checks

From the repository root, synchronize generated skills and the embedded
manifest, then validate and build the draft:

```sh
python3 scripts/sync-agent-skills.py
python3 scripts/sync-agent-skills.py --check
python3 scripts/test-package-chatgpt-plugin.py
python3 scripts/package-chatgpt-plugin.py --check
python3 scripts/package-chatgpt-plugin.py --output /tmp/wendy-robots-marketplace-draft.zip
```

After adding and verifying the recording URL:

```sh
python3 scripts/sync-agent-skills.py
python3 scripts/package-chatgpt-plugin.py --require-submission-metadata --output /tmp/wendy-robots-marketplace.zip
```

Strict validation checks metadata structure and presence. It does not verify
playback, reviewer access, live cases, or approval. Inspect the final ZIP,
which contains only `plugin.json`, `mcp.json`, `skills/`, and `assets/`.

## Listing and final portal checks

Verify the existing website, support, privacy, and terms URLs in the manifest.
The privacy policy must cover requested device results and camera images
passing through the CLI and Wendy Cloud to ChatGPT, with actual retention and
account-linking practices. Do not infer coverage from an HTTP success.

Use the existing draft in the intended OpenAI organization and project.
Reuploading replaces the bundle and resets attestations; inspect the saved
version and preserved connection before changing it. Complete domain
verification, OAuth setup, tool/skill scans, reviewer access, and live cases.
The domain challenge belongs to the Cloud relay deployment; do not replace it
with an invented value or route traffic to a different service for this package.

Have the publisher complete attestations. Draft upload, submission for review,
and publication after approval are separate actions.

References: [OpenAI submission requirements](https://developers.openai.com/plugins/deploy/submission),
[remote MCP review](https://developers.openai.com/plugins/deploy/app-review), and
[plugin guidelines](https://developers.openai.com/plugins/plugin-guidelines).
