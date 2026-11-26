// Package cslam is multi-robot collaborative SLAM on the planar map of the
// spatial layer: robots that start in unknown, unrelated frames build one
// shared pose graph, and the package reports optimised keyframes, the
// alignment of every robot's own map frame to the shared frame, device poses
// and a merged occupancy grid per floor.
//
// Front end, one per robot and independent of every other robot. Odometry
// predicts each lidar scan's pose; a scan becomes a keyframe once the robot
// has moved Config.KeyframeDistance or turned Config.KeyframeAngle, is
// matched against the robot's active submap (scanmatch.Matcher.Refine), and
// the match is fused with the odometry step by their covariances, so a
// direction the scan cannot observe falls back on odometry. Submaps hold
// Config.SubmapKeyframes keyframes and overlap by half. A robot's own map
// frame is the frame in which its first keyframe is the identity.
//
// Back end, shared. Every keyframe is a node of one posegraph.Graph, chained
// to its predecessor by the fused step. A keyframe is also matched against
// finished submaps: of its own robot (loop closure) and of other robots. For
// robots whose frames are already related the search is a correlative match
// in a window around the predicted pose; for robots in unrelated frames it is
// a global correlative match over the whole submap. A match is verified by
// its score and by geometric consistency: it enters the graph only when
// matches from other keyframes imply the same frame-to-frame transform
// (Config.MinConsistent, Config.MinAlignConsistent to relate two frames for
// the first time). Closures are robust edges, and one whose χ² after
// optimisation exceeds Config.OutlierProbability is removed, so a false
// closure cannot bend the map.
//
// Gauge. The shared frame is the own map frame of the robot with the earliest
// keyframe, unless Mapper.AddPrior supplied an absolute pose: then it is the
// prior's frame.
//
// Order independence. Ingest runs the front end only. Solve feeds keyframes
// to the back end in one canonical order (keyframe time, then device, then
// sequence), whatever order the robots' measurements arrived in, and rebuilds
// the back end when a keyframe arrives that sorts before one already
// processed. A Solve after all measurements therefore gives the same result,
// to the last bit, for any interleaving of the robots' streams. Each robot's
// own measurements must arrive in time order. So that live streams, which
// interleave within a fraction of a second, do not trigger that rebuild,
// Solve leaves the newest keyframes for the next call until every robot
// heard from within Config.ReorderWindow has caught up with them.
//
// Inputs this package does not estimate: the lidar mount and clock offset
// (Mapper.SetCalibration, from sensor registration) and the floor a device is
// on (Mapper.SetFloor, from multi-floor mapping). A mount error is invisible
// to the graph: it turns and shifts that robot's whole map about its own
// origin, and with it the base poses the robot reports. The covariance of a
// frame alignment therefore includes the errors the calibrations declare.
//
// Outputs. Solution carries, per robot, the optimised spatial.Keyframe list,
// the spatial.FrameAlignment of its own map frame and its spatial.DevicePose;
// Solution.Grid merges the keyframe scans of one floor; Solution.Records and
// Solution.MapRecords return all of it ready for spatial.ToComponent. Robots
// that share no place with the others stay in their own frames, without an
// alignment, until they do.
//
// Conventions follow package geom: yaw runs from +X toward +Z, a base frame
// is x forward and z right, transforms map child to parent.
package cslam
