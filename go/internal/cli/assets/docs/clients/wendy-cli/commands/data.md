# `wendy data`

For a first recording and campaign, start with the [Wendy Data guide](/docs/data).

Wendy Data records synchronized, device-local Episodes and manages
flight-recorder campaigns. An Episode uses Linux `CLOCK_BOOTTIME` as its
canonical timeline, so wall-clock changes cannot reorder records. Its Wendy
manifest retains UTC correlation intervals, native clock mappings, source and
device identity, drop accounting, calibration revisions, lifecycle state, file
sizes, formats, and SHA-256 checksums.

### Clock status

The manifest's `system_clock_status` says what the Episode knows about the
device's own system clock, judged against the Roughtime consensus. It is one of
four values and never anything else.

| Value | Meaning |
|---|---|
| `system_reported` | No Roughtime consensus was available, so the manifest carries only the system's own reported uncertainty. |
| `agreement` | The system observation's UTC offset interval overlaps the Roughtime consensus interval. |
| `conflict` | The two intervals are disjoint: the system clock and the Roughtime consensus cannot both be right. |
| `roughtime_only` | The device has no synchronized system clock, so its observation is unbounded and there is no interval to compare. Roughtime is the Episode's only UTC evidence. This is not a disagreement; the system clock simply said nothing. |

Each Roughtime round's full evidence, including the nonce and the raw signed
responses, is appended to `roughtime.jsonl` in the Episode, which is sealed and
checksummed like every other file. The manifest keeps the bounds of the first
and most recent rounds in `roughtime_observations` and counts the rounds in
`roughtime_rounds`, so it stays a fixed size however long the Episode records.

## Episode commands

| Command | Description |
|---|---|
| `wendy data sources` | List camera, microphone, ROS 2, application, and telemetry sources, and mark the ones `record` records by default. |
| `wendy data record` | Start one detached Episode. With no source flags, every source `sources` marks with `*` is selected. |
| `wendy data stop` | Stop and seal the active Episode. |
| `wendy data status` | Show the Episodes still in progress: state, start time, duration, trigger, sources and size so far. |
| `wendy data episodes` | List finalized Episodes, with any still in progress at the top. |
| `wendy data inspect [episode]` | Print the manifest and verify every sealed payload checksum. |
| `wendy data download [episode]` | Resume a per-file download, verify it, and publish the local directory atomically. |

Use repeatable `--source` flags to narrow recording or `--exclude-source` to
remove a default source. `record` also accepts `--name`, repeatable
`--calibration <source>=<local-path>`, and
`--require-utc-uncertainty <duration>`. The uncertainty option refuses to start
when fresh UTC evidence does not meet the bound; it never affects local
monotonic ordering.

```sh
wendy data sources
wendy data record --name commissioning --source v4l2:/dev/video2
wendy data status
wendy data stop
wendy data episodes
wendy data inspect <episode-id>
wendy data download <episode-id> --output ./commissioning-episode
```

With no episode ID in an interactive terminal, `inspect` and `download` list
the device's Episodes newest first, with name, state, size and start time.
Enter selects one; Escape or Ctrl-C cancels. With no finalized Episodes on the
device they say so instead. Non-interactive, `--non-interactive` and `--json`
use requires the ID; `wendy data episodes` lists them.

`download` reports the absolute path of the directory it wrote. Without
`--output` that is a new directory named after the Episode ID in the current
directory. The `--json` result keeps `destination` as the path was given.

`wendy data stop` seals an Episode that captures application records only after
its post-seal drain has elapsed, so the command takes up to two seconds longer
than the capture itself. That wait is what lets an app file a prediction about
the samples the Episode just produced into that Episode. Campaigns set it with
`capture.drain`; an ad-hoc Episode started by `wendy data record` uses the two
second default whenever it includes the applications source, which it does
unless `--source` or `--exclude-source` leaves it out.

`sources` prints one section per kind (cameras, microphones, ROS 2, then
everything else), each row a friendly name, a status and the source ID to pass
to `--source`. A `*` marks the sources `wendy data record` records when it is
given no `--source`: every source whose status is not a reason it is unusable.
The ID is the last column, so a long ROS 2 topic or microphone ID makes one
line longer rather than the whole table wider. The clock domain and the
device's raw description are in `--json`.

```text
  CAMERAS                          STATUS     ID
* HD Pro Webcam C920               connected  v4l2:/dev/video0

  MICROPHONES                      STATUS     ID
* Built-in Audio                   detected   audio:alsa_input.platform-sound.analog-stereo
* C920 PRO HD Webcam               detected   audio:alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo

  ROS 2                            STATUS     ID
* Domain 0 (Fast DDS), all topics  reachable  ros2:rmw_fastrtps_cpp:domain-0

  OTHER                            STATUS     ID
* Application records              built in   applications
* Device telemetry                 built in   telemetry

* recorded by `wendy data record` when no --source is given
2 ROS 2 housekeeping topics hidden (/parameter_events, /rosout); they are still recorded with their domain. --all lists them.

Record some: wendy data record --source <ID> [--source <ID> ...]
More:        wendy data sources --json
```

The status says what the agent actually checked, which differs by kind:

