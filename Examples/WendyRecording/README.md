# Application recording streams

This example extends PR #1973's `episode-write` entitlement. Merge the `streams`
configuration in `wendy.json` into your application's entitlement and deploy it
with `wendy run`. These scripts run inside that entitled application. This folder
is a collection of client examples, not a standalone camera or model app.

Wendy injects `WENDY_DATA_STREAM_DIR`. Each configured stream has a Linux Unix
`SOCK_SEQPACKET` socket named `<stream>.sock` inside that directory. The directory
is scoped to the service; journal identities are scoped to app, service and
stream. Socket access and peer attribution remain scoped to the application.

## Send data without an SDK

```sh
printf 'model loaded' | python3 send_raw.py notes
printf '1000000000,21.5\n1001000000,21.6\n' | python3 send_raw.py temperature
```

The CSV example contains illustrative `CLOCK_BOOTTIME` nanoseconds; real senders
must use their actual sample timestamps. Its column order is `time_ns,temperature`,
with no CSV header. `timestampField` identifies the timestamp column, and
`channels` declares the remaining columns in order. Include a schema identifier
when a custom layout needs more detail. The agent retains these descriptions;
it does not parse CSV, JSON, image or custom binary payloads during recording.

One send is one record, including when the packet contains multiple samples or
newlines. There is no Wendy header, length prefix, registration handshake,
acknowledgement, or Protobuf dependency in lightweight mode. A successful send
only means the kernel accepted the message. The agent buffers it in bounded
memory and copies it into episodes selecting the `applications` source. A full
pre-roll ring evicts its oldest records and contributes to episode drop accounting.
These records can be lost on restart and are not in the independent durable journal.

The per-app limits are 200 packets per second and 16 simultaneous connections,
shared with the legacy endpoint. Batch samples to stay within the packet budget.
Idle stream connections close after five minutes and must reconnect.

The sockets accept nonempty packets up to 1 MiB, including the envelope for durable
streams. The kernel may impose a smaller packet limit through its socket buffer
configuration; `EMSGSIZE` means the record was not sent. Split time series into
smaller independently timestamped batches. Large images and video need a separate
bulk-data transfer path; this interface does not fragment packets.

## Durable records

Durable streams use the `Record` and `Ack` messages in
[`recording.proto`](../../Proto/wendy/agent/apps/v1/recording.proto). The envelope
holds an opaque `bytes payload`; its media type comes from the stream configuration.
JPEG, text, CSV, JSON and other bytes can all be enclosed without converting their
contents to Protobuf fields or JSON.

Generate Python types from the repository root with `protoc`, then install the
matching Python Protobuf runtime:

```sh
protoc -I Proto --python_out=Examples/WendyRecording \
  Proto/wendy/agent/apps/v1/recording.proto
python3 -m pip install 'protobuf>=6.33.5'
```

Use a runtime compatible with your installed `protoc` version. The lightweight
example requires neither command. Run `python3 send_durable.py` inside the app
with the sample entitlement. It sends a typed three-sample temperature batch.

Each packet receives one Protobuf acknowledgement in order:

- `COMMITTED`: the record and its agent metadata have been synced to local storage.
- `DUPLICATE`: the same ID and content are already committed in this stream's retained journal.
- `REJECTED`: the agent could not accept the record; `error` explains why.

An ID is unique within app, service and stream. Persist the original envelope if
retries must survive an application restart. Retry the same bytes and ID after an
unknown outcome; reusing an ID with changed content is rejected. Deduplication is
bounded by journal retention. These are local persistence guarantees, not cloud
upload acknowledgements or a guarantee that a campaign started successfully.

The journal works without an active episode. It retains segments for at least
24 hours after their newest record, subject to device clock accuracy across reboots.
It rotates at 4 MiB or one hour, and rejects
new records once a stream reaches 64 MiB of unexpired data. Expiration runs on
access and agent startup. It never evicts an unexpired record to make space.
Durable writes sync before acknowledgement. An I/O failure closes that journal
to new commits until recovery; no memory-only fallback is used.

Recent same-boot durable records repopulate the bounded pre-roll ring after agent
restart. Previous-boot records remain exportable but cannot be placed on a new
boot's timeline. Journal files are independent of socket lifetime and app removal.
Removing an app revokes its sockets without deleting its retained journal.

## Media types and sample timing

| Media type | Typical payload |
| --- | --- |
| `application/octet-stream` | Raw arrays or an application-defined binary layout |
| `text/plain; charset=utf-8` | Messages and annotations |
| `text/csv` | Timestamped measurement rows |
| `application/json` | Named measurements or events |
| `application/cbor` | Compact application-defined structured data |
| `application/protobuf` | Application messages or `TimeSeriesBatch` |
| `application/vnd.apache.arrow.stream` | A complete Arrow IPC stream with its schema and dictionaries |
| `image/jpeg`, `image/png` | Complete images within the packet limit |

Other concrete media types are accepted as opaque data. Declaring a media type
is not a promise of automatic plotting, indexing or transcoding. Schema identifiers
are recorded verbatim and never fetched by the agent. Preserve byte order, numeric
types, shape and layout in the schema for raw arrays.

Time-series configuration declares the clock, channels, numeric types and units.
Lightweight streams must declare `timestampField`; their payload carries the sample
timestamps. Durable envelopes can supply `SampleTiming`, either one timestamp per
sample or a start time, positive interval and count. All times are signed integer
nanoseconds. `CLOCK_BOOTTIME` timing also names the kernel boot ID. Device clock
names are preserved as supplied; this interface does not invent a device-to-boot
clock mapping.

The optional built-in schema `wendy.agent.apps.v1.TimeSeriesBatch` uses packed typed
columns. When explicitly selected, the agent validates the timing, channel types
and sample counts. Both delivery modes can use it. Envelope timing, when also
present, must agree with the batch. CSV/JSON layouts may instead declare a
`timestampField`. One batch can contain many samples, within the packet limit.

Agent receipt timestamps are always separate from sample timestamps. Uniform
sampling cannot represent missing samples implicitly: start another batch or use
explicit timestamps across a gap. An opaque payload's contents cannot trigger
campaigns. A stream may declare a fixed `event` or `model`; durable model records
can additionally carry finite uncertainty in 0..1 and up to 32 input references.
The agent uses those declared fields for existing campaign triggers and model-input
joins, without decoding the application payload.

## Retrieve records

From an authenticated operator CLI:

```sh
wendy data export-stream sh.wendy.examples.recording samples -o samples.wdr
# Multi-service apps add --service <service-name>.
python3 read_export.py samples.wdr extracted-samples
```

Export takes a bounded snapshot of the retained journal. It does not delete records
or change upload state. The CLI refuses to overwrite an existing output file.
Active episodes selecting `applications` also receive stream records in `records.wdr`;
they are checksummed and uploaded with the other episode files. The manifest's
`model_io.binary_outcome_log` identifies this additional outcome log alongside
legacy `events.jsonl`.

A `.wdr` file contains repeated frames: a four-byte big-endian Protobuf length,
a four-byte big-endian CRC32C, and a `StoredRecord`. This is an internal storage
and export format, not a header lightweight applications need to construct.
Recovery truncates an incomplete final frame; a complete frame with a checksum
mismatch fails closed for the affected journal. The supplied reader checks each
frame and writes original payload bytes to numbered files.

The original `WENDY_DATA_SOCKET` JSON endpoint remains available for compatibility.
Its `buffered` acknowledgement still describes volatile pre-roll. Use a durable
stream when acknowledgement must mean the record survived a storage sync.
