# SensorLink camera identity

The supervisor owns the mapping from an authenticated source principal and
channel to its current V4L2 node. Node numbers are allocated in connection order;
they are not persistent camera identities. Kernel loopback nodes and their card
labels can survive an agent restart. The camera inventory therefore takes the
name, stable identity, and channel liveness from one supervisor snapshot, rather
than combining a retained kernel label with a new owner. Unowned nodes are hidden.
A channel is online after a frame is successfully written in its current stream
attempt; disconnect clears it. This is not a frame-age or freshness guarantee.

Wendy Data publishes these cameras as `sensorlink:<source-key>:<channel>`.
Use the exact ID returned by Data source discovery in a campaign's `camera:`
selector. Recording, standby pre-roll, and model subscriptions resolve that
identity when joining or rejoining a camera. A missing identity fails rather
than falling back to the old node or a display-name match.

Existing `v4l2:/dev/videoN` selectors and `/dev/videoN` campaign selectors still
resolve through the current node alias. **They cannot prove which physical
camera was originally intended.** Deployment and campaign resolution warn when
using such a SensorLink selector. Verify the camera, then explicitly redeploy
with its stable ID. Existing stored campaigns are not silently rewritten.
Direct V4L2 consumers still see kernel card labels and must not treat them or
node numbers as durable identities.

An unpaired channel's node reservation lasts until the agent exits, even if its
kernel node can be removed. In-process feed subscribers can outlive removal;
reassigning their path to another source would deliver the wrong camera. The
same source/channel can reclaim its reservation. The 96–127 band permits 32
unique source/channel reservations per agent lifetime; exhausting it fails new
mounts until restart. Kernel nodes are never recreated solely to refresh a name.

Get/Set/Reset camera controls reject SensorLink virtual nodes: local loopback
controls are not remote physical-camera controls. This does not add remote
SensorLink tuning or stable-control selectors for USB/CSI cameras. No protobuf
fields change; the existing camera StableId and Data string IDs carry identity.
