# Campaigns and episodes

## Author a campaign

A file contains one YAML document. Version 1 requires `name`, `sources`,
`capture`, `upload`, and `export`. Unknown fields are rejected except for legacy
`notify.on: episode_committed` extension keys, which warn and are ignored.

This small plan records telemetry and application records. Add discovered
camera, audio, or ROS 2 sources to match the task.

```yaml
version: 1
name: incident-review
sources:
  - telemetry: true
capture:
  buffer: 10s
  drain: 2s
  after_trigger: 20s
  triggers:
    - event: incident
upload:
  when: manual
export:
  annotation: cvat
```

There is no `applications` YAML source key; campaigns include it automatically.
At least one trigger is required even when the operator will trigger manually.
An event trigger matches an application's or agent inference's named event.
The other trigger form is `model.uncertainty: "> 0.65"`, with a comparison
against a value in `[0, 1]`. An app must actually publish uncertainty. Do not
derive its meaning from the field name or assume all models calibrate it alike.

Before changing a deployed name, inspect its existing plan so the edit retains
unrelated sources, triggers, and policies. `fleet` is metadata on a direct
device deployment; it does not fan deployment out to every fleet member.

## Capture constraints

- `buffer` accepts `0s` through `5m`. Application pre-roll has a device-wide
  five-minute/50 MiB ceiling. Continuous camera capture uses a standby
  encoded-frame ring; startup or unavailable cameras can shorten pre-roll.
  Snapshot cameras, audio, and ROS 2 have no pre-roll.
- `after_trigger` must be positive and at most `24h`. `drain` accepts `0s`
  through `30s`, defaults to `2s`, and accepts late app results after sensor
  capture stops. It extends episode lifetime, not sensor recording duration.
- Camera `continuous` and `snapshot` modes are implemented. Snapshot requires
  `interval`, for example `2s`, and a transport that delivers whole access
  units. Byte-chunk streams record continuously and explain this in the
  manifest. Rate caps also depend on transport and can be best effort at GOP
  granularity. A snapshot is not guaranteed to be a JPEG.
- Camera `max_resolution: 1280x720` and continuous `rate` request stream
  parameters. Buffered capture keeps the existing producer parameters and
  reports these requests as unachieved. For an exact resolution/rate request,
  use `buffer: 0s` and inspect the achieved parameters. Unsupported modes or
  conflicts with an existing parameter-specific consumer remain visible.
- Audio supports `continuous` and `threshold`, such as
  `trigger: "level_db > -20"` and `fragment: 30s`. ROS 2 and telemetry support
  `continuous`. Other mode/kind combinations validate, warn, and record
  continuously. A validated policy does not prove the adapter honored it.
- A ROS topic selector records that topic on healthy graphs publishing it.
  A missing ROS selector warns at deployment; capture records the other sources
  and marks it absent in the manifest. Missing camera/audio selectors can fail
  trigger resolution, and audio availability is checked at deployment. Inspect
  availability before reporting that a campaign can collect its sources.

Each campaign has at most one active capture. A trigger during that capture is
dropped; different campaigns can record concurrently. A trigger during the
post-capture drain can start the next episode after the previous drain finishes.

## Deploy and verify

Use the existing authorized target. The commands below show the sequence for a
requested deployment and test, not a required action for every YAML edit.

```sh
wendy --json data sources --device <selector>
wendy --json data campaign list --device <selector>
wendy data campaign deploy ./campaign.yaml --device <selector>
wendy data campaign inspect incident-review --device <selector>
wendy --json data campaign trigger incident-review --reason verification --device <selector>
wendy --json data episodes --device <selector>
wendy data inspect <episode-id> --device <selector>
wendy data download <episode-id> --output ./incident-episode --device <selector>
```

`deploy` strictly validates, persists, and arms the plan. It is not a dry-run
validator. Use a filename to avoid the interactive picker. Capture the returned
revision and any warnings. Inspect returns the canonical plan even without
`--json`. An enrolled device's deployment registers `campaign:<name>` in Cloud
Apps, using the matching operator session. Registration failures stop deployment;
`--skip-cloud-registration` supports an intended offline deployment.

