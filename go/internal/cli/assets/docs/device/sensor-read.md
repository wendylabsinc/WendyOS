# Read generic SensorLink samples from an app

On a Linux Wendy Agent, pair the source using `wendy device pair`, then give the
consuming app this entitlement in `wendy.json`:

```json
{ "entitlements": [{ "type": "sensor-read" }] }
```

The agent sets `WENDY_SENSOR_SOCKET=/run/wendy/sensors/sensors.sock`. Connect a
gRPC client to that Unix socket using local insecure transport credentials;
filesystem permissions and the entitlement's private mount provide access
control. No TCP port or administrative API is exposed by this socket.
This entitlement reads **all paired generic channels** permitted by each pairing's
sensor allowlist. It is not a per-source or per-channel authorization mechanism.

Use the existing `wendy.agent.services.v2.WendySensorService` generated client:

1. Call `GetSensorManifest` to get the sensor descriptors and their typed sources.
2. Select the returned channel IDs whose `sensor.schema` your app understands.
3. Call `StreamSensors` with those IDs. Cancel the RPC when finished.
4. If a source disconnects, refresh the manifest and retry with backoff. There is
   no manifest-watch RPC; rediscover periodically to find newly connected sources.

The agent forwards opaque payloads. Thermal conversion, units, calibration,
heatmaps, ROS topics and application-specific alarms belong in your app.
Neither four channels nor any particular sensor model is assumed.

## Source identity

Forwarded `channel_id` and `input_id` values are local routing IDs. They change
when a source reconnects or the agent restarts. Do not persist them as identities.
The original display name, schema, sample size and rate are preserved.

Each returned descriptor contains an optional `SensorSource source` message:

```json
{
  "channelId": 17,
  "source": { "pairingKey": "asset:123", "channelId": 1, "inputId": 0 }
}
```

`source.pairing_key` is the authenticated immediate upstream pairing key: a
device SPIFFE principal for current PKI pairings, or `asset:<id>` for legacy
numeric pairings. Persist `(source.pairing_key, source.channel_id)` to identify
a paired channel. `source.input_id` identifies alternate formats of one physical
input on that device. These are channel identities, not sensor serial numbers.
The same display name on two devices does not imply the same source.

The forwarding agent sets this field from its authenticated pairing and original
channel/input IDs, replacing any upstream-provided value. It is descriptive
metadata, not an authorization grant. Direct sources and older agents can omit
it; clients must check message presence. Older readers can ignore the additive
field. No new firmware or RPC signatures are required.

For example, with the existing Go generated client (`client`):

```go
manifest, err := client.GetSensorManifest(ctx, &agentpbv2.GetSensorManifestRequest{})
if err != nil { return err }
for _, sensor := range manifest.GetSensors() {
    source := sensor.GetSource()
    if source == nil { continue } // direct source or older forwarding agent
    // Identify by source.GetPairingKey() and source.GetChannelId().
    // Select by sensor.GetSensor().GetSchema(), then stream sensor.GetChannelId().
}
```

## Samples and lifecycle

Each output `SensorData` contains one reassembled sample: `chunk_seq=0`, the
last-chunk flag set, and the original frame sequence, source timestamp and other
flags. Payload bytes are unchanged. `ts_us` remains in the source's time domain
(for Wendy Lite, uptime in microseconds); it is not converted to host wall time.
Equal, zero or decreasing timestamps are passed through. Apps decide how to
handle source clocks and frame-sequence gaps or wraparound.

Subscriptions share the pairing's upstream connection. For this first version,
paired channels are captured even without app readers, matching existing media
pairing behavior; app cancellation does not stop upstream capture. Unpairing or
disconnecting closes the upstream stream. Demand-driven capture is not included.

For Wendy Lite, one format per `input_id` can be active. Mounted cameras have
priority, followed by microphones, then generic channels in ascending channel-ID
order. A pairing's sensor allowlist can select a particular mode. Older agent
sources without meaningful input IDs retain concurrent streams.

Supported generic descriptors fit within 2048 protobuf bytes and have a nonempty schema (up to 1024 bytes), a name
of at most 256 bytes, and a fixed `sample_bytes` from 1 to 65,536. Unknown schemas
are forwarded; `rate_hz=0` is allowed for event-driven fixed-size samples.
Unspecified/variable sample sizes and malformed lengths are not forwarded.
Each pairing has a separate eight-frame FIFO for generic samples before app
delivery. When that queue fills, newly arriving generic frames are dropped.
Delivery is best effort. Applications needing guaranteed event delivery need
a separate acknowledgement/replay protocol.

Limits are 1024 active generic channels across sources, 64 concurrent readers,
and 1–64 selected channels per reader. Each reader has an eight-sample queue.
An overflowing reader ends with `ResourceExhausted`; a source disconnect ends
its readers with `Unavailable`. Unknown or stale IDs return `NotFound`; an empty
or oversized selection returns `InvalidArgument`. Refresh the manifest after a
disconnect. No samples captured before subscription are replayed as live data.

The dedicated directory mount survives socket recreation on agent restart.
Existing readers still have to reconnect. `WENDY_LOCAL_SOCKET=off` disables this
socket as well as the administrative local socket. Older agents without
`sensor-read` cannot provide this API; update rather than granting `admin`.
