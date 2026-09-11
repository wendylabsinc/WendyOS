# Wendy Data Platform demo runbook

How to reproduce the end-to-end Wendy Data Platform demo on a real device from
a clean start: an NVIDIA Jetson running a WendyOS agent built from the data
platform branch, the reference model app (`Examples/WendyDataModelApp`), a
flight-recorder campaign, and episode uploads into a deployed cloud ingest
service.

The path this runbook walks has been executed on real hardware. An episode
fires on the app's edge-triggered `person_detected` event, or on demand with
`data campaign trigger`, carries the app's YOLOv8n prediction records in
`events.jsonl` (including the pre-trigger buffer), and reaches the ingest
catalog with upload state `uploaded`, which only flips after `CommitEpisode`
verifies every file hash server-side.

Placeholders used throughout, to be replaced with your own values:

| Placeholder | What it is |
|---|---|
| `<device-hostname>` | The device's mDNS name, for example `wendyos-<name>.local` |
| `<org-id>` | The organization the device is enrolled in |
| `<asset-id>` | The device's asset identifier in that organization |
| `<your-gcp-project>` | The Google Cloud project hosting the ingest service |
| `<ingest-url>` | The ingest service URL the device should upload to |
| `<episode-id>` | An episode identifier, as printed by `wendy data episodes` |

## 0. Record the device's starting state

Before changing anything, write down what you are about to change, so you can
put it back. The demo touches the agent binary, one environment file, and the
auto-updater timer; it does not touch enrollment.

| Fact | Why it matters |
|---|---|
| Hostname | Every `wendy` command below takes `--device <device-hostname>` |
| Hardware and WendyOS version | Determines which agent build to install |
| Enrollment (organization and asset) | Stays as-is; the demo redirects uploads only |
| Camera device nodes | The app and the campaign compete for these, see step 5 |
| Agent version before the demo | The rollback target |
| Free space on `/` and on the data partition | Episodes are large; see step 4 |

## 1. Prerequisites on the workstation

- Go toolchain. On macOS, cgo builds need `CC=/usr/bin/clang`; if `clang`
  resolves to a Swift toolchain shim, cgo fails with a silent
  "exit status 2".
- Docker for the app image build. If a pull hangs on macOS, the Docker Desktop
  credential store is the usual cause: point `DOCKER_CONFIG` at a scratch
  directory containing a `config.json` of `{}`.
- The Google Cloud command-line interface (`gcloud`) if you need to look up the
  ingest URL. If it crashes under an old system Python, set `CLOUDSDK_PYTHON`
  to a newer interpreter.

## 2. Recon (read-only)

```sh
wendy device list --timeout 8s          # no --scan flag; --timeout scans once
wendy device info --device <device-hostname>
ssh root@<device-hostname> 'df -h /; ls /dev/video*'
```

Check `df` before anything else. A device whose root filesystem is full will
fail the agent install in confusing ways. The failure mode seen in practice:
the daily agent updater accumulates one backup of the agent binary per run in
`/opt/wendy/bin` (`wendy-agent.backup.*`), fills the root partition, and then a
failing update leaves `/usr/local/bin/wendy-agent` truncated and the agent
dead. Recovery:

```sh
ssh root@<device-hostname> '
  rm -f /opt/wendy/bin/wendy-agent.backup.*
  cp /opt/wendy/bin/wendy-agent.latest /usr/local/bin/wendy-agent
  systemctl restart wendyos-agent'
```

## 3. Build the agent and command-line interface (CLI)

```sh
cd <wendyos-checkout>/go

# Agent: pure Go, cross-compiles from macOS directly (CGO_ENABLED=0).
make build-agent-linux-arm64 VERSION=dev-data-platform-demo

# CLI: needs cgo (gousb, Bluetooth Low Energy); the darwin Makefile target's
# CGO_ENABLED=0 does not link, so build it directly:
CC=/usr/bin/clang CGO_ENABLED=1 go build \
    -ldflags "-s -w -X github.com/wendylabsinc/wendy/go/internal/shared/version.Version=dev-data-platform" \
    -o bin/wendy-demo ./cmd/wendy
./bin/wendy-demo data --help   # confirm the data verbs exist in this build
```

