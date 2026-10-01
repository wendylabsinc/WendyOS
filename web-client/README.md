# Wendy Client

A browser workspace backed by Wendy's Go client compiled to WASM. It starts with
an empty workspace. Device metrics, applications, and OpenTelemetry data come
from connected agents; there is no demo mode.

## Electron desktop

The desktop workspace uses the same components and theme as the browser client.
It bundles a CLI built from this checkout, so local workflows do not depend on
the version of `wendy` installed on your PATH. The browser app still runs with
`npm run dev` as described below.

From `web-client`:

```sh
npm ci
npm run desktop
```

This builds the CLI, copies the Go2 runtime source, prepares `node-pty` for
Electron, and opens the desktop app with Vite on `127.0.0.1:5174`. Node.js 22.13+
and Go 1.27+ are required to build. Compiling the terminal dependency may also
require Xcode Command Line Tools on macOS or a C++ toolchain on Linux/Windows.

To run without a development server, or produce a native application:

```sh
npm run desktop:build
npm run desktop:start
npm run desktop:package
```

The package is written to `release/` for the current OS and architecture. On
macOS it contains `Wendy Desktop.app`. This is a local development package;
release signing and notarization are not configured. Packaging includes only
the desktop renderer, native bridge, terminal module, CLI, and simulator source.
The packaged application does not need Node, Go, Vite, or a separate web server.

- Choose **Docker** or **Apple Container** in the sidebar. Docker uses your
  current context and environment. Apple Container requires an Apple silicon
  Mac; **Start Apple Container** opens its service and builder setup prompts.
- **Open folder**, then **Chat** for persistent interactive Wendy chat. The
  settings button configures the model/provider through Wendy's existing setup.
  Chat credentials stay in Wendy's CLI configuration. Tool approval remains
  interactive. Open an empty folder and ask chat to create an app, or open an
  existing Wendy project.
- **Build** and **Build & run** use an explicit target. Local container targets
  select their own provider; remote devices use the selected builder.
  A target can be `docker`, `apple-container`, `vm:name`, or a Wendy device
  address. Output and any CLI prompts appear in persistent sessions below.
- **New simulator** offers Unitree Go2, Unitree G1, and Raspberry Pi app simulation.
  Robot simulation builds the checked-in image with the selected runtime
  and starts an isolated container with a loopback-only viewer port. The first
  build downloads pinned ROS, MuJoCo, model and policy dependencies. Each robot
  uses 4 CPUs and 4 GiB. The live viewer appears after the endpoint's simulation
  identity is verified. The 3D canvas and movement controls are part of the
  desktop page, with no embedded page or nested viewer scrollbar. Scrolling
  over the scene scrolls the workspace; zoom uses the + and − buttons.
  Camera, lidar, pause, and reset controls stay beside the view. World, sensor,
  and ROS app settings expand below. Pausing or a robot fault leaves the viewer
  available to recover.
- **Raspberry Pi apps** uses a generic ARM64 WendyOS VM through QEMU with 2 CPUs,
  2 GiB of memory, and a 16 GiB virtual disk. It supports application development,
  not Pi GPIO or peripheral emulation. QEMU installation and OS download progress
  appear in the session. Readiness requires a response from the guest agent.
  Open **Dashboard** to inspect the VM. Set the build and chat target in the sidebar
  when you want to deploy to it. **Boot logs**
  streams its serial console; **Device logs** streams app and agent logs.
- **Devices & monitoring** lists existing local VMs and discovers network devices.
  Open **Dashboard** directly from a device or VM row for live CPU, memory,
  disk, GPU, temperature, and application usage. Metrics depend on the device.
  **Logs** displays searchable records with application and severity filters,
  pause/resume, and expandable details. **Containers** and simulator **Boot logs**
  use the same log viewer. Monitoring runs in native app views and only connects
  to already running VMs.
- Simulator records persist across app restarts. Closing the app stops active
  chat/build/installer sessions after confirmation; simulator containers and VMs keep
  running. Use **Stop** to stop one. **Logs**, **Start**, and **Retry build**
  operate only on that simulator's labeled container.
  VM **Start**, **Stop**, and **Retry setup** operate on its dedicated VM record.
