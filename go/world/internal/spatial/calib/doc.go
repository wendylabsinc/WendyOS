// Package calib estimates camera calibrations: the intrinsics (focal lengths,
// principal point, two radial distortion terms) and, for a fixed camera, its
// pose in the site frame and its clock offset. Nothing is entered by hand.
//
// Three pure estimators, each returning an Estimate with a covariance and
// residual statistics, or a typed error when the data does not constrain the
// answer (geom.ErrInsufficient, geom.ErrDegenerate, geom.ErrNoConsensus,
// geom.ErrSingular, geom.ErrNoConvergence):
//
//   - CalibrateIntrinsics: Zhang's method on views of a planar target.
//   - SolvePnP: the pose of a camera with known intrinsics from known 3D
//     points (surveyed fiducials, mapped landmarks), by RANSAC over a linear
//     solve and then nonlinear refinement.
//   - Resect: intrinsics and pose together from known 3D points alone.
//
// AutoCalibrator is the streaming form. It ingests camera frames, the pose
// estimates of localized robots and, optionally, a landmark map. It harvests
// correspondences on its own (views of the target, the fiducial marker of a
// robot crossing the view, wall tags), solves all of them jointly and moves a
// camera from estimating to valid only when the covariance of that solution
// is inside the limits of Config. Until then Status says why.
//
// All of it is one least-squares problem (bundle.go): pixel residuals in the
// camera's intrinsics, pose and clock offset, plus one small block of
// nuisance parameters per group of observations that share an unknown: the
// pose of the target in one view, the localization error of a robot during
// one pass, the survey error of one landmark. The blocks are eliminated by a
// Schur complement, so the cost is linear in the evidence and the reported
// covariance is the marginal one, with the upstream errors accounted for
// rather than averaged as if they were independent pixel noise.
//
// Conventions follow package geom: transforms map child to parent, the
// optical frame is x right, y down, z forward, pixels are raw (distorted).
// Results are deterministic: sampling is seeded by Config.Seed and no clock
// is read; time comes from the measurements.
package calib