## 4. Install the agent on the device

Stage a rollback first, on the device, so recovery does not depend on the
workstation:

```sh
ssh root@<device-hostname> '
  mkdir -p /data/demo-rollback
  cp /usr/local/bin/wendy-agent /data/demo-rollback/wendy-agent.pre-demo'
```

Then push with the sanctioned update path and verify:

```sh
./bin/wendy-demo device update --binary bin/wendy-agent-linux-arm64 --device <device-hostname>
./bin/wendy-demo device info --device <device-hostname>    # "version" shows the new build
./bin/wendy-demo data sources --device <device-hostname>   # v2 DataService answers; camera healthy
```

The nightly auto-updater would overwrite this agent shortly after midnight, so
disable it for the demo window and re-enable it afterwards:

```sh
ssh root@<device-hostname> 'systemctl disable --now wendyos-agent-updater.timer'
# undo after the demo:
ssh root@<device-hostname> 'systemctl enable --now wendyos-agent-updater.timer'
```

## 5. Point the data plane at the ingest service

Enrollment stays untouched; only the episode transfer worker is redirected. The
agent's systemd unit reads `/etc/default/wendy-agent` (`EnvironmentFile=`), so:

```sh
INGEST_URL=ingest.data.wendy.sh

ssh root@<device-hostname> "cat >> /etc/default/wendy-agent <<EOF
WENDY_DATA_INGEST_URL=$INGEST_URL
WENDY_DATA_DIR=/data/wendy-agent/episodes
EOF
mkdir -p /data/wendy-agent/episodes && systemctl restart wendyos-agent"
```

- `WENDY_DATA_INGEST_URL` is the DataIngestService endpoint the transfer worker
  dials, `ingest.data.wendy.sh` for the dev platform. It is required: the
  enrolled cloud host does not serve this service, so there is no fallback, and
  an agent without it logs once that uploads are disabled and keeps sealed
  episodes queued. Identity is the enrolled asset certificate, presented in the
  mutual Transport Layer Security (mTLS) handshake and read by the service from
  the validated certificate; the worker attaches no identity header.
- `WENDY_DATA_DIR` moves the episode store off the root partition (default
  `/var/lib/wendy-agent/data/episodes`) onto the larger data partition. Worth
  doing on any device whose root partition is a few gigabytes.
- `WENDY_DATA_MAX_BYTES` and `WENDY_DATA_RESERVE_BYTES` bound that store. The
  enforced quota is the smaller of a fifth of the store's filesystem and
  `WENDY_DATA_MAX_BYTES` (default 50 GiB), and eviction preserves
  `WENDY_DATA_RESERVE_BYTES` of free space (default 5 GiB). Both take a plain
  byte count; a value the agent cannot parse is logged and the default is kept.

Verify the override took: the agent logs
`data transfer worker: ingest endpoint override set` at startup.

## 6. Deploy the reference app and campaign

```sh
cd <wendyos-checkout>/Examples/WendyDataModelApp
<wendyos-checkout>/go/bin/wendy-demo run --device <device-hostname> --detach --yes
<wendyos-checkout>/go/bin/wendy-demo data campaign deploy campaign.yaml --device <device-hostname>
<wendyos-checkout>/go/bin/wendy-demo data campaign trigger model-harness-demo --device <device-hostname>
```

### One camera, two consumers

`campaign.yaml` declares a `camera: front` source and the app consumes the same
camera, on a single-camera device, at the same time. That works because the app
never opens the physical device: it reads the agent-fed v4l2loopback node that
the `camera` entitlement grants, while the campaign's capture adapter reads the
same producer. Video4Linux2 admits one holder of a capture device, and the agent
is it.

