# Linux digital twin

This first-party Wendy app owns the room database, scan merging, people tracks,
Go2 localization, rotation searches, route planning, movement loop and watchdogs.
macOS and iOS connect through the selected gateway's authenticated app transport.
They render snapshots, contribute scans and camera observations, and send user
intent. There is no fallback to a locally running robot engine.

The app binds `127.0.0.1:8791`. Its `http` entitlement lets the gateway expose it
through authenticated app forwarding. Do not publish this port directly to a LAN.
The `admin` entitlement gives this trusted app access to the agent's local gRPC
socket through `WENDY_AGENT_SOCKET`. The `persist` entitlement mounts the named
`dev.wendy.digital-twin` volume at `/data`. Executables stay in the image.

## Deploy

Prepare a standalone build context from this checkout, then deploy that directory
with the Wendy CLI or the MCP `run` tool:

```sh
./go/apps/digital-twin/prepare.sh /tmp/wendy-digital-twin-project
wendy json validate /tmp/wendy-digital-twin-project/wendy.json
wendy run --prefix /tmp/wendy-digital-twin-project --device DEVICE --detach --wait-ready
```

The Dockerfile uses the repository's Go version and builds a static Linux binary.
The container uses root to access the agent's admin socket and the agent-created
persistent volume. No host GPU or device-node entitlement is needed. ROS producers
run in their existing robot environment and send observations to the twin API.

Open Sites on the same gateway in Wendy for macOS or iOS. On first connection,
existing local captures are copied into the Linux store, preserving IDs and
retaining the originals. Already present captures are not overwritten. Removed
captures stay removed. The client keeps only disposable render assets and node
calibration locally. A service outage is shown as an error, not a new empty room.

## Start with a robot

A Go2 already paired with this Linux gateway can start a space without an Apple
app. Run on the gateway or use an authenticated forwarded endpoint:

```sh
python3 start_go2_room.py GO2_SERIAL
```

This creates a new room at the current odometry origin. It does not command a
journey or rotation. Opening the robot retains the existing control preparation,
which sends Stop and configures onboard avoidance. After creation, the helper
releases control; the Linux observation subscription keeps collecting the map.
Opening a new operator session replaces the old robot connection. Alignment must
be confirmed again before movement.

For G1 or a Go2 using ROS 2, source the robot's ROS environment and run:

```sh
python3 ros_contributor.py --source g1:warehouse --topic /lidar/points --frame map
```

Use the actual PointCloud2 topic and SLAM frame published by the robot. The adapter
uses the capture-time TF transform, rejects stale clouds and creates a room for
this SLAM epoch. It publishes no motion commands. Its default endpoint is the
local Linux twin. A robot on another host needs an authenticated app tunnel and
`--endpoint` pointing to that tunnel.

To add to an existing room, provide its ID and an explicit registration:

```sh
python3 ros_contributor.py --source g1:warehouse --topic /lidar/points --frame map \
  --room ROOM_UUID --room-from-ros X Y Z YAW_RADIANS
```

Coordinates are metres, right-handed and Y-up. The adapter first converts ROS
X/Y/Z to twin X/Z/-Y, then applies the supplied translation and Y-axis yaw. Start a
new room or supply a new measured registration after a SLAM/odometry reset. A
persisted map is not proof that a restarted robot still knows its position.

## Add Apple scans

Import or scan a room to establish a new space. To extend an existing robot or
Apple room, choose **Contribute model to this room** or, on a LiDAR-equipped iOS
device, **Contribute LiDAR to this room**. Enter the scan origin and heading in the
shared room. Zero is valid only when the scan already shares that coordinate
frame. The client converts its USDZ capture to measured geometry; Linux applies
the registration and merges it with other nodes' points.

This PR requires explicit registration between independent scans. It does not
claim automatic cross-device SLAM. G1 contributes geometry through ROS; Go2 has
the existing supervised movement controls. It does not add G1 locomotion control.

## State and control rules

Room writes use optimistic revisions. A stale editor receives HTTP 409 instead
of overwriting another node. Contributions carry a source, monotonic sequence and
model revision. Linux merges ten-centimetre voxels, up to 200,000 per room, and
atomically replaces and fsyncs the room file. Confirmed localization references
are persisted with their room. People positions expire after 1.25 seconds and
are never restored as live readings after a restart.

Linux retains observations after a UI disconnect, but background mapping and
search status reads cannot renew the operator's motion lease. The existing
600 ms operator watchdog and agent movement expiry remain in force. Restarting
the service restores room geometry, not live robot handles, alignment or motion.
Reconnect contributors explicitly. Search jobs are bounded to twelve turns and
five minutes and still use live obstacle and sensor checks.

Historical points and supported horizontal tiles are for rendering and selecting
a destination. They never become navigation free space. Route clearance uses the
robot's current floor and obstacle measurements. USDZ export remains available
for captured rooms; point-only robot rooms do not masquerade as USDZ archives.

## Verification

```sh
go test -race ./go/internal/twin/... ./go/internal/twinrobot/... ./go/macos/backend
python3 -m unittest discover -s go/apps/digital-twin -p 'test_*.py'
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./go/cmd/wendy-twin
```

Also build both Xcode schemes, `WendyDesktop` and `WendyMobile`. Hardware acceptance
must check a G1 cloud contribution, a Go2-created room, another client's live
rendering, service restart persistence, a bounded Go2 destination, and Stop on
operator or sensor loss. Local tests do not establish physical robot readiness.