- **Flash device** defaults to Raspberry Pi 5, with Pi 3/4 and Jetson developer
  kits also available. Find the drive, review the exact release/target/checksum,
  then open the installer. The desktop rechecks the plan and asks for native
  confirmation before starting. Wendy retains its own disk, Wi-Fi, naming and
  administrator prompts. No automatic `--yes` or internal-drive bypass is used.
  Verify first boot at an explicit address after the write finishes.

These container simulators have their own ROS loopback bus. An app run on the
ordinary Docker or Apple Container target is a separate container and does not
automatically join that bus. For the existing managed ROS app deployment flow,
create a robot VM with `wendy vm create NAME --profile go2` or `--profile g1`
and target `vm:NAME`.
The managed VM flow currently builds its robot runtime with Docker. The desktop
container viewer itself works with either runtime and requires no QEMU VM.

Native access is confined to the desktop's main frame through a small preload
API. The renderer is sandboxed with Node integration disabled. The desktop
bundles the viewer code and requests only allowlisted simulator data and control
endpoints through the native bridge. Container-hosted scripts never execute in
the desktop renderer. Commands use argument arrays, never shell strings.
Project paths require the native folder chooser; flash plans stay in the main
process and cannot be replaced by a command array supplied by the renderer.
The existing browser/cloud credential store is separate from CLI chat state.

Validation:

```sh
npm run desktop:test
npx tsc --noEmit
npm run desktop:build
# Hidden renderer fixture checks Dashboard and Logs interactions without devices.
npx electron scripts/desktop-verify-monitoring.mjs
WENDY_DESKTOP_SMOKE=1 npm run desktop:start
# Optional integration check: builds a disposable simulator, verifies physical
# movement and the viewer endpoint, then removes its own container.
node scripts/desktop-verify-simulator.mjs docker
node scripts/desktop-verify-simulator.mjs apple-container
node scripts/desktop-verify-simulator.mjs docker g1
node scripts/desktop-verify-simulator.mjs apple-container g1
# Creates its own guest, deploys an app, checks Dashboard data and logs, and removes it.
node scripts/desktop-verify-vm.mjs
```

The smoke launch checks the React renderer, preload, runtime probes and a real
Electron terminal running the bundled CLI. Project actions can also be checked
with `node scripts/desktop-verify-project.mjs docker` or `apple-container`.
Flash tests use
fixtures to verify changed-drive rejection and confirmation; they do not erase
physical storage. Model responses require configured chat credentials.

## Run locally

Install Node.js 22.13 or newer and Go 1.27 or newer, then run from `web-client`
inside the Wendy repository:

```sh
npm i && npm run dev
```

This builds the Go WASM client and matching JavaScript runtime, builds and starts
the Cloud API and broker relay on `127.0.0.1:8788`, then starts the web app on port
5173. The relay stops with the dev server when you press Ctrl-C. No separate
relay terminal or hosted Site configuration is needed. The first run may download
Go modules. Restart `npm run dev` after changing Go code to rebuild it.

Both ports must be free. Stop any manually started relay or previous dev server
before running this command. The app fails if port 5173 is occupied, since signing
in requires the registered OAuth callback on that port.

If Cloud discovery cannot connect, check the `npm run dev` terminal for relay
upstream errors. The relay verifies TLS and HTTP/2 before accepting the browser
connection, and logs connection failures there.

Open **http://localhost:5173**. Use this exact hostname and port: the existing
`cloud-login` OAuth client registers `http://localhost:5173/auth/callback`.
Enter your email and choose **Sign in with Wendy**. Authentication opens at
`auth.dev.wendy.sh`; your password never passes through this app.

The WASM worker follows the CLI's flow:

- Email-to-realm lookup, OIDC discovery, authorization code and S256 PKCE.
- ML-DSA-65 operator key generation, per-request DPoP proofs and nonce retry.
- Verification of access-token signatures, issuer, audience, expiry and key binding.
- A CSR and operator certificate from `identity.dev.pki.wendy.sh`.
- Refresh-token rotation to the Cloud API resource, then paginated v2 device discovery.

