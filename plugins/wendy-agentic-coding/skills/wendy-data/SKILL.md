---
name: wendy-data
description: Create, deploy, inspect, and troubleshoot Wendy Data campaigns, inference and watch documents, and episodes, or add application recording streams, event triggers, model predictions, and notifications. Use for Wendy data capture and retrieval; use app lifecycle guidance for ordinary logs and OpenTelemetry.
---

# Wendy Data

Wendy Data collects device-local episodes on a shared `CLOCK_BOOTTIME` timeline.
Campaigns arm event or model-uncertainty triggers and record a bounded window.
Inference documents run agent-managed detectors that emit events, and watch
documents deliver those events as notifications.
Application streams can also retain durable records outside episodes.

## Choose the available interface

Check the installed CLI's `wendy data --help` and relevant subcommand help when
a terminal is available. Use explicit `--device <selector>` on device commands.
Local CLI commands reconnect independently of an MCP session, so preserve the
connected target and transport instead of relying on the default device.

The MCP server uses `wendy_status`, `device_list`, and `device_connect`.
Inspect its actual tool list. General campaign/episode management uses the CLI,
and `wendy_tools` has no `data` group. Do not invent tool names. A skill update
does not update a running server.

Use `wendy data` when a local terminal is available. Without one, prepare the
files, explain the missing execution access, and provide exact operator
commands. App deployment tools deploy apps; they do not deploy arbitrary
campaign YAML. Do not claim a campaign was deployed from a file write or an app
deployment.

## Select the data

For CLI campaigns, discover sources with
`wendy --json data sources --device <selector>`. Choose sources that match the
requested measurements; preserve returned IDs.

| Data | Campaign selection and limits |
| --- | --- |
| Video | `camera: <selector>` for a USB, CSI, or registered IP camera. A stable ID is preferable to an ambiguous name. `camera: "*"` includes all healthy cameras and cameras discovered later. |
| Audio | `audio: <selector>` for a discovered capture device. Select the real microphone rather than an internal audio-routing channel. |
| ROS 2 | `ros2: /topic` for data already published as ROS messages, such as LiDAR, odometry, IMU, GPS, or joint state. A full per-topic source ID selects one graph; a domain source ID captures the whole domain. |
| Device health | On Linux, `telemetry: true` captures uptime, 1/5/15-minute system load, and total/free RAM at 1 Hz. It does not supply arbitrary hardware measurements. |
| Application data | Campaigns automatically include application records. Apps with `episode-write` can contribute events, predictions, images, custom bytes, and time series. |

Temperature, vibration, current, production counts, or other measurements need
a ROS publisher or an application stream. Naming a measurement in YAML does
not add a sensor adapter. Use actual sample timestamps and declare their clock
and units; receipt time alone does not establish when a measurement occurred.

## Follow the requested workflow

- For campaign YAML, inference and watch documents, one-off recording,
  deployment, notifications, and episode retrieval, read
  [campaigns.md](references/campaigns.md).
- For application events, model input references, durable records, custom
  measurements, and `export-stream`, read [application-streams.md](references/application-streams.md).
- If an app must be built or deployed, use the available `wendy-app-lifecycle`
  workflow. For missing device access, use `wendy-device-ops` or
  `wendy-device-debug`. These are separate from campaign deployment.

For an explanation or a YAML edit, complete that task without starting capture.
For a requested CLI deployment or test, verify the deployed plan and recorded
episode on the selected target. Deployment arms
the campaign immediately.

## Report what was achieved

An armed plan proves configuration, not successful capture. For CLI capture,
inspect a finalized episode to check source status, requested versus achieved pre-roll, capture
mode notes, drop accounting, trigger evidence, and payload checksums. Unknown
drops remain unknown. Continuous camera capture and application records support
pre-roll; snapshots, audio, and ROS 2 start at the trigger. Check the actual
manifest before promising history.

Keep local capture, durable acknowledgements, upload, labeling, and notification
delivery distinct. `wifi` upload currently behaves as `always`, per-campaign
`retention.local_quota` is stored but not enforced, and `export.annotation: cvat`
does not create CVAT tasks. Fleet search, replay, evaluation, and model
redeployment are not supplied by these commands.

The [data command reference](https://docs.wendy.dev/latest/advanced/clients/wendy-cli/commands/data)
contains the full CLI schema. Repository examples are
[WendyDataCampaign](https://github.com/wendylabsinc/WendyOS/tree/main/Examples/WendyDataCampaign),
[WendyDataPeople](https://github.com/wendylabsinc/WendyOS/tree/main/Examples/WendyDataPeople),
[WendyDataModelApp](https://github.com/wendylabsinc/WendyOS/tree/main/Examples/WendyDataModelApp),
and [WendyRecording](https://github.com/wendylabsinc/WendyOS/tree/main/Examples/WendyRecording).
