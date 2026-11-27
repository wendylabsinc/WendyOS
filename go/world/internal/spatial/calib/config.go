package calib

import (
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Feature and ModelVersion identify this package in the provenance of the
// records it derives: spatial.InferredProvenance(Feature, ModelVersion, ...).
const (
	Feature      = "calib"
	ModelVersion = "1"
)

// Config holds every threshold and noise assumption of the package. A zero
// field takes the documented default.
type Config struct {
	// Seed selects the RANSAC samples. Any value gives a deterministic result.
	Seed uint64

	// PixelSigma is the 1σ localisation noise, in pixels, assumed for a
	// detection whose frame reports none. Default 1.
	PixelSigma float64
	// HuberSigmas is the residual, in sigmas, beyond which a detection's
	// influence stops growing. Default 3.
	HuberSigmas float64
	// MaxIterations bounds each nonlinear refinement. Default 50.
	MaxIterations int

	// MinViews is the fewest target views CalibrateIntrinsics accepts. Zhang's
	// method needs three in general position. Default 3.
	MinViews int
	// MinTiltSpread is the smallest angle, in radians, that must separate the
	// target normals of the two most different views. Views that share one
	// orientation do not determine the focal length. Default 10°.
	MinTiltSpread float64

	// InlierPx is the reprojection error within which a known 3D point agrees
	// with a candidate camera. It has to cover the position error of the
	// points as well as the pixel noise: 2 cm at 3 m is about 5 px. Default 8.
	InlierPx float64
	// RansacIterations is the number of minimal samples drawn. Default 200.
	RansacIterations int
	// RansacPoints caps the points the sampling stage looks at; a larger set
	// is thinned evenly. The refinement always uses every point. Default 400.
	RansacPoints int
	// MinInliers is the smallest consensus accepted, in points. Default 8, two
	// more than the six-point linear solve.
	MinInliers int

	// The remaining fields configure AutoCalibrator.

	// MaxTargetViews bounds the target views kept per camera. Default 120.
	MaxTargetViews int
	// MinViewShiftPx is how far, as a mean over its corners, the target must
	// have moved in the image since the last kept view. Default 8.
	MinViewShiftPx float64

	// PoseErrorCorrelation is how long the localization error of a robot
	// persists: the correlation time of the error of the DevicePose stream. A
	// map-matching error moves with the robot's surroundings, over tens of
	// seconds, not from frame to frame. Default 20 s.
	PoseErrorCorrelation time.Duration
	// PoseErrorWindow is the length of a marker pass: the sightings of one
	// marker by one camera inside a window share a localization error that is
	// modelled as an unknown offset plus a linear drift. It has to be short
	// enough for the departure of the real error from a straight line to stay
	// well under the pixel noise: over a window W an error of size s that
	// varies on the time scale T bends by about s·(W/T)²/2, which for 2 cm
	// seen from 3 m is 0.2 px at a quarter of the correlation time and close
	// to 1 px at half of it. Default 5 s.
	PoseErrorWindow time.Duration
	// ElevationSigmaM is the 1σ error of a robot's elevation, which a planar
	// DevicePose covariance does not carry. Default 0.01.
	ElevationSigmaM float64
	// PoseTimeSigma is the 1σ accuracy of the timestamps of the pose stream. A
	// camera's clock offset is measured against them and is never reported as
	// better known. Default 1 ms.
	PoseTimeSigma time.Duration
	// MaxPoseGap is the longest interval between two robot pose estimates
	// across which a pose is interpolated to a frame's timestamp. Default 250 ms.
	MaxPoseGap time.Duration
	// PoseBuffer is how long pose estimates and unmatched marker sightings are
	// kept waiting for each other. Default 2 s.
	PoseBuffer time.Duration
	// MinMarkerTravelM is how far a marker must have moved since its last
	// kept sighting: a parked robot adds nothing. Default 0.05.
	MinMarkerTravelM float64
	// MaxPasses bounds the marker passes kept per camera. Default 512.
	MaxPasses int
	// PixelSigmaFloor is the best pixel accuracy credited to the mean of many
	// detections of a static fiducial: averaging removes noise, not detector
	// bias. Default 0.1.
	PixelSigmaFloor float64

	// OutlierSigmas is the residual, in sigmas, beyond which AutoCalibrator
	// sets a site-frame point aside before its final solve: a misdetection, or
	// a frame taken while the robot started or stopped. Default 5.
	OutlierSigmas float64
	// MaxOutlierFraction is the largest share of site-frame points that may be
	// set aside in a valid calibration. More than that means the evidence
	// disagrees with itself, as it does after the camera has moved. Default 0.1.
	MaxOutlierFraction float64

	// CoverageCells is the number of columns of the image grid on which
	// coverage is counted; rows follow from the aspect ratio. Default 8.
	CoverageCells int
	// MinSitePoints is the fewest site-frame correspondences from which a pose
	// is attempted. Default 24.
	MinSitePoints int
	// MinImageCoverage is the fraction of image cells that must hold evidence
	// before a calibration can become valid: distortion fitted on a corner of
	// the image does not extrapolate. Default 0.25.
	MinImageCoverage float64
	// MinDepthRatio is the smallest ratio of far to near depth of the
	// site-frame points when no target view pins the intrinsics: without depth
	// variation the focal length trades against the distance. Default 1.5.
	MinDepthRatio float64

	// A calibration becomes valid when its 1σ uncertainty is inside all four
	// limits below. The defaults are half the design targets (1 %, 3 px, 5 cm,
	// 1°), so that a two-sigma error still meets them.
	MaxFocalSigma        float64 // relative. Default 0.005.
	MaxPrincipalSigmaPx  float64 // Default 1.5.
	MaxTranslationSigmaM float64 // Default 0.025.
	MaxRotationSigmaRad  float64 // Default 0.5°.
	// MaxReprojectionRMSE is the largest RMS pixel residual of a valid
	// calibration. Default 1.
	MaxReprojectionRMSE float64
}

func (c Config) withDefaults() Config {
	def := func(v *float64, d float64) {
		if !(*v > 0) {
			*v = d
		}
	}
	defInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	const deg = math.Pi / 180
	def(&c.PixelSigma, 1)
	def(&c.HuberSigmas, 3)
	defInt(&c.MaxIterations, 50)
	defInt(&c.MinViews, 3)
	def(&c.MinTiltSpread, 10*deg)
	def(&c.InlierPx, 8)
	defInt(&c.RansacIterations, 200)
	defInt(&c.RansacPoints, 400)
	defInt(&c.MinInliers, 8)
	defInt(&c.MaxTargetViews, 120)
	def(&c.MinViewShiftPx, 8)
	defDur(&c.PoseErrorCorrelation, 20*time.Second)
	defDur(&c.PoseErrorWindow, 5*time.Second)
	def(&c.ElevationSigmaM, 0.01)
	defDur(&c.PoseTimeSigma, time.Millisecond)
	defDur(&c.MaxPoseGap, 250*time.Millisecond)
	defDur(&c.PoseBuffer, 2*time.Second)
	def(&c.MinMarkerTravelM, 0.05)
	defInt(&c.MaxPasses, 512)
	def(&c.PixelSigmaFloor, 0.1)
	def(&c.OutlierSigmas, 5)
	def(&c.MaxOutlierFraction, 0.1)
	defInt(&c.CoverageCells, 8)
	defInt(&c.MinSitePoints, 24)
	def(&c.MinImageCoverage, 0.25)
	def(&c.MinDepthRatio, 1.5)
	def(&c.MaxFocalSigma, 0.005)
	def(&c.MaxPrincipalSigmaPx, 1.5)
	def(&c.MaxTranslationSigmaM, 0.025)
	def(&c.MaxRotationSigmaRad, 0.5*deg)
	def(&c.MaxReprojectionRMSE, 1)
	return c
}

// Correspondence pairs a known point with the raw (distorted) pixel at which
// it was detected. The point is in the target's own frame (z = 0) for
// CalibrateIntrinsics and in the site frame otherwise.
type Correspondence struct {
	Point geom.Vec3
	Pixel geom.Pixel
}

// Estimate is a calibration with its uncertainty. Fields that the producing
// estimator did not solve for keep their input or zero value.
type Estimate struct {
	Intrinsics geom.Intrinsics
	// CameraToSite maps the optical frame to the frame of the 3D points.
	CameraToSite geom.Pose3
	// TimeOffset is the sensor clock minus site time. Only AutoCalibrator
	// estimates it, and only from moving points.
	TimeOffset      time.Duration
	TimeOffsetSigma time.Duration

	// Covariance is the covariance of the solved parameters, from the normal
	// matrix of the refinement with the nuisance parameters marginalised and
	// scaled by the fit. Rows follow the order fx, fy, cx, cy (pixels), k1,
	// k2, rotation (3: a right perturbation in the optical frame, radians),
	// position (3: site axes, metres), time offset (seconds), keeping only the
	// parameters the estimator solved for.
	Covariance geom.Mat

	// One-sigma summaries of Covariance; zero for what was not solved.
	FocalSigma        float64 // relative, the less certain of fx and fy
	PrincipalSigmaPx  float64 // radial
	TranslationSigmaM float64 // radial
	RotationSigmaRad  float64 // all axes

	// Residuals are reprojection distances in pixels over the points used.
	Residuals spatial.ResidualStats
	// Inliers indexes the correspondences within Config.InlierPx of the
	// solution (SolvePnP and Resect).
	Inliers []int
}

// estimate fills an Estimate from a solved bundle.
func (b *bundle) estimate(res bundleResult) Estimate {
	e := Estimate{
		Intrinsics: res.state.cam.in, CameraToSite: res.state.cam.pose, Covariance: res.cov,
		TimeOffset: time.Duration(res.state.cam.dt * float64(time.Second)), Residuals: residualStats(res.dist),
	}
	sigma := func(par int) float64 {
		for i, j := range b.free {
			if j == par {
				return math.Sqrt(math.Max(res.cov.At(i, i), 0))
			}
		}
		return 0
	}
	in := res.state.cam.in
	e.FocalSigma = math.Max(sigma(parFX)/in.FX, sigma(parFY)/in.FY)
	e.PrincipalSigmaPx = math.Hypot(sigma(parCX), sigma(parCY))
	e.RotationSigmaRad = math.Sqrt(sigma(parRot)*sigma(parRot) + sigma(parRot+1)*sigma(parRot+1) + sigma(parRot+2)*sigma(parRot+2))
	e.TranslationSigmaM = math.Sqrt(sigma(parPos)*sigma(parPos) + sigma(parPos+1)*sigma(parPos+1) + sigma(parPos+2)*sigma(parPos+2))
	e.TimeOffsetSigma = time.Duration(sigma(parDT) * float64(time.Second))
	return e
}
