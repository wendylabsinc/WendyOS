# Camera entitlement

`{ "type": "camera" }` is the canonical V4L2/camera entitlement; `video` is its
deprecated alias. It allows character devices of major 81, bind-mounts the host
`/dev` so a camera plugged in later appears, and bind-mounts `/run/udev`
read-only so libcamera can enumerate CSI cameras.

## Node settings

Apps share each camera through a node at the camera's own device path, unless
any camera entry has exactly `"mode": "direct"`, which gives the app the real
device and leaves no node to configure.

| Key | Values |
| --- | --- |
| `format` | `"pixels"` (default): decoded pictures. `"native"`: the camera's own frames. `"h264"`: H.264, one whole frame per buffer. |
| `stamp` | `"time"` (default): the capture time. `"frame"`: the agent's frame number. |

A camera that cannot provide the `format` asked for is served in another one,
and `wendy run` says which and why.

## Allowlist

An entry's `allowlist` selects the cameras its `format` and `stamp` apply to;
without one they apply to every camera. It restricts nothing: the app still
gets a node for every shared camera. An entry cannot be empty.

Each allowlist entry names one camera by an identifier that
`wendy device camera list --json` prints for it:

| Identifier | Written as | Follows |
| --- | --- | --- |
| `stable_id`, `by-id:` | `by-id:usb-046d_HD_Pro_Webcam_C920-video-index0`, with or without `by-id:` | The camera, across reboots and replugging. Write this one. |
| `stable_id`, `by-path:` | As printed | The port the camera is plugged into. |
| `path` | `/dev/video0`, or `v4l2:/dev/video0` as a campaign file writes it | Whichever camera holds the path; a reboot or a replug can move it. |
| `name` | As printed | Every camera of the model. |

Two cameras of one model without distinct serial numbers share one `by-id:`
name: one is listed with it and the other with a `by-path:` id, and which is
which can change. Name such cameras by device path, and expect a reboot or a
replug to move the settings.

## Entries that name the same camera

Two `camera` or `video` entries that claim the same camera must agree on
`format` and `stamp`. Validation compares allowlist entries as written, and
also knows `/dev/video0` and `v4l2:/dev/video0` for one camera, and a `by-id:`
stable id with and without its prefix. Entries that name one camera by
different identifiers, such as its stable id and its path, are only found on
the device: there the stable id decides before the path and the path before
the name, and `wendy run`, the agent's log and the camera's status say which
entry decided and which was passed over.

## What `wendy run` prints

When the device's agent reports that it shares cameras, `wendy run` prints the
cameras the app shares with each node's format and stamp, then a line for each
allowlist entry that selects no shared camera's node, and for each that names a
camera that is not shared through a node, such as a network camera: such an
entry configures nothing. It prints none of this for a Compose project, on a
device that lists no camera, or when the agent does not answer within three
seconds.

An agent from before stable id and `v4l2:` entries matches only the exact
device path or name. An agent from before `format` and `stamp` reads neither of
them nor the allowlist and gives every camera pictures stamped with the capture
time. `wendy run` says so when the app asks for something the device's agent
does not read.
