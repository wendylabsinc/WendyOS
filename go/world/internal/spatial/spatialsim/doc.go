// Package spatialsim is the deterministic simulator the spatial layer is
// proven against. A Site is an immutable world: floors with walls, rooms,
// doors, furniture, wall fiducials, UWB beacons, sound and gas sources, moving
// actors and devices that follow fixed routes. Scheduled events change it over
// time: furniture appears, moves and disappears, doors toggle, a fixed camera
// is bumped, a lidar mount slips, devices join late.
//
// A Site has three faces and tests must keep them apart:
//
//   - What any estimator may consume: the per-device generators (Scan,
//     Odometry, Imu, Baro, Gnss, CameraFrame, Ranges, Bearings, Gas), the
//     merged Stream, and the declared knowledge Manifests, Fiducials, Target
//     and Survey.
//   - Reference, the stand-ins for what OTHER features would have estimated:
//     localized device poses, sensor calibrations, the landmark map and a
//     per-floor occupancy map, as the payloads those features publish, each
//     truth plus a seeded error near that feature's accuracy. A feature test
//     may feed a Reference value to its estimator only for a quantity the
//     feature does not itself estimate. It is the only truth-derived input a
//     test may use; it exists so that a tracker's test need not carry its own
//     SLAM and calibration, nor read Truth instead.
//   - Truth, which is for SCORING ONLY. Nothing read from Truth may reach an
//     estimator under test.
//
// Provenance. Site.SimulationContext, Site.Input, ClockDomain and SiteFrame
// are the one simulation context, clock domain and site frame, so records
// derived by different features apply to the same world.Replica.
//
// Determinism. The layout, the schedule and every trajectory are fixed by the
// scenario and do not depend on the seed. The seed selects sensor noise,
// per-sensor clock offsets, sensor biases and the true intrinsics of fixed
// cameras. Each measurement draws from its own random stream keyed by (seed,
// device, sensor, instant), so a measurement is the same value whatever was
// generated before it and in whatever order.
//
// Streams are bit-identical for a seed on one GOARCH. Across architectures
// they are numerically equal to about 1e-9, not byte-equal: the compiler
// fuses multiply-adds on arm64 and not on amd64. Do not commit a digest or a
// recorded fixture of a stream, and leave margin on a threshold tuned to one
// seed.
//
// Cost. The whole Office stream takes about 0.2 s per simulated minute, and
// about 1.7 s under the race detector: ten minutes of everything is 17 s
// there before any estimation, most of it lidar and cameras. An acceptance
// test should stream only the devices and kinds it needs (Filter) and shorten
// its run under the race detector with the build-tagged constant pattern of
// race_on_test.go and race_off_test.go.
//
// Time. Arguments are site time as time.Time; Site.At converts an offset from
// the epoch. A sensor with rate f samples at site instants k/f. The timestamp
// a measurement carries is on the sensor's own clock: site time plus that
// sensor's clock offset (Truth.ClockOffset). Odometry is each device's
// reference clock and has offset zero.
//
// Conventions follow package geom: World is right-handed and Y up, the planar
// map is World X and Z, yaw runs from +X toward +Z, a body frame is x forward,
// y up, z right, transforms map child to parent, the optical frame is x right,
// y down, z forward.
//
// Simplifications, all deliberate:
//   - Walls have no thickness and every room polygon is axis-aligned.
//   - Devices move at constant speed along straight legs and turn on the spot,
//     so a mobile IMU reads gravity plus noise and the true yaw rate.
//   - A lidar sweep is instantaneous and sees every solid whatever its height.
//   - Camera occlusion is a planar sight-line test: walls and closed doors
//     always block, solids block when the line passes below their top.
//   - UWB and sound do not cross floors; sound needs a clear sight line.
//   - The gas field ignores walls.
//   - A robot body is a circle to a lidar, so a scan gives another robot's
//     centre but not its heading, and an object's foot pixel carries
//     ObjectPixelSigma (2 px). Lidar-camera extrinsic refinement is therefore
//     scored on the reprojection of a peer robot's marker points (PixelSigma,
//     0.5 px) placed with the peer's estimated pose, not on object feet.
package spatialsim