Earlier revisions of this demo needed a `campaign-telemetry-only.yaml` variant
because the app opened `/dev/video0` itself and the capture adapter then failed
with `VIDIOC_S_FMT: device busy`. That variant is gone; `campaign.yaml` is the
only campaign the example needs.

Beyond removing the conflict, the subscription is what makes an episode usable
as training data: each sample the app receives carries a `sample_id`, the
episode records the same identifier for the bytes it kept, and each prediction
names the samples it was computed from. See "What to expect in the episode".

### Verify, in order

```sh
./bin/wendy-demo data episodes --device <device-hostname>            # newest episode "complete"
./bin/wendy-demo data inspect <episode-id> --device <device-hostname>
# trigger.reason, files [events.jsonl, telemetry.jsonl], upload.state "uploaded"
```

`upload.state: "uploaded"` is the cloud-side proof: the worker only records it
after `CommitEpisode` succeeds, and the server verifies every stored object's
SHA-256 against the manifest before flipping the catalog row to complete.
Read-side inspection of the catalog (`QueryEpisodes` and `GetEpisode`) requires
a user bearer token rather than the asset identity, so check the rows directly
in the deployment's ClickHouse (`episodes` and `episode_files` tables) or
through the business intelligence surface once it is wired.

## What to expect in the episode

`person_detected` is the only trigger. The app emits it edge-triggered when a
person enters frame, once per appearance rather than once per frame, so walking
through produces one episode rather than fifty; with nobody in view, seal one on
demand with `data campaign trigger`. Each trigger seals one episode, 10 seconds
of pre-trigger buffer and 10 seconds after, which uploads within seconds. The
app holds itself to a ceiling of 5 predictions per second, but what it achieves
is set by inference, not by the ceiling: measured nearer three per second
sustained, so a 20-second episode carries on the order of sixty prediction
records. Quote the ceiling as a ceiling.

Earlier revisions of this runbook told you to expect an episode to fire by
itself on a `model.uncertainty > 0.65` trigger. That trigger is gone from
`campaign.yaml`, and the advice was wrong rather than merely stale: the app
scores uncertainty as 1 minus its best detection confidence, so a scene the
detector has no class for sits at exactly 1.0 and the threshold fires on every
prediction. Measured on the device, that produced a fresh episode every 30
seconds with nobody in frame. Collecting on uncertainty is still the right idea;
it needs a real uncertainty signal behind it, entropy across classes or the
margin between the top two, rather than one minus a confidence.

To pair those outcomes with the frames that produced them:

- `model_inputs.jsonl` lists every sample the harness handed to the model,
  as `{source_id, sample_id, episode_nanos, payload_bytes, dropped_before}`.
- `cameras/<source>/index.jsonl` carries the same `sample_id` on each frame the
  episode kept, together with the segment file and byte offset holding it.
- each `prediction` record in `events.jsonl` carries an `inputs` list of
  `{source_id, sample_id}`.

Join on `(source_id, sample_id)` and every prediction resolves to the exact
bytes the model saw. The manifest's `model_io` block states this contract,
counts the predictions that named their inputs, and — per source — whether the
episode retains payloads for all of the consumed samples or only the subset the
capture policy kept.

