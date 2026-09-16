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

The journal works without an active episode. By default it retains segments for
at least 24 hours after their newest record, subject to device clock accuracy
across reboots, and rejects new records at 64 MiB per stream. Configure durable
streams' `storage.maxBytes` to set a different limit, from 1 MiB to 1 TiB.
`storage.retentionSeconds` sets age expiry, from zero to one year. Omit it for
24 hours, or set zero to retain records until an operator acknowledges export.
The agent persists this policy so it also applies at restart before an app connects.

Segments rotate at 4 MiB, one hour, or an export checkpoint. Expiration runs on
access and agent startup. The agent never evicts an unexpired, unexported record
to make space. It also caps retained IDs at one million per stream. Batch samples
rather than assigning each sample a record. Deduplication ends when the record is
reclaimed or expires.

Durable writes sync each batch before acknowledgement. Journals use independent
locks, so a stalled journal sync does not hold the shared episode/pre-roll lock.
An I/O failure closes that journal to new commits until recovery; no memory-only
fallback is used.

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

Plain export takes a snapshot of the retained journal without deleting records or
changing cloud upload state. The CLI refuses to overwrite an existing output file.
Add `--reclaim` to export the oldest chunk and reclaim it after saving it safely.
A chunk includes whole segments until it reaches 16 MiB or 64 segments, so its
size can exceed 16 MiB by one segment. Repeat to drain a larger backlog.

The operator RPC returns an opaque checkpoint in the
`wendy-recording-checkpoint` trailer only after a successful checkpointed export.
`AcknowledgeRecordingExport` reclaims that snapshot. The CLI syncs both the output
file and its directory before sending this acknowledgement. Export alone, an
interrupted download, or a local write failure never acknowledges reclamation.
Checkpoint seals and reclamation watermarks survive agent restart. Tokens are
specific to the stream; old or repeated acknowledgements cannot delete newer data.
Concurrent exports may overlap, and a lost acknowledgement may cause duplicate
exports. Deduplicate overlapping exports by stream identity and record ID.

## Continuous vibration capture

The `vibration` example declares interleaved little-endian float32 x/y/z samples,
a 1 GiB spool, and `retentionSeconds: 0`. Each durable envelope supplies uniform
`SampleTiming` with its actual acquisition start, interval and sample count. The
agent treats the sample array as opaque bytes; only the envelope needs Protobuf.
No conversion of sensor readings into individual Protobuf fields is required.

At 25,600 samples/sec per axis, 100 ms batches contain 2,560 samples per axis,
30 KiB of payload, and require ten commits/sec. The payload rate is 307,200 bytes/sec.
At 100,000 samples/sec per axis it is 1.2 MB/sec. A 1 GiB spool holds approximately
58 minutes at the first rate, or 15 minutes at the second, before metadata overhead.
These are capacity calculations, not measured device throughput.

Keep this command running on the receiving machine:

```sh
wendy data export-stream sh.wendy.examples.recording vibration \
  --follow --reclaim --interval 1s -o ./vibration-capture
```

`--follow` creates a destination directory under an existing parent and writes
separate `.wdr` files. It exports, syncs and reclaims chunks repeatedly while the
application keeps recording. Temporary RPC connection failures retry at the chosen
interval. Restarting the command with the same directory preserves existing files;
failed acknowledgements may produce duplicate exports. Local disk failures stop
the command without acknowledging unsaved data.

The receiver must keep up and have enough disk space; this command does not rotate
or delete its local files. If it is offline long enough to fill the device spool,
new durable records receive `REJECTED` and retained records stay intact. Producers
must retry the same envelope and ID after backpressure or an unknown outcome, and
provide acquisition buffering if the sensor cannot pause. Size the spool for the
expected outage, and monitor the receiver. This is continuous operator export,
not an automatic Cloud upload service.

Use 50 to 100 ms acquisition batches as a starting point and measure commit latency on
the device's actual storage. The `BenchmarkRecordingVibration` Go benchmark covers
30 KiB, 60 KiB and 120,000-byte batches, including periodic export and reclamation.
It measures the journal path, not sensor acquisition, socket transport or networking.

## Episode copies

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
