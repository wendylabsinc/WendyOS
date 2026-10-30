# ThermalNotify

Monitors all thermal cameras attached to the device. When any valid sensor pixel
is strictly above **80°C**, it sends a **CRITICAL** Wendy notification to every
organization role: owner, admin, billing manager, member, and viewer. Cloud uses
the sending device's organization. No recipient IDs or Cloud credentials are
stored in the app.

Run one instance on CCR3 and one on mesh-1 to monitor cameras on both devices.
Use each device's exact selector from `wendy cloud discover`:

```sh
wendy run --device <cloud-device-selector> --detach --yes
```

An owner or admin must enable **Send notifications** for
`sh.wendy.examples.thermalnotify` in the organization's Cloud app settings.
The local `notifications` entitlement alone does not enable that Cloud grant.

The app uses the same private notification gRPC service as `../WendyNotify`.
The `notifications` entitlement supplies `WENDY_SYSTEM_SOCKET`. The `admin`
entitlement supplies `WENDY_AGENT_SOCKET` for the Agent's shared raw camera
streams. This grants full local Agent access; the current camera streaming API
is not available through the narrower notification socket. The app does not
open camera nodes itself. Its raw-stream request leaves width, height, and FPS
at zero to share the current capture without conflicting with existing viewers.

## Trigger behavior

- Each camera has independent alert state, keyed by its stable ID when available.
- The first valid reading above 80°C triggers an alert, including at startup.
- A sustained hot reading sends one alert. A valid maximum of 80°C or below
  rearms that camera. Invalid frames and disconnects do not rearm it.
- Sends are spaced at least ten seconds apart across all cameras to stay below
  Cloud's per-app rate limit. The cooldown starts when a send completes, even
  after a slow connection. Events queue independently of camera capture, so a
  brief excursion still sends even if the camera cools before its turn. Repeated
  excursions on a camera with an unsent alert coalesce into that pending alert.
- Failed sends retain their UUID and retry after at least 60 seconds. Rate-limit refusals wait at least 15 minutes, or longer
  if the server asks. `ALREADY_EXISTS` means the earlier request was accepted.
- State and queued alerts are held in memory. Restarting loses pending alerts
  and rearms all cameras.

The notification includes the camera name, measured maximum, pixel coordinates,
temperature conversion provenance, and a link to the sending device's live view.
Creation confirms Cloud acceptance, not phone delivery.

## Camera requirements

A recent Agent must identify the thermal camera and expose its raw frames with a
version 1 thermal measurement plane. Two temperature paths are supported:

- A declared per-pixel temperature conversion, such as the original TC001
  256×384 mode. The decoder honors the plane offset, stride and invalid values.
- The HIK-based TC001 256×392 default mode used on CCR3 and mesh-1. The camera
  reports maximum and minimum temperatures in a separate metadata block.
  ThermalNotify uses its maximum, with the same decoder as Wendy's desktop
  viewer. It validates the layout, signatures, codes, and coordinates before
  converting the metadata code with `code / 64 - 50`. This formula is never
  applied to the raw sensor pixels. See the [protocol evidence](https://github.com/Krastanov/ht-203u-linux/blob/main/THERMOMETRY.md).

HIK metadata updates around 2.5 Hz rather than on every video frame. The first
summary after connecting is ignored until it changes, because it may be cached.
An unchanged summary expires after three seconds; only a new summary resumes
monitoring. This matches the desktop viewer's freshness rules.

Other cameras that expose only raw counts, including uncalibrated PureThermal
modes, log `Celsius monitoring unavailable`. The app never infers Celsius from
image brightness. Frames older than five seconds are rejected, so the device
clock must be correct.

New cameras are discovered every five seconds. Stream failures retry after five
seconds. Logs show valid temperature reception, unsupported modes, and exact
notification errors.

## Development

```sh
swift test
```

Tests use synthetic frames and do not send notifications. Generated gRPC clients
are checked in. After changing the repository protobuf definitions, regenerate
them with `./regenerate-grpc.sh`, which requires `protoc`.

Verify container state and logs after deployment, then verify a real camera
measurement above the threshold before treating the installation as tested.