Keep the ID returned by `trigger`. Allow `after_trigger` plus `drain` to finish,
then inspect that episode instead of assuming the newest episode belongs to
the test. Until it is finalized, `wendy --json data status --device <selector>`
lists it in `in_progress` as `recording`, `draining` or `sealing`. `inspect` verifies sealed payload checksums. `download` resumes per
file, verifies checksums, and publishes the completed local directory atomically.
Use a destination that preserves existing local data.

For one-off capture, select returned source IDs explicitly when only a subset
is wanted. With no source flags, `record` selects every healthy source: the
table marks them `*`, and in `--json` an unhealthy source's `unusable_reason`
says why it is left out.

Microphone IDs are stable: `audio:<PipeWire node name>`, or
`audio:alsa:<card>:<device>` without PipeWire. The old `audio:<number>` form
still resolves, with a deprecation warning, in `--source`, `--exclude-source`,
`--calibration` and campaign `audio:` selectors. It can name a different
microphone after a reboot or replug, so do not write it into a plan.

```sh
wendy --json data record --name commissioning --source <source-id> --device <selector>
wendy --json data status --device <selector>
wendy --json data stop --device <selector>
```

`status` reports every episode not yet finalized, with its duration, trigger,
sources, and size so far. `episodes` lists those in its separate `in_progress`
array and keeps `episodes` finalized-only. An agent without `reports_in_progress`
in its `status` response reports at most one, in `active`.

`stop` finalizes the ad-hoc episode when present, otherwise the single active
campaign episode. It does not disarm a campaign. Do not use it as general
cleanup when another capture could be active. The campaign CLI has `deploy`,
`list`, `inspect`, `trigger`, `arm`, `disarm`, and `remove`; do not invent
stop, delete, or replay subcommands.

## Switch a campaign off, or remove it

```sh
wendy data campaign disarm incident-review --device <selector>
wendy data campaign arm incident-review --device <selector>
wendy data campaign remove incident-review --device <selector>
```

- `disarm` keeps the plan, revision, and episodes, but stops the campaign's
  triggers and agent inference and releases its camera pre-roll, so the camera
  is no longer held for it. `list` and `inspect` show `disarmed`, and
  `inference_status.state` is `disarmed`. Manual `trigger` is refused. A
  recording in progress finishes its `after_trigger` window and seals normally.
- The state survives agent restarts and reboots. Redeploying a disarmed
  campaign stores the new plan and keeps it disarmed; only `arm` switches it
  back on. Setting `inference.enabled: false` stops only the model, so prefer
  `disarm` to switch a campaign off.
- `remove` deletes the plan and keeps every episode, which still upload under
  the removed plan's upload policy. It refuses while the campaign is recording
  and names the episode; `--force` stops and seals that recording first. Do not
  pass `--force` unless the user accepts ending that recording early.
- These act on the connected device only, even when the plan names a `fleet`.
  To keep a fleet campaign off on one device, disarm it: a removed campaign
  returns, armed, if the plan is deployed again.
- An agent that predates these commands is reported as needing
  `wendy device update`.

Check the manifest's trigger, campaign revision, source identities and status,
requested/achieved pre-roll, capture-policy notes, drops, clock evidence,
`model_io`, files, and upload state. `CLOCK_BOOTTIME` orders records within a
boot; UTC confidence is a separate claim. Prefer the recorded camera index
timing or a derived `playable.mp4` over guessing the raw H.264 frame rate.
Some streams cannot produce a playable MP4; `playable_notes` records why.

## Managed inference and notifications

Use `inference` for agent-managed object detection when the user wants detection
without a separate model app. Preserve or obtain a real pinned model revision;
do not invent a SHA. This block uses the repository's people-detection example:

