# WDY-3113 implementation and validation

Validated on 2026-09-16 with Go 1.26.6 and Wendy Rosmaster Large (Jetson Orin
Nano, WendyOS 0.19.3). The application was `rosmaster-a1`, with four host-network
services on DDS domain zero. Its eligible wired interface was `usb0`; `wlan0`
was excluded from automatic discovery.

## Implementation

- SPDP replies reserve a per-peer timestamp under the participant mutex and
  observe a 30-second cooldown. Valid locators still refresh during suppression.
  Duplicate locators receive one reply. Short leases and disposal cannot reset
  a recent cooldown reservation.
- Exact active local GUID prefixes are ignored before message dispatch. External
  Fast DDS and CycloneDDS participants remain eligible. Periodic announcements
  stay at 30 seconds, with a 90-second advertised lease.
- One agent-owned pool shares physical participants by namespace device/inode,
  resolved interface index, and DDS domain. Namespace descriptors remain open
  until the final lease releases the participant. Ownership and process start
  time checks reject changed/recycled PIDs.
- Each consumer gets endpoint snapshots, coalesced change notifications, and its
  own four-sample queue filtered by subscribed writer. Queue overflow drops the
  oldest sample. Final subscription release disposes the reader and stops its
  recurring announcements. Endpoint expiry/disposal removes stale discoveries.
- Camera reconciliation is serialized and retains leases on enumeration errors.
  Routing includes the lease and writer GUID. Logical camera keys and persisted
  IDs are unchanged. Surviving leases replay discovery when a selected container
  disappears. Battery scanning retains its host scope, timing, preference,
  explicit interface overrides, and staleness behavior.
- Live testing also identified a localhost compatibility requirement: CycloneDDS
  can discover localhost participants through standard unicast ports rather
  than multicast. Loopback participants now bind an available standard
  metatraffic port (indices 0–99, bounded by the UDP port range). Wired discovery
  retains ephemeral ports. No additional unicast probe burst is generated.
  See [CycloneDDS port-number documentation](https://cyclonedds.io/docs/cyclonedds/latest/config/port_numbers.html).
- Review follow-ups: any message from a known peer renews its lease (as
  CycloneDDS does), so lost SPDP datagrams from a short-lease peer no longer
  drop its endpoints mid-stream; the pool verifies namespace ownership outside
  its lock, so a stalled containerd cannot block other consumers; a container
  whose namespace cannot be enumerated retains only its own participants
  instead of suppressing all stale-participant cleanup.

## Automated checks

Focused Linux race tests passed, including real isolated namespace creation in a
container with `SYS_ADMIN` and `NET_ADMIN` capabilities:

```sh
go test -race -count=1 -timeout 120s \
  ./internal/rtps/... \
  ./internal/agent/ros2camera/... \
  ./internal/agent/hoststats/rosbattery/...
```

Repository Linux checks (from `go/`):

```sh
go test -p=1 -gcflags=all=-c=1 ./... -race -count=1 -timeout 120s
go vet ./...
```

The complete suite passed using a native Linux volume for `TMPDIR` and a
resolvable container hostname. Initial Docker-only failures were caused by
update tests rejecting overlayfs, host-shared filesystem locking semantics, and
an unresolved fully qualified container hostname. No unrelated repository code
was changed to accommodate those failures.

Coverage includes concurrent SPDP announcements, echo suppression, cooldown
expiry, locator refresh, local GUID filtering, subscription disposal, endpoint
expiry, concurrent acquisitions, namespace aliases/isolation, ownership changes,
cancellation/failed creation, final socket cleanup, late snapshots, camera queue
flooding alongside battery samples, automatic interface resolution, host/app
reconciliation, nonzero domains, camera IDs, and explicit wireless battery
configuration.

## Device observations

All packet counts below are approximately ten-second captures on the device.
CPU percentages refer to the agent process; 100% represents one CPU core.

| State | Physical participants | Captured SPDP datagrams | Agent CPU |
| --- | ---: | ---: | ---: |
| Original agent, four app services | 9 | 611,555 | 280.1% |
| Shared pool, before active camera processing | 2 | 209 | 0.7% |
| App stopped, after successful reconciliation | 1 | 0 | 0.1% |
| App restarted, camera processing active | 2 | 116 | 90.9% |

The active-camera CPU measurement includes image reception/conversion and is not
comparable to discovery-only CPU. Discovery traffic no longer sustains the
original reply storm. Start RPCs for individual services returned in under one
second. Whole-app shutdown included the existing web service's ten-second
termination grace period. All four services were restored.

Six ROS cameras appeared with IDs 128–133, retained across app/agent restarts.
The real RealSense color camera (ID 130) produced 2,200,949 bytes through the
camera stream API; `ffprobe` identified H.264 at 640×480. The sensor-probe
compressed topic (ID 133) did not yield a frame within the existing timeout.

A temporary publisher sent `sensor_msgs/msg/BatteryState` on `usb0` for three
minutes. After the unchanged two-minute discovery window, host telemetry
reported **62.5%, discharging**, while camera data was also flowing. The shared
participant count remained two. After the publisher exited, the monitor logged
its normal 15-second silence timeout, cleared the reading, and completed a new
scan finding no battery writer. This was synthetic transport/decoder validation;
the robot's existing `/voltage` topic is not a supported BatteryState publisher.

The previous device binary was preserved at
`/data/wdy3113-validation/agent-before`. The validation agent is identified as
`dev-wdy3113`; the final artifact SHA-256 is
`c78e7c93d927079f9af96d48b92f396a851173fbe444d23f4d4ebc1179e5bd9a`.
No CLI/protobuf/manifest or persisted registry format changed.
