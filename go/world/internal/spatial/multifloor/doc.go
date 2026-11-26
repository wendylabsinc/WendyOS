// Package multifloor extends the planar spatial layer across floors and out
// of the building. It finds floor changes in a barometer stream, grows a floor
// model with the portals where devices changed floor, routes between poses on
// different floors, ties the site frame to the Earth from GNSS fixes, and
// keeps one continuous pose per device while GNSS comes and goes.
//
// The pure estimators are:
//
//   - Detector: one barometer stream in, vertical moves out. A move is a
//     height change of at least a storey that has come to rest or turned
//     back, back-dated to the instant the device was half way.
//   - Georeference: site-frame positions paired with GNSS fixes in, a
//     spatial.GeoReference with its residual out.
//   - PlanRoute: per-floor occupancy grids and portals in, the cheapest route
//     between two places out.
//
// Mapper is the streaming entry point that wires them together. It ingests
// spatial.Measurement values (Baro, Odometry, GnssFix) in time order and the
// site-frame pose fixes of a localizer (FixPose), and holds the floor model,
// the geo reference and one pose per device. Records returns everything it
// would publish with the ancestry of each record, and Record.Component wraps
// one as a World component with ModelID "wendy.spatial.multifloor".
//
// Floor model. A floor is a spatial.FloorInfo: an id, an elevation and the
// portals that touch it. As in the foundation, open ground beside a building
// is a floor of its own at the elevation of the storey it adjoins, flagged
// Outdoor, and joined to that storey by a door portal. A device is on the
// outdoor floor while it receives GNSS fixes. Floor ids are generated: the
// floor every device powers on at is Config.StartFloor, storeys found later
// are "level-1", "level-2", ... in order of discovery, and the outdoor floor
// beside a storey is that storey's id with "-outdoor" appended.
//
// Latency. A change of storey is known only once the device has come to rest
// on the new floor: about two FloorConfig.Dwell after it arrives, six to ten
// seconds after it was half way with the defaults. Until then Floor reports
// the old floor with moving set. Transition.Start and End are back-dated, so a
// consumer that files measurements by floor (scans into per-floor maps) holds
// them back while moving is set and files them when the Transition arrives:
// those from before Start under From, those from after End under To.
//
// Timestamps are taken as they come. Measurement times are on sensor clocks;
// an engine that knows a sensor's Calibration.TimeOffset should subtract it
// before Ingest. The tens of milliseconds left otherwise move a floor change
// by centimetres.
//
// Conventions follow package geom: the planar map is World X and Z, yaw runs
// from +X toward +Z (clockwise seen from above), elevation is World Y.
package multifloor