| Status | Source | What was checked |
|---|---|---|
| `connected` | Local camera (USB, CSI, or a ROS 2 image topic's loopback) | The camera answered a capture query when it was listed. Nothing tests that it delivers a picture. |
| `online` | Network camera | It answered the last discovery probe and has a stored login. The login itself is not tried. |
| `detected` | Microphone | PipeWire, or ALSA where no PipeWire session runs, lists it as a capture device. Nothing tests that it hears anything. |
| `reachable` | ROS 2 domain | `ros2 topic list` answered on the domain. Recording the domain records all of its topics. |
| `listed` | ROS 2 topic | `ros2 topic list` lists the topic. |
| `built in` | Application records, telemetry | Nothing; they need no hardware. |

A source that is not usable shows why instead, for example `no login stored`
or `camera offline` for a network camera, `silent audio-hub channel` for an
NVIDIA Jetson audio-DMA endpoint that records digital silence, or
`topic listing failed` for a ROS 2 domain. It has no `*`: `record` leaves it out
unless `--source` names it. In `--json`, `healthy` is the same verdict and
`unusable_reason` carries the reason. An agent that predates these fields is
shown by its raw description, with `healthy` or `unhealthy` as its status.

Every ROS 2 node publishes `/parameter_events` and `/rosout` for the
middleware's own bookkeeping. The table hides those two topics; they are still
recorded with their domain. `--all` lists them, and lists every source of a
kind that has more than six. An NVIDIA Jetson without PipeWire exposes 20
internal audio-DMA channels beside its real microphone, so without `--all` the
first six sources of a kind are listed and the rest are counted in a note.
`--json` always carries every source.

Narrow the table with `--kind`, which is repeatable or comma-separated and
matches case-insensitively. A kind named there is never summarised, because
asking for a kind is asking to see all of it. Without `--kind`, `--json` stays
the full unfiltered response the device sent, so existing scripts keep working;
with `--kind` it carries the filtered set. New fields are only ever added to it.

```sh
wendy data sources --kind camera
wendy data sources --kind camera,telemetry
wendy data sources --kind audio            # every microphone, nothing summarised
wendy data sources --all                   # housekeeping topics too
```

### Microphone IDs

A microphone's ID is stable across reboots and replugs. On a device running
PipeWire it is `audio:` followed by the PipeWire node name, which WirePlumber
derives from the USB vendor, product and serial number, or the board's device
path, and the active profile. Without PipeWire it is `audio:alsa:<card>:<device>`,
using the kernel's card ID, for example `audio:alsa:C920:0`. A paired
sensor-link microphone is `audio:sensor-link:<pairing>:<sensor>`. Characters a
shell or YAML would act on become underscores.

Earlier agents listed microphones as `audio:<number>`. That number is
session-local: PipeWire reuses object numbers as devices come and go, and the
ALSA fallback encoded a card index the kernel assigns in probe order, so the
same microphone could get a different number after a reboot or a replug. The
old form is deprecated but still accepted, for now, by `record --source`,
`--exclude-source`, `--calibration` and campaign `audio:` selectors: it
resolves to the microphone that number names at that moment, `record` prints a
warning, `campaign deploy` returns one naming the stable ID, and the agent log
records each use. New Episodes record the stable ID. A number that no longer
names a microphone is refused with a pointer to `wendy data sources --kind audio`.

### The `--json` dialect

Every `data` subcommand whose payload is a protocol buffer message prints it as
canonical protobuf JSON on one compact line, with protocol buffer field names
exactly as the `.proto` declares them, and unpopulated fields omitted. That
covers `sources`, `episodes`, `record`, `stop`, `status`, `campaign list`,
`campaign trigger`, `campaign arm`, `campaign disarm` and `campaign remove`.

Some payloads are already a JSON document the device produced, and those bytes
are written through untouched so they stay checkable against the device:
`inspect` emits the episode manifest, and `campaign deploy` and
`campaign inspect` emit the canonical campaign plan. `campaign inspect` emits
the plan whether or not `--json` is given, because the plan is the whole output
of the command.

`download` reports what it wrote to the local filesystem, which no wire message
describes, so under `--json` it prints an object of its own: `episode`,
`destination`, `files` (paths relative to the destination, including the
`manifest.json` the command writes itself) and `bytes`.

Episode IDs are stable opaque identifiers. Their readable UTC prefix is only a
convenience; canonical ordering comes from the Episode's `CLOCK_BOOTTIME`
timestamps and boot ID.

### Episodes in progress

An Episode is not finalized until its seal ends, so before that it is listed
apart from the finalized ones. That covers one started by `wendy data record`
and one a campaign trigger started. It passes through up to three states:

| State | Meaning |
|---|---|
| `recording` | Capturing. |
| `draining` | Capture has stopped; it still accepts late application records for its drain. |
| `sealing` | Its playable video and checksums are being written. |

`wendy data episodes` lists these first. They add a `DURATION` column, measured
on the device's `CLOCK_BOOTTIME` and frozen once capture stops, and a `TRIGGER`
column, which reads `manual` or names the campaign. Their `SIZE` is what they
hold on disk so far.

```text
EPISODE                            STATE      SIZE     NAME           DURATION  TRIGGER
20261027T120641Z-9f2e4d6c8b0a1357  draining   2621440  door-watch     31s       campaign door-watch
20261027T120501Z-0b7c9d1e2f3a4b5c  recording  3145728  commissioning  2m13s     manual
20261027T115329Z-e4418826f638364e  complete   7245349  manual-test
```

`wendy data status` prints each of them in full, including the trigger reason
and the sources, and prints `No recording in progress.` when there is none.

```text
Episode: 20261027T120501Z-0b7c9d1e2f3a4b5c (commissioning)
State: recording
Started: 2026-10-27T12:05:01Z (device clock)
Duration: 2m13s
Trigger: manual
Sources: telemetry, v4l2:/dev/video0
Size so far: 3145728 bytes
```

Under `--json`, `episodes` keeps finalized Episodes in `episodes` and lists the
others in a separate `in_progress` array, so a script that downloads every entry
of `episodes` keeps working. Both commands' responses set `reports_in_progress`
when the agent fills `in_progress`, so an empty `in_progress` then means nothing
is in progress. `status` prints the agent's response: every Episode in
`in_progress`, and `active` for older clients.

An agent that predates `in_progress` reports at most one Episode in progress,
with no trigger, sources or size. The CLI still shows that one, estimates its
duration from the device's start time and marks it with `~`, and says so in a
note.

## Campaign commands

| Command | Description |
|---|---|
| `wendy data campaign deploy <file.yaml>` | Strictly validate, persist, and arm a campaign on the connected device. |
| `wendy data campaign list` | List deployed campaigns and revisions. |
| `wendy data campaign inspect <name>` | Print the canonical deployed plan and state. |
| `wendy data campaign trigger <name>` | Trigger a campaign manually; `--reason` is stored in the Episode. |
| `wendy data campaign disarm <name>` | Switch a campaign off without removing it. |
| `wendy data campaign arm <name>` | Switch a disarmed campaign back on. |
| `wendy data campaign remove <name>` | Stop a campaign and delete its plan, keeping its Episodes. `--force` stops and seals a recording in progress first. Alias `rm`. |

Event and model-uncertainty triggers are armed immediately after deployment.
A complete plan is in `Examples/WendyDataCampaign` in the WendyOS repository.

### Switching a campaign off, and removing it

`wendy data campaign disarm <name>` switches a campaign off on the connected
device. Its triggers stop firing, its agent inference stops, and its camera
pre-roll is released, so the camera is no longer held for it. The plan, its
revision and every Episode it recorded are kept, and `campaign list` and
`campaign inspect` show it as `disarmed`; a campaign with `inference` or event
notifications reports `inference_status.state: disarmed`. The command returns
once the inference has stopped (at most 10 seconds). A manual
`campaign trigger` of a disarmed campaign is refused.

The disarmed state is stored with the plan, so it survives agent restarts and
reboots. Redeploying a disarmed campaign stores the new plan but keeps the
campaign disarmed, and the deploy output says so: switching a campaign off is
an operator's decision on that device, and an edited plan, a teammate's
deployment or a fleet sending the plan again must not quietly switch recording
back on. `wendy data campaign arm <name>` reverses it: triggers fire again,
inference restarts and camera pre-roll is re-armed. Both commands are
idempotent.

Disarming does not cut short a recording in progress. It finishes its
`after_trigger` window and is sealed normally; the command names it.

```console
$ wendy data campaign disarm c920-yolo-live
Campaign c920-yolo-live: disarmed (revision 1a2b3c4d5e6f)
Its triggers and inference are stopped and its camera pre-roll is released. The plan and its episodes are kept.
Arm it again with: wendy data campaign arm c920-yolo-live

$ wendy data campaign list
CAMPAIGN	STATE	REVISION	FLEET
c920-yolo-live	disarmed	1a2b3c4d5e6f
```

`wendy data campaign remove <name>` stops the campaign the same way, then
deletes its stored plan, so it is no longer listed, triggered or restored at
boot. Every Episode it recorded is kept: they stay inspectable and
downloadable, and Episodes still waiting to upload follow the removed plan's
`upload.when` and `upload.max_rate`, which the device keeps for that purpose.
The Cloud app registration is also left in place for those uploads. A
campaign that is recording is refused, naming the Episode; `--force` stops and
seals that recording first, then removes the campaign. Deploying the same name
again starts a new, armed campaign.

```console
$ wendy data campaign remove c920-yolo-live
Error: campaign c920-yolo-live is recording episode 20261027T101500Z-6c1f; wait for it to be sealed, or remove the campaign with --force to stop and seal that recording first
$ wendy data campaign remove c920-yolo-live --force
Stopped and sealed its recording first: episode 20261027T101500Z-6c1f (complete).
Removed campaign c920-yolo-live (revision 1a2b3c4d5e6f).
Its episodes are kept; list them with: wendy data episodes
```

A campaign whose plan names a `fleet` is still switched off or removed on the
connected device only. Disarming is the durable choice for one device in a
fleet, because a disarmed campaign stays disarmed when the plan is deployed
again; a removed one comes back armed. Both commands print a warning for a
fleet campaign.

An agent that predates these commands is reported as needing
`wendy device update`.

### Deployment and Cloud registration

`wendy data campaign deploy [file]` validates the plan before contacting the
device. With no filename in an interactive terminal, it offers the campaign files
in the current directory: YAML files with a campaign's `version` and `name`
(compose files and stagefiles are left out). Enter selects a file; Escape or Ctrl-C
cancels. Non-interactive and `--json` use requires an explicit filename.

An enrolled device's campaign is registered in Cloud Apps as `campaign:<name>`
using a matching operator session. Registration failures stop deployment.
`--skip-cloud-registration` permits an offline deployment; unenrolled devices
skip registration automatically. Existing app metadata and notification grants
are preserved. An owner or admin must enable notifications separately.

## Campaign YAML reference

A campaign file contains exactly one YAML document. Unknown fields are
rejected, with one exception: unknown keys inside `notify` warn at deployment
and are ignored when `notify.on` is `episode_committed`. The immediate modes
(`event` and `detection`) reject unknown `notify` keys.

| Top-level field | Required | Description |
|---|---|---|
| `version` | yes | Campaign schema version the author wrote the file against. This release supports version `1`; higher versions are a deploy-time error. |
| `name` | yes | Unique device-local name using letters, numbers, `.`, `-`, or `_`; maximum 128 characters. |
| `fleet` | no | Fleet selector retained with the plan. A direct device deployment applies to the connected device, and so do `campaign disarm` and `campaign remove`. |
| `sources` | yes | One or more source entries. |
| `capture` | yes | Buffer, post-trigger duration, and triggers. |
| `upload` | yes | Upload condition, optional logical destination, and optional rate cap. |
| `retention` | no | Optional on-device storage bounds. |
| `export` | yes | Annotation integration lifecycle intent. |
| `models` | no | Map of model name to deployed version, copied into Episodes. |
| `privacy` | no | List of declared transforms with optional revisions. |
| `inference` | no | Agent-managed object detection from pinned Hugging Face models, using Transformers or YOLO ONNX; see below. |
| `notify` | no | Episode-commit notification intent or immediate event/detection delivery; see below. |

Each `sources` item selects exactly one source:

| Source | Description |
|---|---|
| `camera: <selector>` | A stable source ID, `/dev/videoN` path, or unambiguous name fragment. `front` and `default` select the only healthy camera when exactly one exists. `camera: "*"` selects every healthy camera, including cameras discovered after deployment. Quote the wildcard in YAML. |
| `audio: <selector>` | A microphone ID or unambiguous name fragment from `wendy data sources --kind audio`, for example `audio:alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo` or `C920`. The selector must resolve to a usable capture source at deployment. The old `audio:<number>` form still resolves, with a deployment warning naming the stable ID to use instead; see [Microphone IDs](#microphone-ids). |
| `ros2: <topic>` | A ROS 2 topic name such as `/lidar/points`. Selects that topic on every healthy ROS 2 graph publishing it. A full per-topic source ID such as `ros2:rmw_cyclonedds_cpp:domain-42:/lidar/points` selects that one source. A domain-level source ID such as `ros2:rmw_cyclonedds_cpp:domain-42` selects all topics in that domain. Missing topics warn at deployment; a triggered episode records available sources and reports missing sources in its manifest. Requested topics remain in the manifest. |
| `telemetry: true` | Records uptime, system load, and total/free RAM once per second on Linux. Other sensor measurements need ROS 2 or application streams. |

An optional `calibration_revision` can accompany a source. Application records
are included automatically because they carry campaign trigger evidence and
pre-roll.

### Per-source capture policy

Each source may carry an optional `capture` block declaring how it records.
Fields belonging to a different mode than the declared one are rejected.

| Field | Mode | Description |
|---|---|---|
| `mode` | | One of `continuous` (default), `snapshot`, `fragment`, or `threshold`. |
| `rate` | `continuous` | Optional capture frequency cap in hertz. |
| `interval` | `snapshot` | Required snapshot period, for example `2s`. |
| `pre`, `post` | `fragment` | Durations bounding a fragment around an occurrence; at least one is required. |
| `trigger` | `threshold` | Required expression `<field> <op> <number>`, for example `model.uncertainty > 0.9` or `level_db > -20`. Only `model.uncertainty` is bounded to 0 through 1; other fields carry their own units. |
| `fragment` | `threshold` | Optional captured duration per threshold crossing. |
| `max_resolution` | any | Camera sources only: `WxH` cap such as `1280x720`. |

What each adapter honors today: cameras implement `continuous` and `snapshot`,
audio implements `continuous` and `threshold`, and the ROS 2 and telemetry
paths implement `continuous` only. Every other combination validates and
deploys, and deployment prints a warning naming each source whose requested
mode is not implemented for its kind; those sources record continuously for
now.

Camera snapshots and the continuous capture-rate cap require a stream that
delivers whole encoded frames, currently local V4L2 cameras with native H.264.
IP cameras and GStreamer-encoded streams record continuously at their stream
rate instead; the episode's source detail reports the unapplied policy.
Snapshot cameras do not maintain a video pre-buffer. Audio threshold capture
uses `level_db`, for example `trigger: "level_db > -20"`, to retain WAV
fragments during an active episode. It does not itself start a campaign.

`capture.buffer` must be a Go-style duration from `0s` through `5m`. On a continuous
camera source, a buffer and explicit stream parameters are mutually exclusive in this
release: a buffered camera is armed into a standby subscription that asserts no
stream parameters, so it never takes a running camera away from a viewer and
never changes the stream parameters part way through a clip.

There is exactly one exception to that promise, and it belongs to the other
half of the same rule. A capture that *does* name explicit stream parameters
(`max_resolution` or `rate`, with no buffer) restarts the camera producer at
those parameters when the only consumers holding it asked for nothing in
particular. Those parameter-less consumers, a plain `wendy device camera view`
among them, have their stream ended rather than spliced onto the new stream:
the replacement producer emits a new sequence parameter set, and a decoder
handed both in one timeline produces garbage. The agent marks the ended stream
with the machine-readable reason `CAMERA_PRODUCER_RESTARTED`, and
`wendy device camera view` rejoins the replacement stream automatically,
printing one line when it does. A consumer that named its own stream
parameters is never taken over; a campaign that conflicts with it is refused
and says so in the episode manifest. A source that sets
both a buffer and an explicit `max_resolution` or `rate` therefore records both
its pre-roll and its live tail at whatever parameters the producer is already
running, and the explicit values are reported as requested but not achieved in
the episode manifest. Deployment prints a warning naming each source in that
position. Set one or the other: a buffer for pre-roll, or explicit parameters
for a clip that must be captured at a specific resolution or rate.
`capture.after_trigger` must be greater than `0s` and no more than `24h`.

`capture.drain` is the opposite end of the same idea: `buffer` decides how much
happened *before* the trigger that the Episode keeps, and `drain` decides how
long the Episode stays open for application records *after* capture stops. An
app that scores asynchronously reads samples, spends time computing, and writes
its prediction once the Episode has already finished recording. Without a drain
that write arrives too late, is acknowledged `buffered`, and is then replayed
into whichever Episode records next, where it appears as that Episode's
pre-trigger context. The drain keeps the Episode open long enough for the
verdict to be filed against the recording it was computed from.

It must be a duration from `0s` through `30s`. Omitting it takes the default of
`2s`, so campaigns get the behavior without being rewritten; `drain: 0s` opts
out and seals the moment capture stops. Every campaign Episode pays it: a
campaign plan always selects the applications source, whatever `sources`
declares, because that source is what arms the plan's triggers. A telemetry-only
or camera-only campaign therefore waits exactly as long as any other one. Only
an ad-hoc `wendy data record` Episode can avoid the wait, and only by leaving
the applications source out with `--source` or `--exclude-source`, as described
above. The wait is added
to the Episode's own lifetime, after `after_trigger` has expired, and it is
included in the `stopped_episode_nanos` the manifest records. Nothing is
captured during it, so the manifest also records
`capture_stopped_episode_nanos`, the point at which the capture adapters
actually stopped. Measure how long an Episode recorded from that field;
`stopped_episode_nanos` is the point past which no record was accepted, which
is what an event timeline needs. The difference between them is the drain.

A draining Episode has already stopped capturing, so it no longer holds its
campaign. A trigger that matches during the drain starts the campaign's next
Episode rather than being dropped. It does have to wait for the previous
Episode to finish draining before it can begin, because starting and stopping
capture are serialized on the device, so with the default the next Episode
begins up to two seconds after the trigger that asked for it. Set `drain: 0s`
on a campaign whose events arrive in bursts and whose applications write their
records live rather than asynchronously.

A failed capture adapter start is the one path that does not drain. That
Episode never delivered a sample to any application, so no record about it can
be outstanding; it is sealed as `interrupted` with reason
`capture_adapter_start_failed` and `wendy data record` returns immediately
instead of after the drain.

At least one trigger is required, and each trigger selects exactly one
condition:

| Trigger | Description |
|---|---|
| `event: <name>` | Matches an entitled application's event name. |
| `model.uncertainty: "> 0.65"` | Compares prediction uncertainty with `<`, `<=`, `==`, `>=`, or `>` and a value in `[0, 1]`. |

Prediction uncertainty is read from the structured `uncertainty` attribute
when present, otherwise from the prediction value.

The `upload` and `retention` blocks:

| Field | Required | Description |
|---|---|---|
| `upload.when` | yes | One of `always`, `wifi`, or `manual`. |
| `upload.destination` | no | Logical dataset name the fleet backend maps to storage. Not a URL; devices never receive bucket layouts or credentials through campaign plans. |
| `upload.max_rate` | no | Upload bandwidth cap in bytes per second; plain integers and rates such as `5MB/s` are accepted. |
| `retention.local_quota` | no | Declared on-device episode storage bound in bytes; plain integers and sizes such as `10GiB` are accepted. Stored with the plan; this release enforces only the device-wide quota and deployment prints a warning. |

### Inference

An `inference` block runs object detection in the agent without a separate model
application. The agent installs its managed Python runtime and downloads the
pinned Hugging Face checkpoint on first use; this requires network access and
disk space in the agent's `inference` folder, outside the episode quota. An
agent update reuses what an earlier agent downloaded. Remote model code is
disabled.

| Field | Required | Description |
|---|---|---|
| `backend` | no | `transformers` by default, or `yolo_onnx` for a compatible YOLO ONNX detection export. |
| `model` | yes | `owner/repository` or `https://huggingface.co/owner/repository`; the repository containing the checkpoint or ONNX export. |
| `model_file` | for `yolo_onnx` | Relative `.onnx` file path inside the pinned repository. Not accepted with `transformers`. |
| `accelerator` | no | `auto` by default, `cpu`, or `tensorrt`. Chooses the hardware that runs the model; see below. `tensorrt` requires `backend: yolo_onnx`. |
| `revision` | yes | Lowercase, 40-character Hugging Face commit SHA. Branch names are rejected. |
| `labels` | yes | 1–32 unique, nonempty model labels, each at most 128 bytes. |
| `threshold` | yes | Detection confidence in `(0, 1]`. |
| `rate` | yes | Frames per second per camera in `(0, 30]`. |
| `event` | yes | Event emitted on detection; 1–128 letters, numbers, `.`, `-`, or `_`. |
| `clear_after` | yes | Continuous absence needed to clear presence; positive duration, at most 24h. |
| `cooldown` | yes | Minimum interval between detection events; positive duration, at most 24h. |
| `enabled` | no | Defaults to `true`; `false` keeps the configuration but stops inference. |

Inference requires a camera source and a trigger matching its event in
`capture.triggers`. Predictions and events are attributed to
`sh.wendy.campaign.<name>`. Campaign inspect reports
`inference_status`, including runtime state, per-camera state and notification
errors. Temporary camera loss retains presence/cooldown state so reconnecting
does not produce a duplicate detection.

Each scored frame produces a `prediction` record, which an Episode that
records applications keeps in its `events.jsonl`. A frame that raises the
inference event also writes an `event` record with the same attributes, which
the device event journal keeps too.

```json
{"version":1,"type":"prediction","model":"webnn/yolo11n","attributes":{
  "campaign":"c920-yolo-live","source_id":"v4l2:/dev/video0","model_version":"<revision>",
  "width":1280,"height":720,
  "detections":[{"label":"person","score":0.9225323796272278,
    "box":[0,184.68881225585938,845.9542236328125,693.4185791015625],
    "box_norm":[0,0.25651222,0.6609017,0.96308136]}],
  "input_reference_status":"encoded_stream_decode_does_not_preserve_sample_ids",
  "runtime_results_dropped":0}, ...}
```

| Attribute | Description |
|---|---|
| `width`, `height` | Size in pixels of the picture the model scored: the decoded camera frame. |
| `detections[].box` | `[x1, y1, x2, y2]`: the top-left and bottom-right corners, in pixels of that picture. A box may reach past the picture's edges. |
| `detections[].box_norm` | The same corners clamped to the picture and divided by `width` and `height`, so every value is in `[0, 1]`. `(0, 0)` is the top-left corner of the top-left pixel and `(1, 1)` the bottom-right corner of the bottom-right pixel; x grows to the right and y down. Omitted for a box with no area inside the picture. |

To draw a box on the picture at any other size (a thumbnail, a phone, a
half-scale stream), multiply `box_norm` by the displayed width and height.
Records written by agents before `box_norm` existed have no `width`, `height`
or `box_norm`; `box` there is in pixels of the camera's own resolution, which
the record does not state. Readers that do not know the new fields ignore them.

#### Accelerators

With `accelerator: auto`, a `yolo_onnx` model runs on an NVIDIA Jetson's GPU
through TensorRT when the device reports an NVIDIA GPU with CUDA and has the
TensorRT libraries; every other device, and the `transformers` backend, runs
on the CPU. `cpu` never uses an accelerator. `tensorrt` requires TensorRT and
never scores on the CPU: if TensorRT is unavailable the campaign reports
`error` with the reason and retries.

- **First use.** The agent downloads NVIDIA's Python bindings for the device's
  TensorRT version once (about 0.85 MB, pinned by SHA-256) and caches them in
  its inference directory. The TensorRT and CUDA libraries come from the
  WendyOS image.
- **The engine.** The device builds a TensorRT engine for the model in the
  background, at FP16; for `yolo11n` on a Jetson Orin Nano this took 11 to 14
  minutes and about 2.2 GB of memory. Under `auto` the model keeps detecting
  on the CPU meanwhile. The engine is cached and reused across restarts and
  reboots, with no network, until the model, TensorRT version or GPU changes.
- **When a build starts.** Under `auto`, the first build waits a random delay
  of up to 20 minutes (`engine.state: scheduled`, with `not_before`), so that
  devices updated together do not all build at once. Every build then waits
  until 3 GiB of memory is available (`waiting_for_memory`), and until at
  least 2 CPUs are idle (`waiting_for_cpu`), checking again every 2 minutes.
  The campaign's own CPU inference counts as idle, since the engine replaces
  it. The wait for memory has no limit, and the campaign reports itself
  degraded while it lasts. The wait for the CPU lasts at most 30 minutes
  (until `engine.build_by`) and is not degraded: after it, the build starts
  anyway at the lowest priority (nice 19 and the idle I/O class,
  `engine.low_priority: true`), so a device kept busy by continuous camera
  capture still gets its engine. Either wait shows `waiting_since` and
  `elapsed_seconds`. A `tensorrt` campaign scores nothing until its engine is
  ready, so it skips the delay.
- **Memory before the engine.** The model process loads TensorRT and CUDA
  only when an engine is about to load. Until then, through the delay and the
  waits, it uses about as much memory as one with no TensorRT; a short-lived
  child process reads what the engine cache needs from TensorRT and the GPU.
- **The switch.** Before replacing the CPU, the engine must find the same
  detections as the CPU on the same camera frames: same labels, IoU of at
  least 0.8, and scores within 0.05.
- **Fallback.** If the bindings cannot be fetched, the build fails, the engine
  disagrees with the CPU, or the accelerated path needs more than 1.5 GiB of
  memory (TensorRT's libraries, the CUDA context and the engine together), an
  `auto` campaign stays on the CPU. It is never silent: on a device with a
  usable GPU, `inference_status.accelerator.degraded` is `true` with a
  `reason`, and the agent logs a warning. A failed build is not retried for
  24 hours unless the model changes.
- **A worker that keeps crashing.** If the model process exits three times
  within 10 minutes while it has TensorRT, without being asked to and without
  reporting an error of its own, the model gets no TensorRT for an hour: an
  `auto` campaign detects on the CPU, degraded, and a `tensorrt` campaign
  reports `error`. Either way the reason carries the end of the last
  process's stderr. Whenever the model process exits, `inference_status.error`
  and the agent log say why, with the end of its stderr.

`inference_status.accelerator` reports what actually runs: `active` (`cpu` or
`tensorrt`), `degraded` and `reason`, `runtime` and `precision`,
`gpu_memory_mib` (what the accelerated path took, measured as described
under Fallback), and `engine` with its `state` (`checking`, `scheduled` with
`not_before`, `waiting_for_memory` with `waiting_since`, `waiting_for_cpu`
with `waiting_since` and `build_by`, `queued`, `building` with `started_at`,
`verifying`, `ready` or `failed` with an `error`) and any `warnings` TensorRT
printed while loading it. A build or a wait also reports `elapsed_seconds`,
and a build that stopped waiting for idle CPUs reports `low_priority: true`.
`inference_status.stats` reports `fps`, inference latency (`infer_ms_p50`,
`infer_ms_p95`) and `frames_scored`, the frames the model process has scored
since it started. Each `prediction` record carries `accelerator`, `precision`,
`infer_ms` and `model_sha256`, because FP16 TensorRT scores differ slightly
from FP32 CPU scores.

The agent keeps what TensorRT needs in its inference directory: `accel/` holds
the bindings, `engines/` the built engines (about 9 MB each for `yolo11n`),
and `cuda-cache/` CUDA's cache of GPU code it compiled, which spares the next
start compiling it again (about 172 MB with TensorRT on an Orin Nano, and
never more than 512 MiB).

See [the all-camera people example](https://github.com/wendylabsinc/WendyOS/tree/main/Examples/WendyDataPeople)
for a complete campaign. Campaign revision hashing now uses revision schema 2,
which includes inference and notification settings. Existing plans receive new
revision hashes when redeployed; the YAML `version` remains `1`. An absent
`accelerator` is left out of the hash, so campaigns deployed before the field
existed keep their revisions and are not re-armed by an agent update. Writing
`accelerator: auto` explicitly is a new revision, as an explicit
`backend: transformers` is, although it behaves the same as leaving it out.

### Notifications

| Field | Required | Description |
|---|---|---|
| `notify.on` | yes | `episode_committed`, `detection`, or `event`. |
| `notify.event` | for `event` | Exact named application or inference event to match. |
| `notify.webhook` | no | For `event`/`detection`, an HTTP(S) endpoint without embedded credentials or a URL fragment. Omit it to deliver through Wendy Cloud. |

`episode_committed` is cloud-side intent carried in the committed manifest;
it does not open a device-side notification connection. `detection` requires
an inference block and sends an immediate notification when presence is detected.
`event` also works for matching application events, independently of episode
capture triggers. Immediate notifications use a bounded queue and retries;
delivery failures appear in `inference_status.notification_error` and agent logs.
For Cloud delivery, enroll the device and enable the campaign's notification
grant in Cloud Apps. Deployment registers `campaign:<name>` but never grants
permission to send notifications. Permanent authentication, authorization,
validation and protocol errors stop retries; transient failures get at most
three attempts, each with a 10-second timeout and the same event UUID.

A `detection` notification carries `detections`: up to five `{label, score}`
pairs that passed the label and threshold filter, highest score first; `count`
stays the total. They appear in the device's notification journal and in
webhook bodies; Wendy Cloud notifications do not carry them yet. A
`notify.on: event` rule naming the inference's own event carries them too. It
never carries boxes or images. Readers that predate the field ignore it.

Unknown keys inside `notify` are rejected for `event` and `detection`.
For `episode_committed`, they warn at deployment and are ignored.

```yaml
version: 1
name: forklift-failures
fleet: warehouse-west

sources:
  - camera: front
    capture:
      mode: snapshot
      interval: 2s
      max_resolution: 1280x720
  - ros2: /lidar/points
  - ros2: /vehicle/odometry

capture:
  buffer: 10s
  drain: 2s
  after_trigger: 20s
  triggers:
    - event: emergency_stop
    - model.uncertainty: "> 0.65"

upload:
  when: wifi
  destination: forklift-episodes
  max_rate: 5MB/s

retention:
  local_quota: 10GiB

export:
  annotation: cvat

notify:
  on: episode_committed
```

## Playing back camera capture

Camera capture is kept as a raw H.264 Annex-B elementary stream
(`cameras/<source>/segment-NNNNNN.h264`). An elementary stream has no
container, so it carries no timing at all, and a player handed one has nothing
to build a presentation schedule from. Players invent a frame rate instead, so
the clip runs at the wrong speed and reports the wrong duration. Passing a
fixed rate with `ffmpeg -r` does not fix this: the capture rate is genuinely
variable, falling as the device gets busy with inference, so no single number,
including the measured average, describes more than a fraction of the clip.

The timing is already recorded. `cameras/<source>/index.jsonl` carries one line
per kept frame with `canonical_episode_nanos`, the frame's position on the
Episode's canonical `CLOCK_BOOTTIME` timeline, alongside the segment file, byte
offset and `byte_size` of its payload. Remuxing those payload bytes into an MP4
gives every frame the presentation time the index recorded for it, so the
variable frame rate is represented honestly rather than averaged away.

### Sealed Episodes already carry the MP4

The agent runs that remux itself while sealing an Episode: each camera source
gains a `cameras/<source>/playable.mp4` beside its raw capture, listed in the
manifest with a size and SHA-256 like every other file and marked with
`"role": "derived"`. Because it is listed, the transfer worker uploads it and
commit-time verification covers it, so the bucket object plays directly in a
browser (or after a plain download) with no conversion step. The derived file
is never capture payload: model-input and payload-retention accounting
resolve payloads through `index.jsonl` into the raw segments, and the raw
files are kept byte-for-byte as before. The remux is a copy, not a transcode,
so expect the clip to add roughly the size of the raw stream to the Episode;
that overhead counts against the Episode's size for the local quota.

Sealing never fails or waits on the remux. A source whose stream cannot become
an honestly timed, seekable clip (B slices, slice headers the muxer cannot
parse, parameter sets that change mid-stream after a producer restart, no
random-access frame) seals without its `playable.mp4`, and the
manifest's `playable_notes` names the reason; a clip that had to omit frames
whose bytes were missing gets a note too.

### Converting older Episodes by hand

The `episode-playable` command performs the same remux laptop-side, for
Episodes sealed before the agent did it (or for a source whose seal-time mux
was refused, where it will report the same warnings). When every camera source
already carries a seal-time `playable.mp4` the command says so and converts
nothing:

```sh
# From the repository root.
CGO_ENABLED=0 go build -o bin/episode-playable ./go/cmd/episode-playable

wendy data download <episode-id> -o ./episode --device <device-hostname>
./bin/episode-playable -o ./playable ./episode
```

`wendy data download` takes either an absolute or a relative `-o` path. A
trailing separator is fine too: the destination is cleaned before the staging
directory beside it is named.

One `<source>.mp4` is written per camera source into the output directory,
which must be somewhere other than the Episode. The command prints the index
line count, the frames written, the segments read, the index's first-to-last
span, and the minimum, mean and maximum inter-frame interval, which is enough
to check the conversion numerically without watching it.

Details worth knowing:

- The Episode is only ever read. The elementary stream and its index are
  checksummed archival truth, and `index.jsonl` addresses frames by byte offset
  within the stream, so rewriting it in place would break the join between a
  frame and the model input recorded against it.
- It is a remux, not a transcode. The coded pictures are copied verbatim, so
  the output is the same video, only re-framed.
- MP4 was chosen over Matroska because the output has to play unmodified in
  both VLC and QuickTime, and QuickTime cannot open Matroska. MP4 states an
  explicit duration for every sample in its `stts` box, which represents a
  variable frame rate exactly as precisely as Matroska timestamps do.
- There is no external dependency. The muxer is pure Go, so no ffmpeg,
  mkvmerge or PyAV is needed anywhere on the path from an Episode to a playable
  file.
- A frame the index names but whose bytes are missing or truncated is reported
  and skipped rather than written short, because a truncated access unit makes
  the file undecodable from that point on. A partial trailing index line, the
  shape an interrupted Episode leaves behind, is counted as unusable and
  ignored. Multiple segments per source and multiple sources per Episode are
  both handled.
- `byte_size` from the index is authoritative for how many bytes belong to a
  frame. The model-input ledger's `payload_bytes` can exceed it for a frame
  that opens a segment, because the segment begins at the parameter-set prefix
  inside that payload rather than at its first byte.
- The index records frames in capture order, which is coded order. That is
  exact for the B-frame-free streams the device's encoder produces. A stream
  containing B slices has a presentation order that differs from its coded
  order, which the index does not carry, so such a stream is flagged with a
  warning rather than silently mistimed.

`wendy data export` is the natural future home for this, at which point the
command becomes a verb on the CLI rather than a separate binary.

## Pre-trigger behavior

Application pre-roll is exact, device-wide, and bounded by the campaign buffer
(with a five-minute/50 MiB global ceiling). Continuous camera sources also keep
pre-roll: a campaign that requests a buffer arms them into a standby ring of
keyframe-aligned encoded frames, so the episode can include footage before the
trigger. The camera source's manifest entry reports the achieved offset, which
may be shorter than requested when the campaign was recently armed or the
ring's byte cap was reached. Snapshot cameras do not maintain this video ring.
Audio, ROS 2, and telemetry start at the trigger. Every source's manifest entry
preserves both the requested offset and the achieved offset; campaign deployment prints a warning
when audio or ROS 2 pre-roll was requested. Unknown drop counts are displayed
as unknown, never as zero.

A record the ring replays into an Episode is written with
`preroll_flushed: true` in `events.jsonl`. Records the Episode observed live
omit the key entirely, so the two are distinguishable offline without changing
any existing line. Window membership is decided on both the agent's own receipt time and the
record's presentation timestamp: a record reaches an Episode's pre-roll only if
it physically arrived inside that window, so a client cannot place itself into
a later Episode by choosing its own `client_boottime_nanos`.

## Concurrency and retention

Each campaign records at most one active Episode at a time; a second trigger
for the same campaign while its Episode is active is dropped. Different
campaigns capture concurrently, and one ad-hoc `wendy data record` Episode can
run beside them. `wendy data stop` finalizes the ad-hoc Episode when one is
running, otherwise the single active campaign Episode.

When the device storage quota is exceeded, the oldest Episodes that are
already uploaded or were recorded without an upload policy are evicted first.
Episodes still awaiting upload are evicted only as a last resort, with a
warning in the agent log naming the Episode and campaign.

The device-wide quota is the smaller of a fifth of the episode store's
filesystem and `WENDY_DATA_MAX_BYTES` (default 50 GiB), and eviction preserves
`WENDY_DATA_RESERVE_BYTES` of free space on it (default 5 GiB). Both are read
by the agent from its environment file.

On WendyOS the episode store is `/var/lib/wendy/wendy-data/episodes`, on the
`/data` partition. Where `/var/lib/wendy` is not a separate filesystem larger
than the one holding `/var/lib/wendy-agent`, the store stays at
`/var/lib/wendy-agent/data/episodes`. Campaign inference keeps its runtime and
model files in an `inference` folder beside the store, except on a device that
already ran campaign inference under an earlier agent: that device keeps using
`/var/lib/wendy-agent/data/inference`, so the update downloads nothing. The
agent logs `Wendy Data episode store` at startup with the paths it chose and
why. Set `WENDY_DATA_DIR` only to put the store somewhere else.

When there is no room, `wendy data record` and campaign triggers fail with a
message that names the disk, its free space and the space Wendy Data keeps free:

```text
Wendy Data can't record: the disk holding /var/lib/wendy-agent/data/episodes has 4.1 GB free and needs 5.4 GB free. Point WENDY_DATA_DIR at a folder on a bigger disk (e.g. under /data) or lower WENDY_DATA_RESERVE_BYTES.
```

## Current layer boundaries

- Upload policy: `upload.when` and `upload.max_rate` are applied by the agent's
  episode transfer worker, which uploads sealed episodes to the cloud ingest
  service. `wifi` is currently treated as `always` because no device
  network-type signal is plumbed to the worker yet. `upload.destination` is a
  logical dataset name stored with the plan; the device never receives bucket
  layouts or credentials.
- Per-source capture modes other than `continuous` are validated and stored,
  with camera `snapshot` and audio `threshold` implemented. Other combinations
  record continuously and deployment warns per source. Camera snapshot and
  rate policies also depend on the transport, as described above.
- `retention.local_quota` is validated and stored, but eviction currently
  applies only the device-wide quota; deployment warns when it is set.
- `export.annotation` is stored as labeling lifecycle state, but this release
  does not create CVAT tasks.
- `privacy` declares transform metadata; it does not apply redaction to captured
  payloads.
- Fleet catalog search, replay, evaluation, and model redeployment are later
  Wendy Data layers and are not performed by these commands yet.

Episodes remain locally inspectable and downloadable regardless of network or
UTC confidence.

## Application recording streams

The `episode-write` entitlement can declare named streams with `mode` set to
`lightweight` or `durable`, plus a `mediaType` and an optional schema. Lightweight
streams accept native bytes over Unix `SOCK_SEQPACKET` sockets without an SDK.
Durable streams accept Protobuf envelopes and acknowledge after local storage sync,
including when no episode is active. The agent injects `WENDY_DATA_STREAM_DIR`;
each stream is available at `<directory>/<stream>.sock`.

Supported payloads include text, CSV, JSON, CBOR, Protobuf, Arrow IPC, JPEG, PNG
and custom binary layouts. Time-series streams declare their channels and clock;
sample timestamps remain separate from agent receipt time. See the repository's
`Examples/WendyRecording` for configuration, senders and an export reader.

```sh
wendy data export-stream <app-id> <stream> --service <service-name> -o records.wdr
```

Omit `--service` for a single-service app with no explicit service name. Export
writes a snapshot of retained durable records and refuses to overwrite an existing
file. Plain export does not delete records, change cloud upload state or include
volatile lightweight records. `--reclaim` exports the oldest chunk, syncs its local
file and directory, then acknowledges that checkpoint for device reclamation.
Chunks contain whole segments up to 16 MiB plus one segment, or 64 segments.

For continuous capture, use an output directory under an existing parent:

```sh
wendy data export-stream <app-id> <stream> --follow --reclaim --interval 1s -o capture
```

This writes separate files, retries temporary RPC failures and reclaims only saved
chunks. Local storage failures stop export. A lost acknowledgement or overlapping
exports can produce duplicates; use record IDs and stream identity to deduplicate.
Existing destination files are preserved. The receiver must keep up and manage its
own storage; the CLI never deletes local capture files.

Durable stream configuration accepts `storage.maxBytes` from 1 MiB to 1 TiB and
`storage.retentionSeconds` from 0 to 31536000. Defaults remain 64 MiB and 24 hours.
Zero retention disables age expiry, retaining records until export acknowledgement.
Set a larger spool, such as 1 GiB, for continuous capture. A full journal rejects
new data without overwriting retained records; producers must buffer and retry.
Deduplication applies only while records are retained, with at most one million
retained IDs per stream. Age retention across reboots depends on device clock accuracy.
This export path does not update cloud upload state.
Payloads and metadata are exported in checksummed Protobuf `.wdr` frames. Episodes
also include these frames in `records.wdr`, alongside legacy `events.jsonl`.
