Wendy devices produce their structured logs, metrics and tracing through [OpenTelemetry](https://opentelemetry.io/).

These logs are received by a local on-device collector, which forwards the stream to other targets. One of those targets is the Wendy Cloud.

The OTel spec is a standard wire protocol, also based on gRPC, that we can implement and codegen. Wendy Cloud then collects the OTel data, and "processes" it. Any queries over structured logs, visualisation and forwarding of data happens on these streams.

## Environment variables injected into app containers

When the agent starts an app container it injects a set of environment variables
on top of the image's own env. These identify the app, the device and the agent
in every network mode, and need no entitlement:

| Variable | Value |
|---|---|
| `WENDY_HOSTNAME` | For single-container apps: the device's mDNS hostname (omitted when unresolvable). For multi-service apps (`serviceName` set): `{serviceName}.local`, giving each service a distinct hostname identity. |
| `WENDY_DEVICE_HOSTNAME` | The device's own mDNS hostname, injected unconditionally. Unlike `WENDY_HOSTNAME` it is always the host's name, never a per-service `{serviceName}.local`, so a multi-service app can still identify the device it runs on. |
| `WENDY_DEVICE_NAME` | The device's name, for example `orin-nano-pr319`. See [Device name](#device-name) for where it comes from. Omitted when the host has no name. |
| `WENDY_APP_ID` | The `appId` from `wendy.json` (omitted when empty). |
| `WENDY_APP_GROUP` | The `appId` of the owning app. **Multi-service only** — injected when `serviceName` is non-empty so a service can discover its siblings. Absent for single-container apps. |
| `WENDY_AGENT_VERSION` | The version of the Wendy Agent that created the container, as `wendy device info` reports it: for example `2026.11.13-120000`, or `dev` for a local build. |

An app cannot set or override these. A deploy that names a `WENDY_` variable is
refused, and a value the image sets for one of them loses to the agent's.

A native macOS app started by Wendy Agent for Mac is not a container, and of
this table it receives `WENDY_APP_ID` only.

### Device name

`WENDY_DEVICE_NAME` is the name the device itself holds, taken from the first of
these that exists:

1. The name set with [`wendy device rename`](../clients/wendy-cli/commands/device/rename.md).
2. The name the device was given when WendyOS was installed: the one you chose,
   or the one generated for it when you chose none. The device's hostname is
   this name with a `wendyos-` prefix, so `WENDY_DEVICE_NAME=orin-nano-pr319`
   goes with `WENDY_DEVICE_HOSTNAME=wendyos-orin-nano-pr319.local`.
3. The host's own hostname, on a machine that is not a WendyOS image, such as a
   Linux computer or a robot the agent was installed on.

`wendy discover` lists a device on your network under this name written as
capitalised words: `orin-nano-pr319` appears as `Orin Nano Pr319`.

This is not the device's name in Wendy Cloud, which the agent is never told.
The two match when the device was enrolled while WendyOS was being installed,
and after a `wendy device rename`, which sets both. They differ when the device
was enrolled under another name with `wendy device enroll`, or renamed in Wendy
Cloud alone.

### When the values are read

The agent writes these variables into the container when the app is deployed,
and they stay as they are for as long as that container exists. Restarting the
app, rebooting the device, updating the agent and renaming the device do not
change them: after an agent update `WENDY_AGENT_VERSION` still holds the version
that deployed the app, and after a rename `WENDY_DEVICE_NAME`, `WENDY_HOSTNAME`
and `WENDY_DEVICE_HOSTNAME` still hold the old name. Deploy the app again, for
example with `wendy run`, to pick up the current values.

The private app connection variable is conditional on an entitlement:

| Variable | Value | Notes |
|---|---|---|
| `WENDY_SYSTEM_SOCKET` | `/run/wendy/system/system.sock` | Injected only with the `notifications` entitlement. Points to the app's private app-facing socket. |

The OpenTelemetry variables are injected **only when the app has the host
`network` entitlement**, because the agent's local OTLP receiver listens on the
device loopback and is reachable only from a host-networked container. Each is
left untouched if the image already sets it, so image-provided values win:

| Variable | Value | Notes |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://127.0.0.1:4317` (port from `WENDY_OTEL_PORT`, default `4317`) | Only set if the image hasn't configured an endpoint. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | Only set together with the default endpoint above (i.e. when the image hasn't set an endpoint) and only if absent. An image that sets its own endpoint keeps full control of the protocol. |
| `OTEL_SERVICE_NAME` | `<appId>` for a single-container app; `<appId>_<serviceName>` for a multi-service app | Only set if absent and `appId` is non-empty. |
| `OTEL_RESOURCE_ATTRIBUTES` | `wendy.app.name=<appId>` | Only set if absent and `appId` is non-empty. |

`OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` set the `service.name` and
`wendy.app.name` resource attributes that `wendy device logs --app <id>` filters
on, so telemetry an app exports directly via OTLP is attributed to its app id
without the app having to hardcode it. (Container stdout/stderr is already
stamped with these attributes by the agent's log bridge.) The identity variables
are set even when the image preconfigured a custom endpoint, so an app pointing
at its own collector still produces app-filterable logs.

The agent uses the OTLP receiver port (`4317`, gRPC) for the injected endpoint;
an HTTP/protobuf receiver is also available on `4318` for clients that prefer it.
