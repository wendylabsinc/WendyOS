# Agent-managed object search

Deploy [campaign.yaml](campaign.yaml) to search one camera's frames for two
objects described as formats rather than trained classes: a red can, by shape,
size and colour, and a bottle, by a three-part composition and its size. The
agent owns the perception worker, the camera and depth subscriptions, scoring,
tracking, events, episode capture and notifications. Users supply only YAML: no
application, model checkpoint, Python environment or continuously running
command line interface (CLI).

This is the world view feature. The schema, every field and the record format
are described in the
[campaign YAML reference](../../go/internal/cli/assets/docs/clients/wendy-cli/commands/data.md#world-view-objects).

## Before deploying

Three values in the YAML are placeholders for your hardware:

* `sources[0].camera` is the colour camera. Use its identifier exactly as
  `wendy --device <device> data sources --kind camera` lists it. The example
  names one camera rather than `camera: "*"` because depth pairs only with a
  campaign that resolves to exactly one camera; with more, the depth source is
  reported as unavailable.
* `depth.source` is the depth stream, also as `wendy data sources` lists it.
  The agent passes this value to the camera service unchanged, so a name
  fragment or an alias such as `front` is not accepted here.
  `wendy --device <device> device hardware list --category camera` marks a
  video node that offers Z16 depth frames with `depth=z16`.
* `depth.intrinsics` holds placeholder values for an Intel RealSense D435-class
  camera at 848x480. The comment in the YAML says how to measure your own.

## Deploy and inspect

```sh
wendy --device <device> data campaign deploy campaign.yaml
wendy --device <device> data campaign inspect objects-can-and-bottle
```

With no file argument, the CLI opens a picker for `.yaml` and `.yml` files in the
current directory. Scripts and `--json` invocations must pass a file explicitly.

The agent runs one perception worker per campaign in the same managed Python
runtime as `inference`. On first use it installs that checksum-pinned runtime
and the worker's locked dependencies (PyAV, NumPy and headless OpenCV); no model
checkpoint is downloaded. The runtime exists for 64-bit Linux (x86_64 and
aarch64) and Apple Silicon macOS; on any other platform `objects_status` reports
`error`.

`objects_status` in campaign inspect is the live state of the search, never part
of the plan or its revision:

* `pending`: the plan is armed and the agent has not started its job yet.
* `loading`: the worker is starting, including the first-use runtime install.
* `running`: at least one camera is being searched.
* `waiting_for_cameras`: the worker is up but no selected camera is available;
  `objects_status.error` carries the reason when the camera selector did not
  resolve.
* `error`: the worker failed or cannot run here; `objects_status.error` says
  why, and the agent retries with a backoff from 5 seconds up to one minute.
* `disabled`: every object has `enabled: false`.

`objects_status.sources` has one entry per camera and one for the depth source.
A camera reads `connecting`, `streaming` or `scoring` (results are arriving),
`reconnecting: <reason>` after an interruption, or `unavailable: <detail>`. The
depth source reads `depth streaming` while depth is paired, and
`depth unavailable: <reason>` otherwise. `objects_status.notification_error`
says why the latest `coke_can_seen` notification failed, including a full
notification queue, and clears when a notification is delivered. Because
the campaign sets `notify.on: event`, inspect also shows an `inference_status`
block for the agent's event notification job, although the campaign declares no
`inference`; it reads `running` while that job is up.

## What the campaign looks for

`coke_can` is a cylinder 6 to 7 centimetres wide and 11 to 13 tall with a red
palette: `[53, 80, 67]` in the International Commission on Illumination (CIE)
L\*a\*b\* colour space (CIELAB) covering 60 percent of the region and a white or
silver `[80, 0, 0]` covering 25 percent. It is scored 4 times a second per
camera and must reach a fused confidence of 0.75. `shape` is required: a
cylinder casts a rectangle or a disc, any other outline scores at most 0.25 on
shape, and that is below the default veto floor of 0.5. A new sighting on a
camera emits `coke_can_seen`, which starts an episode unless one is already
recording and sends a notification. A sighting not matched for 3 seconds
(`clear_after`) is lost, and a new sighting within 20 seconds (`cooldown`) of
the last `coke_can_seen` on the same camera stays silent until the cooldown has
passed.

`bottle` is a cylinder body, a cone shoulder and a cylinder neck, listed bottom
to top, with a `size` for the whole bottle of 5 to 6 centimetres wide and 24 to
30 tall. It is scored every fourth frame and matches at a fused confidence of
0.7, which emits `bottle_seen`. With a `composition`, the composition is scored
as the `shape` attribute and the `shape` attribute's own `expect` is not read;
declaring `shape` is what gives the composition a weight. Without it, the
composition score is reported under `unweighted` and does not move the
confidence. The capture trigger `object.bottle.confidence: "> 0.8"` starts an
episode from any bottle prediction record above 0.8 except `lost`; see the next
section for why it does not fire in this release.

Because the two objects use different cadence forms, the worker samples each
camera at 7.5 frames per second, the larger of `coke_can`'s rate of 4 and 30
divided by `bottle`'s `every_frames` of 4. Each object is then scored only at its
own cadence.

## Size, depth and what that means for the bottle

`size` measures metric extent, which a camera frame alone cannot give. Without
a working `depth` source the `size` attribute is unavailable, which is not the
same as scoring zero: fusion renormalises the weights over the attributes that
are available, so an unavailable attribute neither raises nor lowers the
confidence. In the records, `size` is listed in `unavailable` and missing from
`scores`, `depth_paired` is `false`, and `position` and `size_m` are absent.
`objects_status.sources` shows the depth source as
`depth unavailable: <reason>`.

Which cameras can serve as `depth.source`, in the words of the reference: "A
depth source that cannot be opened, or that does not deliver `z16` raw frames,
is reported there and the search runs without it." In this release the camera
service delivers camera samples to the world view as encoded video (H.264 or
VP8), never as `z16` raw frames, so no camera is paired as a depth source yet
and `size` is always unavailable. The `depth` block is included so the example
is complete once a raw depth producer exists; until then expect
`depth unavailable: samples are "h264", not z16 raw frames with known dimensions`
or a similar reason.

That leaves `coke_can` scored on shape and colour, with weights 1 and 1.5, which
can still reach 0.75. It leaves `bottle` scored on its composition alone, and
the worker segments a bottle as a single region, which the agent matches
against one part: the first part whose solid can cast the region's outline.
Without depth that is the body for a rectangle or disc outline, and the score is
the body's share of the expected height, about 0.74; a trapezoid matches the
shoulder and scores about 0.22, and an `other` outline matches nothing and
scores 0. So `bottle_seen` can fire at the 0.7 threshold, but the
`object.bottle.confidence: "> 0.8"` trigger cannot be reached without depth.
With depth, the single region measures as the whole bottle, which is taller
than the body's range, so the composition scores below its ceiling of 0.74:
about 0.07 for a 27 centimetre bottle, for a confidence of about 0.53, and no
height in the bottle's `size` range brings the confidence to 0.74. The trigger
is in the example to show the second trigger form; treat it as unreachable in
this release.

## The proposer

The worker's default and only proposer, `contours`, is a simple contour
segmenter meant to prove the pipeline end to end, not a strong detector. It
finds edges on the lightness and both colour channels of a frame scaled to at
most 640 pixels on its longer side, keeps up to 50 external contours covering at
least 0.2 percent of the frame, classifies each outline as `rect`, `disc`,
`trapezoid` or `other`, and measures a palette of up to three colours by k-means.
It never classifies objects. Because it keeps only outer outlines, objects that
touch in the image merge into one candidate, and an object whose edges have
little contrast against the background can be missed.

## Records in the episode

Episodes carry the world view's records in `events.jsonl`, attributed to
`sh.wendy.campaign.objects-can-and-bottle`. Each tracked sighting produces
prediction records from model `worldview` when it `appeared`, reached a new
`peak` confidence, `moved`, or was `lost` after `clear_after`. Their attributes
are:

* `campaign`, `object`, `track_id` and `kind` (`appeared`, `peak`, `moved` or
  `lost`)
* `confidence`, the fused confidence
* `scores`, per available attribute
* `unavailable` and `unweighted`, lists of attribute names
* `vetoed`, only when a required attribute vetoed the candidate
* `bbox`, `[x, y, w, h]` in pixels, and `frame`, with `w` and `h`
* `source_id`, `sample_id` and `boot_nanos`
* `position` (`distance_m`, `bearing_deg`) and `size_m` (`w`, `h`), only when
  the candidate was measured against a paired depth frame
* `requested_rate` for `coke_can`, `requested_every_frames` for `bottle`
* `achieved_fps`, the worker's rate for that camera, and `object_achieved_fps`,
  the object's own scoring rate
* `depth_paired`
* `model_version`, the campaign revision

A `lost` record carries the box and scores of the last match and never starts
an episode. On `appeared` the object's event is also recorded as an event
record, with `object`, `track_id`, `confidence` and `source_id`.

```sh
wendy --device <device> data episodes
wendy --device <device> data inspect <episode-id>
```

## Notifications

`notify.on: event` with `event: coke_can_seen` sends an immediate Wendy Cloud
notification each time the can appears, under the Cloud notification source
`campaign:objects-can-and-bottle`. On a Cloud-enrolled device,
`wendy data campaign deploy` registers that entry in Cloud Apps with sending
disabled; an organization owner or admin must enable its notification grant
before events deliver. Events emitted while the grant is disabled are rejected
and are not replayed. To deliver to your own endpoint instead, add
`notify.webhook`. The [people example](../WendyDataPeople/README.md) describes
registration, the webhook payload and retries in full; the world view uses the
same delivery path.

## How to author a palette

Photograph the object under the lighting the camera will see, pick a pixel or a
small area of each main colour with any image tool that reports CIELAB, and
write each as `{lab: [L, a, b], share: s}`, where `s` is roughly the fraction of
the object's visible surface that colour covers. Shares must sum to at most 1,
and at most 8 entries are accepted. The worker measures at most three colours
per candidate, so a palette of more than three entries is matched against
only three measured colours. The agent compares
colours with the CIE 1976 colour difference (CIE76 delta E), the straight-line
distance in CIELAB, and an entry stops matching at the attribute's `tolerance`,
25 by default. `[53, 80, 67]` is pure red in the standard red, green and blue
(sRGB) colour space, rounded. A printed can under real light will measure
differently, so measure your own rather than keep this value.

## Stopping

Set `enabled: false` on an object to stop searching for it, or on both to stop
the worker, and redeploy. Redeployment replaces the job; records from the old
revision do not start new episodes.

## Not yet verified on hardware

This example has run only against the repository's tests: they parse this file
with the agent's campaign parser and check that its triggers match the records
the world view writes. It has not been deployed to a device, the descriptors
have not been tuned against real cans or bottles, and the depth placeholder has
not been checked against a RealSense. Do not take it as a tuned configuration.
