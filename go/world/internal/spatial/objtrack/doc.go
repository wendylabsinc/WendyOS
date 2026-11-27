// Package objtrack tracks what moves on a site: people, carts and robots.
//
// Three layers, each usable on its own:
//
//   - LidarDetections subtracts the static map from a scan (a return is
//     static when an occupied cell lies within its position uncertainty, and
//     carries no information where the map never saw free space), clusters
//     the rest and reports each cluster's centre and radius in the site frame.
//   - CameraDetections places the foot pixel of every object detection on
//     the floor plane through the camera calibration, with the covariance the
//     pixel noise and the calibration uncertainty give it at that range.
//   - Tracker fuses detections from any number of sensors: a constant
//     velocity Kalman filter per track in site (x, z), global assignment of
//     each sample's detections by likelihood (position and class) against a
//     clutter density, class votes and a lifecycle of tentative, confirmed,
//     coasting and deleted. Tracks live in the site frame and belong to no
//     sensor, so an object that leaves one sensor's view and enters another's
//     keeps its identity as long as it reappears where its coasting track
//     says it can be.
//
// Static structure is kept out three times. The map removes what was there
// when it was built. What appears later (new furniture, a door that closes)
// still produces detections, so a track is confirmed only after one sensor
// has seen it travel further than a static body's apparent motion could
// explain: MinTravelM plus its own diameter, by way of a point half as far,
// each distance significant against the detections' own uncertainty. And
// what has been watched for StaticAfter without doing so is held to be
// structure: its track stops assuming it might move and is never confirmed.
// Something that never moves stays tentative for as long as it is seen.
//
// A localized device reports its own pose (ObservePose). That pose places
// the device's sensors and is also the best observation of the device as a
// moving body, so each such device owns one track, confirmed at once, to
// which other sensors' detections of it are associated instead of starting
// a second track.
//
// Streaming use: SetFloor for each floor, SetCalibration for each sensor,
// then ObservePose and Ingest in time order; Tracks returns the current
// spatial.Track payloads and Component wraps one for publication.
//
// The existing go/world/internal/tracking package was considered and not
// reused: it estimates position only (velocity is an exponential average
// with no covariance, fusion is covariance intersection), assigns greedily
// rather than globally, has neither classes nor a tentative/confirmed
// lifecycle, drops a track after at most 10 s whatever its state, and every
// detection must carry a model digest, a World version and a validity
// window that lidar clusters do not have. Its bookkeeping of duplicate and
// reordered source samples is the transport's concern here.
//
// Conventions follow package geom: the planar map is World X and Z, positive
// yaw is clockwise seen from above, body +Z is right.
package objtrack
