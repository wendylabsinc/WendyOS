# Application records and streams

Add `episode-write` to an application's `wendy.json` when it must contribute
records. Use the injected socket paths; apps do not need the broad `admin`
entitlement to record data. Named streams use `WENDY_DATA_STREAM_DIR`; the legacy
JSON event/prediction endpoint uses `WENDY_DATA_SOCKET`.

## Choose the recording contract

| Path | Use | Delivery meaning |
| --- | --- | --- |
| Legacy JSON socket | Existing event/prediction clients | `buffered` means volatile pre-roll; `recorded` means accepted into an active episode. Neither means cloud upload. |
| Lightweight stream | Native text, CSV, JSON, images, or binary packets with no SDK | A successful send means the kernel accepted the packet. The agent uses bounded memory and copies records into episodes. |
| Durable stream | Records that need acknowledgement after local persistence, including outside an episode | `COMMITTED` follows storage sync; `DUPLICATE` identifies an already retained record; `REJECTED` requires handling. These are not cloud acknowledgements. |

Do not treat durable journaling as a guarantee that a trigger successfully
started a campaign. Inspect the episode separately.

## Configure named streams

This example keeps temperature samples durably and uses a small lightweight
stream to trigger an `incident` event. Merge the entitlement into the existing
application configuration, then validate with `wendy json validate`.

```json
{
  "type": "episode-write",
  "streams": {
    "temperature": {
      "mode": "durable",
      "mediaType": "application/protobuf",
      "schema": "wendy.agent.apps.v1.TimeSeriesBatch",
      "storage": { "maxBytes": 1073741824, "retentionSeconds": 0 },
      "timeSeries": {
        "clock": "CLOCK_BOOTTIME",
        "channels": [{ "name": "temperature", "type": "float64", "unit": "Cel" }]
      }
    },
    "incidents": {
      "mode": "lightweight",
      "mediaType": "application/json",
      "event": "incident"
    }
  }
}
```

Each stream is `<WENDY_DATA_STREAM_DIR>/<stream>.sock`, a Unix
`SOCK_SEQPACKET` socket. One lightweight send is one raw payload with no Wendy
header or acknowledgement. Durable streams use the `Record` and `Ack`
Protobuf messages in `Proto/wendy/agent/apps/v1/recording.proto`, wrapping opaque
payload bytes. Start from the senders and export reader in
[WendyRecording](https://github.com/wendylabsinc/WendyOS/tree/main/Examples/WendyRecording)
instead of inventing a wire format.

Packets must be nonempty and at most 1 MiB including a durable envelope. Kernel
socket buffers can impose a smaller limit. Per-app limits are 200 packets per
second and 16 concurrent connections, shared with the legacy endpoint. Batch
samples with their acquisition timestamps to fit the packet budget. This is
not a bulk video transport.

Time-series declarations record channels, units, and the sample clock. Durable
envelopes can provide explicit sample timestamps or a start, interval, and
count. Include the boot ID with `CLOCK_BOOTTIME` timestamps. Lightweight time
series must declare `timestampField` unless they use the built-in
`TimeSeriesBatch` schema. Preserve gaps with explicit times or new batches;
agent receipt time remains separate.

Media types include text, CSV, JSON, CBOR, Protobuf, Arrow IPC, JPEG, PNG, and
custom binary layouts. The agent generally retains payloads as opaque bytes;
declaring a schema does not install a decoder or fetch that schema. The
explicit `wendy.agent.apps.v1.TimeSeriesBatch` schema validates channel types,
timing, and sample counts. An opaque JSON `event` field does not trigger a
campaign. Declare a fixed stream `event` or `model` for that purpose; durable
model records can also carry uncertainty and input references.

Durable IDs are scoped to app, service, and stream. Retry the same envelope and
ID after an uncertain result; changed content under the same retained ID is
rejected. Deduplication ends when the record expires or is reclaimed. Defaults
are a 64 MiB spool and 24-hour age retention. `storage.maxBytes` accepts 1 MiB
through 1 TiB, and `retentionSeconds` accepts 0 through 31536000. Zero disables
age expiry until acknowledged export. Full journals reject new records instead
of overwriting retained data, so producers need backpressure handling.

## Events, predictions, and model inputs

For the legacy endpoint, each Unix stream message is a four-byte big-endian
length followed by at most 64 KiB of JSON. Version 1 records have type `event`
or `prediction`, a boot ID and `client_boottime_nanos`; events name `name`, and
predictions name `model`. Put uncertainty in `attributes.uncertainty` and model
version/detections in attributes. Use
[WendyDataModelApp](https://github.com/wendylabsinc/WendyOS/tree/main/Examples/WendyDataModelApp)
for framing, acknowledgements, and a model app that consumes agent-fed frames.

Predictions may include up to 32 `inputs` references with `source_id` and
`sample_id`. Preserve the identifiers supplied with model input; do not invent
them from wall time. They join the outcome to `model_inputs.jsonl` and camera
`index.jsonl`, whose segment/offset/size identify recorded payload bytes.
Missing input references and inputs without retained payloads remain visible in
the manifest's `model_io` counts. A stored prediction alone does not prove its
input can be recovered.

For asynchronous scoring, set campaign `capture.drain` to fit result latency,
within its 30-second maximum. A `buffered` result that arrives after sealing
may appear in a later episode's pre-roll; it does not rewrite the earlier one.

## Retrieve and verify

Plain export snapshots retained durable records without deleting them or
changing cloud upload state. It excludes volatile lightweight records and
refuses to overwrite an existing file.

```sh
wendy data export-stream <app-id> <stream> --device <selector> -o ./records.wdr
```

Add `--service <service-name>` for an explicit service. The exported format is
checksummed Protobuf `.wdr` frames. Use the example `read_export.py` to inspect
records and recover original payloads. Episodes also contain `records.wdr`
alongside legacy `events.jsonl`; inspect/download the episode for those copies.

When continuous export and reclamation are requested:

```sh
wendy data export-stream <app-id> <stream> --device <selector> \
  --follow --reclaim --interval 1s -o ./capture
```

The parent directory must exist. `--reclaim` acknowledges device reclamation
only after syncing the saved chunk and its directory. `--follow` repeats with
separate files, retries temporary RPC failures, and stops on local storage
failure. It does not manage receiver disk usage. Lost acknowledgements or
overlapping exports may duplicate records; deduplicate by stream identity and
record ID. Verify record counts, timestamps, units, payloads, and spool behavior
before claiming sustained capture at a requested rate.
