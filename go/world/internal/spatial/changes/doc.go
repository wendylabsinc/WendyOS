// Package changes detects changes of the static environment: furniture that
// appeared, disappeared or moved, and doors that opened or closed.
//
// Method. Lidar sweeps placed in the site frame are counted into an Evidence
// map per floor and epoch: for every cell, how many sweeps returned from it
// and how many crossed it (a beam stops counting as a crossing
// Config.HitMargin short of its return, because noise smears a surface over
// neighbouring cells). A cell is confidently Occupied or Free only when
// it was looked at often enough and nearly always read the same way; a cell
// that read both ways is Transient (a person walked through, a robot drove
// by) and a cell seen too rarely is Unobserved. Compare sets a reference map
// against a current one and keeps the cells that are confident in both and
// differ, drops those within Config.Tolerance of matching structure (map
// misregistration, not change), clusters the rest and classifies each
// cluster: appeared, disappeared, moved (a disappearance paired with a nearby
// appearance of similar footprint) or, at a supplied door, door closed or
// opened. Nothing is ever reported where either map lacks confident
// evidence, so unobserved areas and moving actors produce no events.
//
// Detector is the streaming form. It cuts time into epochs of Config.Epoch,
// compares each finished epoch with the reference and then lets the
// reference adopt what the epoch observed confidently: new ground, cells
// that agree, and the cells of the changes it reported. The reference is
// thus the last confirmed state of each cell, a change that persists is
// reported once, and a difference too small to report is not absorbed. A change keeps its id while later epochs
// add evidence to it (another face of the object comes into view, the other
// half of a move is seen an epoch later).
//
// Inputs are device-agnostic: spatial.Scan with the spatial.DevicePose and
// spatial.Calibration of its sensor, spatial.Track for known movers and
// spatial.Portal for door locations. Outputs are spatial.ChangeEvent
// payloads; Change.Component wraps one as a World component with Inferred
// provenance and the scans of both epochs as ancestry.
//
// Conventions follow package geom: the planar map is World X and Z, yaw runs
// from +X toward +Z, a scan frame has +X forward and +Z right. Cells are
// those of package grid: cell (ix, iz) covers [ix·s, (ix+1)·s) × [iz·s,
// (iz+1)·s).
//
// Limits, all deliberate:
//   - A lidar sees the outline of an object, not its inside. An event is
//     located at the centre of the bounding box of its changed cells, which
//     is the object's centre when two adjacent faces were seen and is short
//     by half its depth when only one was; Location.UncertaintyM says which.
//   - Two objects closer together than Config.ClusterLink are one event.
//   - A mover that stood still for every look an epoch had at it is furniture
//     to a lidar. Supply its track (ObserveTrack) to exclude it.
//   - Without supplied portals a door leaf is reported as appeared or
//     disappeared structure.
package changes
