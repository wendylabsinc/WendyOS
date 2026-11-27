// Package geom is the numerical core of the spatial layer: planar and 3D rigid
// transforms, small dense linear algebra, nonlinear least squares, rigid
// alignment, RANSAC, covariance propagation, a pinhole camera model and planar
// geometry. Standard library only; every function is deterministic.
//
// Conventions, fixed by the spatial design:
//   - World axes are right-handed, Y up. Lengths are metres, angles radians.
//   - The planar map uses World X and Z. Pose2.Yaw is measured from +X toward
//     +Z, so the heading vector is (cos Yaw, sin Yaw) in (x, z). Seen from above
//     positive yaw is clockwise; it is a right-handed rotation by −Yaw about
//     World +Y (see Pose3FromPose2).
//   - A planar body or sensor frame has +X forward and +Z to the right of
//     travel. A ROS REP-103 planar pose (x, y, yaw) maps to Pose2{x, −y, −yaw}.
//   - Transforms map child coordinates to parent coordinates.
//   - The camera optical frame is x right, y down, z forward.
//
// Estimators return a residual or covariance with the estimate and a typed
// error (see errors.go) when the data does not constrain the answer.
package geom