To watch the camera capture rather than join against it, open the
`cameras/<source>/playable.mp4` the episode already carries: the agent remuxes
each camera source into a browser-playable MP4 while sealing, lists it in the
manifest as a derived file, and uploads it with everything else. Only an
episode sealed by an older agent (or a source whose seal-time remux was
refused, see the manifest's `playable_notes`) still needs the laptop-side
`episode-playable` conversion; the raw capture is an elementary stream with no
container, so players otherwise invent a frame rate and the clip runs at the
wrong speed. See "Playing back camera capture" in the `wendy data` command
documentation.

## Pitfalls discovered on the way (all hit for real)

1. Full root disk from accumulated updater backups: see the recon step.
2. `Dockerfile` export stage: ultralytics pulls the graphical
   `opencv-python` wheel, which needs `libxcb1 libgl1 libglib2.0-0`, and
   torch 2.13 and newer needs `onnxscript` for Open Neural Network Exchange
   (ONNX) export. Both are handled in the example's Dockerfile.
3. App data socket refused with "peer is not in a wendy app scope": on a device
   whose containerd uses the cgroupfs driver the `system.slice:edge-agent:<app>`
   cgroup path is literal, which the attribution parser must recognize.
   Without that, no record ever reaches the agent.
4. After any agent restart the app's data socket listener is gone and the app
   gets ECONNREFUSED until the app is redeployed
   (`device apps remove --force`, then `wendy run`). Cause: containerd's
   metadata store drops labels with empty values, so type-only entitlements
   (camera, episode-write) lost their `sh.wendy/entitlement.*` labels and
   `RestoreAppSystemAPISockets` skipped the container. Fixed: `wendyLabels`
   now writes a `present=true` marker for type-only entitlements. Containers
   created by an older agent still carry the broken labels, so after updating
   the agent redeploy the app once.
5. An episode that exhausts its 5 upload attempts is terminally `failed` and is
   never retried; trigger a fresh episode after fixing connectivity.
6. `wendy device apps start` streams logs and does not return with `--detach`
   semantics; script around it or use `wendy run`.

## Rollback

```sh
ssh root@<device-hostname> '
  cp /data/demo-rollback/wendy-agent.pre-demo /usr/local/bin/wendy-agent
  chmod 755 /usr/local/bin/wendy-agent
  sed -i "/WENDY_DATA_INGEST_URL/d;/WENDY_DATA_DIR/d" /etc/default/wendy-agent
  systemctl restart wendyos-agent
  systemctl enable --now wendyos-agent-updater.timer'
```

Enrollment is untouched by the demo, so there is nothing to restore there.

## Open gaps

- Catalog read verification from the CLI needs a user bearer token; today the
  proof is the commit-gated `uploaded` state plus a direct ClickHouse read.
- The graphics processing unit (GPU) variant of the app, using
  `onnxruntime-gpu`, has not been exercised; the demo runs the CPU path.
- The app sensor socket authenticates a caller by the app-private mount, the
  group-2000 gate and the 0750 directory only. It performs no
  peer-credential check, so it does not yet have the `SO_PEERCRED` plus cgroup
  attribution the app data socket gained; a process in group 2000 that can
  reach another app's sensor socket path would be served, and that app's
  entitlement allowlist is what would be applied. The fix is the data socket's
  `verifyPeer` behind an accept-time wrapper on the sensor listener.
- ROS 2 sources report `healthy: true` unconditionally
  (`data_ros2_adapter.go`, `ros2DataSource`): nothing probes the Data
  Distribution Service (DDS) graph, so a dead domain enumerates as healthy.
  Camera and audio sources derive their health; ROS 2 does not yet.
- ROS 2 capture treats "rosbag2 has not exited within 750 ms" as "rosbag2 is
  recording" (`data_ros2_adapter.go`, `startOne`). Nothing confirms the bag
  directory was created or that a topic was subscribed, so a recorder that
  fails just after that window reports a successful start and surfaces the
  failure only at Stop.
- The audio capture now backdates a segment stamp past the chunk of pulse-code
  modulation (PCM) data it read and past a constant for the delay still
  outstanding behind it, and publishes a 20 ms uncertainty half-width to cover
  that constant (`mapping_segment` reads `receipt-minus-pipeline-v1`). The
  constant is an estimate, not a reading: nothing queries the actual ALSA or
  PipeWire delay per chunk, so a device whose buffering departs markedly from
  the Jetson Orin Nano the constants were measured on could fall outside the
  published bound.
- `wendy data download` mutates its own `--output` flag variable when the flag
  is empty, so re-running the same command object in one process writes to the
  first episode's directory.
