// Package anchors keeps persistent named places. An anchor is a pose bound to
// map keyframes by relative offsets, so it follows the map when the pose graph
// is corrected, and it carries a signature, the scans of those keyframes, so a
// device that has never seen the map can find it again from its own scans.
//
// Three parts:
//
//   - Store is the durable set of anchors: an append-only journal with a
//     checksum per record in a directory the caller chooses. Place, Update and
//     Delete change it; ApplyKeyframes feeds it the map's keyframes and moves
//     every anchor whose keyframes moved, under a new revision. Reopening the
//     directory restores the same anchors, identifiers and revisions.
//   - Resolve finds anchors in a device's own frame: it builds a small map
//     from the device's keyframes, searches each signature scan in it at every
//     position and heading, and refuses a match that is weak, has a rival,
//     puts walls across space the device saw empty, or disagrees with another
//     match of the same call.
//   - Resolver is the streaming front of Resolve for a newly arrived device:
//     it turns the device's scans and odometry into keyframes in the device's
//     odometry frame.
//
// Frames. The Store works in the site frame: ApplyKeyframes takes the
// transform from the frame the keyframes' Local poses are in. Resolve works in
// whatever frame its keyframes are in and returns anchor poses in that frame.
// Conventions are those of package geom: yaw from +X toward +Z, transforms
// map child to parent.
//
// Neither Store nor Resolver is safe for concurrent use.
package anchors
