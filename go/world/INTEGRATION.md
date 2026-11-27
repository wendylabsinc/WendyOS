# World integration status

The integration keeps WendyOS's existing room model, robot control, deployment,
enrollment, sensors and Chat behavior, and adds the implemented World workflows.
Source preservation and successful compilation are separate from an integrated
feature's runtime acceptance.

## Sources

- WendyOS base: `3b8d7341c`, including the current Go2 twin implementation.
- World base: `3afa7b6e55d735fe6f760ef911b663b96592e5b4`. This includes merged
  `8bc61bf`, the 35 feature deliveries, ESP32 firmware emulation and camera-driven
  Go2 inspection changes.
- The later localization experiments add the omnidirectional odometry model,
  stationary-update scheduling margin, exact terminal orientation and rejection
  of malformed laser transforms. Their earlier exploratory successes do not
  establish complete runtime qualification.

World's former modules now use `github.com/wendylabsinc/wendy/go/world` inside
Wendy's root Go module. Generated World protobuf descriptors retain their wire
names and were regenerated for the relocated Go packages. Third-party notices
remain in `third_party` and `production/third_party`.

## Capability tracking

| Capability | Integrated implementation | Remaining acceptance or integration |
| --- | --- | --- |
| Durable observations, source versions, expiry, provenance, geographic frames and packages | Core libraries, schemas and regression tests under `internal` | Combined client workflows and migration of existing live deployments |
| Agent publication, queries, evidence and native replication | Optional protected `world.json`, World entitlement, app sockets, lifecycle and native SensorLink integration | Deployed-agent interoperability and operator setup experience |
| Existing room twin plus signed World service | Optional `WENDY_TWIN_WORLD_CONFIG` in `wendy-twin`; shared process lifetime and independent state namespaces | Unified room/frame selection and production-authority client integration |
| Queries, regional replicas, offline areas, zones, annotations, proposals, tasks, evidence and packages | Native `wendy world` command family using invocation-owned I/O and cancellation | Broader typed Chat access and release documentation |
| World services, executors, camera, patrol, SensorLink and recording/replay | Original runtime commands under `cmd` and `production/cmd` | Full runtime matrix, packaging and operator entry points |
| macOS profiles, evidence, annotations, areas and mission review | Local WorldClient SDK and World operator workspace inside Wendy | Interactive native runtime acceptance |
| Multimodal twin, patrols, exclusion editing, incident inspection and viewer controls | All 26 native viewer files and ten review/session modes integrated in WorldTwin | Interactive UI acceptance across live profiles and sessions |
| Chat observation and mission reads, mission proposals | Host-selected agent profile; bounded signed calls; proposals remain non-executable | Broader typed mutations beyond proposals; execution remains operator reviewed |
| ESP32 firmware camera and Go2 response | Integrated Wendy Lite observer, original emulator/firmware protocol and simulation runner | Repeated trials and physical sensor/robot qualification |
| Existing physical Go2 control | Existing `twinrobot/sitenav`, room API and interactive leases retained | Combined app runtime regression checks; no physical deployment performed |

## Verified during integration

- Core and production Go package tests pass after relocating their fixtures.
- All migrated World Go packages pass. All 44 World command tests pass. Actual `wendy world` subprocess tests cover
  authenticated queries/watch over 200 records, resumable evidence retrieval
  and package downloads.
- Runtime commands build on Darwin and Linux arm64; their tests and vet pass.
- Agent appconfig and OCI tests, SensorLink/interceptor race suites, focused
  World lifecycle race tests and the Linux arm64 agent build pass.
- Combined twin/World HTTP tests exercise signed publication/query, rejected
  unsigned or wrong-audience calls, room APIs, persistence across restart,
  cancellation and startup failure cleanup.
- The macOS app builds with the integrated SDK, operator workspace and WorldTwin.
  All 176 Swift tests pass, with 13 optional interoperability skips. A native
  replay export completes all 576 native frames and produces a verified
  48-second 1920×1080 video at 24 fps. Four representative frames were inspected.
  This does not establish interactive UI acceptance.
- The migrated simulator suite passes 604 tests with 12 optional skips in a
  local Linux container, including the localization additions. The exact-goal
  C++ planner tests pass. All 15 integration test directories pass 516 tests
  with five optional skips. Another 13 firmware-scene/perception tests pass in
  the Linux MuJoCo image with one optional skip. The separate test-helper suite
  passes 139 tests with four explicit archive skips, or 154 tests with the
  original historical archive supplied read-only. Those archival checks do not
  represent new runtime measurements.
- The node image builds and its command smoke passes without networking.
  Three staged Wendy manifests validate without warnings. No device deployment
  was performed.
- The 12 World Chat tools pass signed HTTP and race tests, including rejection
  of operator credentials, shared/symlink credential files and model-supplied
  endpoint or credential arguments.

- The integrated emulator/observer/Go2 run passed all 11 checks: 360 firmware
  frames, 27.998 metres of travel, completed inspection and return, and no operator
  command files. The runner stopped its owned processes and containers.
- All five fault trials passed: invalid transforms, missing LiDAR, repeated scan
  timestamps, executor heartbeat loss and Stop. Observed stop latency ranged from
  0.499 to 1.603 seconds. Four recoverable cases remained stopped for five seconds
  before explicit Resume. These are simulation results from one fault matrix.
- A fresh two-store Go/Swift HTTP test passes 15 retention and recovery checks,
  including lost acknowledgements, partition/reconnect, process restart, quotas,
  expiry, current revocation and wrong-authority rejection. Both owned stores
  and proxies exited after the test. The payload was three synthetic bytes.
- World uses the platform SensorLink protobuf types. Signed/protocol tests and
  interoperability with the actual platform SensorLink simulator pass.

The broad existing MCP suite encounters live mesh discovery in this development
environment. The same discovery failure was reproduced on the unchanged
WendyOS checkout. Focused new Chat and catalog tests pass; this does not count
the entire MCP suite as passing.

## Behavioral boundaries

The embedded World gateway remains a signed simulation service. Production
agent publication uses the separate native agent identity and commissioned
service scopes. Combining their process or UI does not turn simulation grants,
replayed evidence or recovered localization into physical movement authority.

Chat reads its startup-selected `WENDY_WORLD_AGENT_CONFIG`. The macOS operator
workspace imports its own profile. These profiles can differ; opening a workspace
card does not transfer profile identity or authority. World CLI/MCP operations
currently support Linux and macOS. The existing Windows CLI builds for x64 and
ARM64; unsupported World operations report the platform requirement.

Current Go2 navigation uses WendyOS's custom room navigation. The transferred
AMCL/lattice changes apply to World's Nav2 simulation path. They do not replace
or silently retune the physical robot's navigation.

Original captures, firmware binaries, model weights, generated profiles, build
artifacts and review videos stay outside this repository. Synthetic regression
vectors remain explicitly test fixtures. Do not describe earlier World receipts
as new verification of this integrated build.