The worker saves the private key, certificates, profile, and refresh token in
IndexedDB for this browser origin. Reloading or reopening the browser restores
the same certificate, and expired access tokens are refreshed. These credentials
are not sent to the UI thread or stored in localStorage. The
same-origin `/api/wendy-auth` proxy forwards only allowlisted Wendy auth/PKI
requests and Cloud grant signing keys; tokens and public CSRs pass through it, but private keys do not.
The local `/cloud` WebSocket relay terminates verified public TLS to the fixed
`api.dev.wendy.sh:443` upstream. DPoP proofs are generated inside the worker.
The relay binds only to a loopback IP and accepts direct loopback connections
with the configured browser origin. Origin is a browser CSRF check, not relay
authentication. Remote listeners and reverse-proxy forwarding are unsupported;
do not expose this development relay through a proxy or port forward. A hosted
relay requires a separate authenticated service. The relay does not accept an
arbitrary Cloud hostname.

Sign-out deletes the saved credentials and clears other open Wendy Client tabs.
Transient connection failures preserve the saved session for retry. An expired
operator certificate requires signing in again. Browser storage is specific to
the browser profile and origin, so localhost and the hosted Site have separate
sessions. Clearing site data also clears the saved login.
Sign-out is local; it does not log you out of other Wendy applications.

## Cloud workspace

The sidebar shows the signed-in email from OIDC UserInfo and the organization
name from Cloud's v2 OrganizationService. UserInfo must match the authenticated
subject, and organization responses must match the authenticated tenant UUID.
Profile lookups that fail leave a compact expandable message in the sidebar.

Select an online cloud device to open a live `wendy device top` dashboard with
CPU, memory, container storage, temperatures, GPUs, battery, and per-application
usage. CPU uses counter deltas and normalizes application usage across all cores.
Missing metrics remain unavailable. Applications can be started, stopped, and
restarted. Selecting an application opens its logs.

Device tabs stream OTLP logs, metrics, and traces from the selected cloud device.
Streams support application filters, search, and expandable attributes. The UI
retains at most 500 entries and shows stream failures. Applications must emit
OTel data for it to appear. There is no browser shell; shell RPCs are disabled.

Cloud connections use the CLI's signed tunnel authorization, verified Cloud
grants, broker challenge proofs, and end-to-end agent mTLS pinned to the selected
tenant/device principal. The worker reads the selected asset's `pki_device_name`
enrollment binding from the authenticated Cloud API. It does not assume the
inventory UUID is the certificate identity or learn that identity from the peer.
The local `/broker` relay accepts only `relay.dev.wendy.sh`, `relay.wendy.sh`,
`eu.relay.wendy.sh`, and the exact Wendy development Cloud Run broker on port 443, and verifies
their public TLS certificates. The worker selects
the endpoint from the verified grant. Operator private keys stay in the browser.
Both inventory and device sessions currently require the local relay. Native
flashing, local builds, and LAN scanning still require the CLI.

## Hosted version

The existing private Site is also updated. Wendy auth currently rejects its
callback URL. To enable hosted sign-in, register this exact redirect URI on the
`cloud-login` public client and enable it in the login component:

```
https://wendy-client.joannis690160.chatgpt.site/auth/callback
```

Hosted Cloud discovery additionally needs a trusted WSS Cloud relay and its
configuration in the worker. Until these exist, the hosted UI explains the local
sign-in requirement. It does not pretend to be signed in or substitute fake data.

## Validation

```sh
# From the repository root:
go test -race ./go/internal/cli/browserauth
node go/experiments/wasmgrpc/auth-worker.mjs
node web-client/tests/credential-store.test.mjs
node --experimental-strip-types web-client/tests/telemetry.test.mjs
node --experimental-strip-types --test web-client/tests/auth-proxy.test.mjs

# From web-client:
npm run build:wasm
npx tsc --noEmit
npm run build
```

The auth worker integration test uses the running local app and live auth
metadata/authorization endpoints without signing in a user. Unit tests cover a
complete PKCE/DPoP/certificate flow and reject callback replay, state mismatch,
expired callbacks, untrusted issuers, and invalid token claims. Interactive
account login and account-specific discovery require the user's authentication.
The agent worker integration fixture lives in `go/experiments/wasmgrpc`.

Browser UI automation has not been run. Optional WebMCP read-only workspace and
navigation tools are feature-detected; credentials are never exposed to them.
