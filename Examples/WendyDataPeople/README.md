# Agent-managed people detection

Deploy [campaign.yaml](campaign.yaml) to run a people detector on **every camera
feed wendy-agent knows about**. The agent owns the model runtime, camera
subscriptions, detection events, episode capture and notifications. Users supply
only YAML: no application, Python environment or continuously running CLI.

With no file argument, the CLI opens a picker for `.yaml` and `.yml` files in the
current directory. Scripts and `--json` invocations must pass a file explicitly.

```sh
wendy --device <device> data campaign deploy campaign.yaml
wendy --device <device> data campaign inspect people-all-cameras
```

The agent downloads its checksum-pinned runtime, locked Python dependencies and
the pinned Hugging Face checkpoint on first use. This requires internet access,
several GB of free space and enough RAM for the model. Subsequent starts reuse
the cache in the agent's data partition. This initial backend supports CPU
inference on 64-bit Linux (x86_64 and aarch64) and Apple Silicon macOS; its Linux
wheels require glibc 2.28 or newer. GPU execution is not enabled by this example.

`inference_status` in campaign inspect (or the plan JSON from list) reports `pending`, `loading`,
`running`, `waiting_for_cameras`, `error` or `disabled`, with per-camera health and
notification errors. A successful deploy persists and arms the plan; it does not
claim model loading has already succeeded. Failed loads retry, and an agent
restart automatically restores deployed inference plans.

`camera: "*"` selects all currently healthy cameras, including USB/V4L2, CSI and
registered IP cameras. The agent reconciles inventory every five seconds and
reconnects interrupted subscriptions. Offline cameras and cameras missing
credentials remain visible in status and join when usable. For an IP camera,
configure credentials with `wendy camera login <id>` as usual. A stream with no
samples for 30 seconds is reopened. The model reads the same camera producers as
viewers and episode capture; it never opens a second physical camera reader.

The default model is
[facebook/detr-resnet-50](https://huggingface.co/facebook/detr-resnet-50), trained
for object detection. Change `inference.model` to a repository name or its Hugging
Face URL and pin `inference.revision` to a 40-character repository commit SHA.
The backend accepts safetensors checkpoints supported by Transformers'
`AutoModelForObjectDetection` and an image processor with
`post_process_object_detection`. Remote model code is disabled. Labels must
exist in the checkpoint; a mismatch is reported as a model-loading error.

One shared model handles the campaign's cameras in turn, scoring each camera's
latest decoded frame at up to `rate` frames per second. Throughput depends on
compute and camera count. Old frames are discarded, so this is sampled detection;
brief appearances between scored frames can be missed. Encoded-stream overflow
resets the affected decoder instead of feeding it a corrupted splice.

An appearance of `person` at the configured threshold emits `person_detected`,
starts an episode of the campaign's cameras, and applies its notification policy. A person
remaining in view generates one event per camera. `clear_after` of observed
empty frames rearms detection, and `cooldown` limits repeated appearances. A
stream outage does not count as an empty scene. Cameras detecting people during
an existing episode share that recording; immediate webhook mode can send an
alert for each camera without opening duplicate recordings. A new camera joins subsequent episodes; an episode in progress keeps
its original source set.

The example uses `notify.on: event` with `event: person_detected` to send an
immediate Wendy Cloud notification to organization owners and admins. It uses
`campaign:people-all-cameras` as its Cloud notification source and the device's
enrollment credentials. No application container or webhook is required.

On a Cloud-enrolled device, `wendy data campaign deploy` registers
`campaign:people-all-cameras` and its device assignment before arming the plan.
It appears in Cloud Apps immediately, without waiting for a detection. Open
Apps → people-all-cameras → Wendy Notifications and enable notification sending
as an organization owner or admin. Events emitted while the grant is disabled
are rejected and are not replayed after enabling it. The grant covers this named
campaign across assigned devices in the organization; each CLI deployment
registers its target device without changing an existing grant or stopped state.

The CLI uses your saved operator session for the device's enrolled Cloud host
and organization. Run `wendy auth login` first. Unenrolled devices can deploy
locally; `--skip-cloud-registration` also allows an explicit offline deployment
on an enrolled device. Rerun without the flag to register once connected. A failed
registration stops deployment; a later device failure may leave its registered
Cloud entry. `wendy run` performs the same registration for ordinary apps using
their `wendy.json` app ID, including Compose and multi-service deployments.

The Cloud server must include `AppService.RegisterAppDeployment` (WDY-2939).
Older CLI deployments can still register campaigns on their first signed event,
with sending disabled.
This agent branch uses the v1 notification API served by Cloud main; Cloud dev's
v2 API requires an agent with the matching API and enrollment identity contract.
Cloud failures are visible in logs and `inference_status.notification_error`.

`notify.on: episode_committed` is still available as manifest-carried intent for
a separate ingestion service. It does not send an immediate notification, and
Cloud main/dev do not implement an episode-ingestion notification consumer here.

For direct delivery to your own notification service, add a webhook:

```yaml
notify:
  on: event
  event: person_detected
  webhook: https://your-notification-service.example/hooks/people
```

`notify.event` matches the event name exactly. This works for agent model events
and application events, independently of capture triggers or an active recording.
Use `upload: {when: manual}` to keep episodes local without automatic uploads.
`on: detection` remains available for all appearances from the campaign's model.

The agent POSTs JSON containing `id`, `event`, `campaign`, `source_id`, `model`,
`model_revision`, and `count`. Configure an endpoint that accepts this format and
delivers your notification, returning a 2xx response. The URL is stored in the
campaign plan; credentials in userinfo and URL fragments are rejected. Redirects
are not followed. The model worker never receives the webhook URL.

There are three bounded attempts using the same `Idempotency-Key` header; the
receiver should deduplicate that key. Failed attempts and queue overflow are
visible in agent logs and, for enabled model campaigns,
`inference_status.notification_error`. Webhook delivery
has no persistent outbox. Recording failures do not suppress webhook alerts, and
webhook failures do not stop detection.

Cloud delivery uses the same bounded queue and event UUID. Permission failures and
`ALREADY_EXISTS` stop retries; transient failures retain bounded retries. Cloud
registration never grants notification permission automatically.

Episodes contain prediction and detection records in `events.jsonl` and the
model revision in the manifest. The input ledger records the encoded samples
delivered to the model process. The first backend decodes opaque H.264/WebM
streams without an exact decoded-frame-to-sample mapping, so prediction records
explicitly report `input_reference_status` and do not invent input sample IDs.
Those outcomes are counted as having unknown inputs by the existing manifest
checks. Models execute locally; images are not sent to Hugging Face or included
in notification metadata. Captured episodes follow the YAML's upload policy.

```sh
wendy --device <device> data episodes
wendy --device <device> data inspect <episode-id>
```

To stop inference, set `inference.enabled: false` and redeploy. Redeployment
cancels the old worker and model subscriptions; results from replaced revisions cannot
start new episodes. An already-triggered recording finishes its configured
window. Cached runtimes and weights are retained outside episode retention quotas.