```yaml
inference:
  model: facebook/detr-resnet-50
  revision: 1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b
  labels: [person]
  threshold: 0.9
  rate: 1
  event: person_detected
  clear_after: 5s
  cooldown: 30s
  enabled: true
```

Inference requires camera sources and a `capture.triggers` event matching
`inference.event`, here `event: person_detected`. The default backend is `transformers`.
`backend: yolo_onnx` instead requires `model_file` to name a relative `.onnx`
file in the pinned Hugging Face repository. First use installs a managed runtime
and downloads model files, requiring network access and storage outside the
episode quota. Remote model code is disabled. Check `inference_status`, its
per-camera states, and actual events after deployment.

Each scored frame produces a `prediction` record for `events.jsonl`. Its
`detections[].box` is `[x1, y1, x2, y2]` in pixels of the scored picture, and
`detections[].box_norm` is the same corners clamped to the picture and
normalised to `[0, 1]`. The record's `width` and `height` give the picture's
size. Draw from `box_norm` when the picture is shown at another size. Older
records have only `box` and no picture size.

`accelerator` chooses the hardware: `auto` (the default), `cpu`, or `tensorrt`.
On a Jetson, `auto` runs a `yolo_onnx` model through TensorRT on the GPU, and
every other device or backend runs on the CPU. The first TensorRT start builds
an engine in the background, which takes minutes and about 2.2 GB of memory;
`auto` keeps detecting on the CPU meanwhile and switches once the engine
matches the CPU's detections. Under `auto` the build first waits a random
delay of up to 20 minutes, and every build waits for free memory and for 2
idle CPUs, so `engine.state` may read `scheduled`, `waiting_for_memory` or
`waiting_for_cpu` for a while. A busy CPU delays the build by at most 30
minutes (`engine.build_by`); then it builds at the lowest priority.
`waiting_for_cpu` is not degraded. `tensorrt` never runs on the CPU and requires
`backend: yolo_onnx`. Check `inference_status.accelerator`: `active` is what
runs, `engine.state` is the build, and `degraded: true` with a `reason` means a
usable GPU is not being used. Do not promise GPU speed until `active` is
`tensorrt`.

`notify` chooses one delivery condition:

```yaml
notify:
  on: event
  event: person_detected
```

- `event` matches the named app or inference event independently of capture
  triggers. `detection` requires an inference block.
- For `event` or `detection`, an optional `webhook` selects an HTTP(S) endpoint
  without embedded credentials or a fragment. Omitting it uses Wendy Cloud.
- `episode_committed` carries cloud-side intent in the committed manifest. It
  does not open a device-side notification connection.

Cloud delivery needs an enrolled device and the campaign's notification grant
enabled in Cloud Apps. Deployment registers the campaign but does not enable
that grant. Respect the requested recipient and existing grants; authoring a
notification block does not establish delivery. Check
`inference_status.notification_error`, agent logs, and delivery evidence for
the intended endpoint when a notification test is requested.

## Upload and retention

`upload.when` accepts `always`, `wifi`, and `manual`; `wifi` currently behaves
as `always`. Do not promise Wi-Fi-only transfer. `destination` is a logical
dataset name, not a bucket URL. `max_rate` is a byte rate, such as `5MB/s`.
Local capture and download remain usable without network access.

`retention.local_quota` is declared intent. The device-wide episode quota is
enforced instead, and deployment warns. It is the smaller of 20% of the store's
filesystem and `WENDY_DATA_MAX_BYTES`, default 50 GiB, while reserving
`WENDY_DATA_RESERVE_BYTES`, default 5 GiB. Unuploaded episodes can be evicted as
a last resort. `export.annotation: cvat` records labeling intent only.

On WendyOS the store is `/var/lib/wendy/wendy-data/episodes`, on the `/data`
partition; it stays on the root disk only where `/var/lib/wendy` is not a
separate, larger filesystem. Do not tell users to set `WENDY_DATA_DIR` unless
they want a custom location. A "Wendy Data can't record" error names the disk,
its free space and the reserve; read the agent's `Wendy Data episode store` log
line before changing anything.
