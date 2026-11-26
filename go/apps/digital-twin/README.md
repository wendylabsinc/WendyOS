# Linux digital twin

This first-party Wendy app owns the room database, scan merging, people tracks,
Go2 localization, rotation searches, route planning, movement loop and watchdogs.
macOS and iOS connect through the selected gateway's authenticated app transport.
They render snapshots, contribute scans and camera observations, and send user
intent. There is no fallback to a locally running robot engine.

The app binds `127.0.0.1:8791`. Its `http` entitlement declares the service
port. Apple clients reach it through authenticated container exec, using the
bundled binary’s fixed `--proxy` mode to forward bytes to that loopback listener. Do not publish this port directly to a LAN.
The `admin` entitlement gives this trusted app access to the agent's local gRPC
socket through `WENDY_AGENT_SOCKET`. The `persist` entitlement mounts the named
`dev.wendy.digital-twin` volume at `/data`. Executables stay in the image.

## Deploy

Opening Digital Twin in the macOS or iOS app automatically installs the bundled
Linux service on the selected device, starts it when stopped, and waits for its
health endpoint. App updates replace the service version while preserving its
named room-data volume. ARM64 and x86-64 bundles ship with the Apple app; no
separate CLI or registry download is required.

For standalone deployment, prepare a build context and use the Wendy CLI or MCP:

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
retaining the originals. Valid saved LiDAR reference maps migrate too, including
for previously migrated rooms, only when the frame and model digest match and
Linux has no references for that robot. Already present captures are not overwritten. Removed
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
Reconnect contributors explicitly. Search jobs are bounded to six 60-degree turns and
five minutes and still use live obstacle and sensor checks.

## Find an object

`POST /v1/robot` with `{"Operation":"find","Frame":…,"Query":"toolbox","Bark":true}`
starts a mission on an aligned, ready Go2. Linux does the looking and walking:

1. If this room remembers the object, Go2 first walks back to where it was last seen.
2. At each place it checks a camera view, then turns 60 degrees, up to a full circle.
3. With nothing found, it walks to measured clear floor farthest from the places
   already searched, at most 4 m away, and repeats. It stops after six places.
4. When the object is seen, its place in the room is the nearer of the first LiDAR
   obstacle along that bearing and the point where the box's lower edge meets the
   floor. Go2 walks to 0.8 m short of it, or 1.3 m when that is blocked.
5. The location is saved with the room, and Go2 barks if asked.

Every walk and turn is the operator's own `destination` or `look` order, with the
same route checks, 30-second movement limit and 600 ms operator lease. The mission
never renews that lease, so Go2 stops when no client is rendering it. Any other
order, including `stop`, ends the mission. A mission lasts ten minutes at most.

The service cannot decode Go2's H.264 video. `find-poll` returns `Find.WantsView`
when Go2 is standing still and a view is needed; the client answers with
`find-view` carrying that number and one JPEG decoded after the request. Late and
duplicate frames are refused. The vision model runs in the gateway's own Ollama on
loopback (`ModelPort`, default 11434). The service uses `Model` when it reports
the `vision` capability, otherwise the first installed model that does, and fails
with a download hint when there is none. The model only returns a box in the image;
it never produces motion.

`Find.State` ends as `found`, `not-found`, `stopped` or `failed`. `found` means the
camera model reported the object once; it is not re-checked after the walk. A
remembered location is a hint for the next search, never proof the object is still
there. Camera intrinsics are the built-in Go2 720p preset without distortion, and
the camera height is a constant, so floor-based distances are approximate.

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

## Natural-language Go2 control

Terminal `wendy chat` exposes `unitree_pairings`, `robot_status`, and
`robot_command` through the robotics MCP group. The CLI and this Linux service
both need the chat-control update. The CLI uses the authenticated container
stream to reach `POST /v1/robot/command`; no new public port is needed.

Try "connect to my Go2", "turn left 90 degrees", "turn around", or "stop".
Connect starts observations using the same preparation as the robot map. Turning
needs current robot measurements but does not require a map alignment. Turns
are limited to 180 degrees and use the existing controller in steps of at most
60 degrees, with an acknowledged stop between steps.

For navigation, first localize Go2 in its map. Say "remember this spot as the
kitchen", then "go to the kitchen" to return to that saved position. Named
positions belong to the map revision and are stored separately from object
sightings. Chat does not infer traversable coordinates from a room name or an
image. The existing planner still checks the route and destination against live
observations. Unknown names and missing localization return a concrete blocker.

A motion call waits at most 30 seconds and reports completion, cancellation,
failure, or replacement by a newer operator command. Interrupting the call stops
its motion; a manual map command takes precedence. Request IDs prevent retried
turns or navigation goals from executing twice during the observer session.
Stop is always executed, even when the command history is full. Reopen the robot
session after 256 retained non-stop command receipts. Restarting the service
ends active goals and clears receipts; never replay an uncertain old motion
across a service restart.
