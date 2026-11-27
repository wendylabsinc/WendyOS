package calib

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// The streaming tests build a small world by hand: one fixed camera and one
// robot that drives a figure through its view. Marker points are placed in
// the site with the planar convention written out below, not with the geom
// helper the calibrator uses, so a handedness slip in either shows.

var handEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const (
	handCam   spatial.DeviceID = "cam"
	handRobot spatial.DeviceID = "robot"
	handSigma                  = 0.3 // pixel noise per axis
)

var (
	handTarget = spatial.TargetGeometry{ID: "board", Rows: 6, Cols: 8, Spacing: 0.08}
	// handMarker has points at two heights and no mirror symmetry.
	handMarker = spatial.FiducialGeometry{ID: "marker", Carrier: handRobot, Points: []geom.Vec3{
		{X: 0.10, Y: 0.55, Z: 0.10}, {X: 0.10, Y: 0.55, Z: -0.10}, {X: -0.10, Y: 0.55, Z: 0.10}, {X: -0.10, Y: 0.55, Z: -0.10},
		{X: 0.25, Y: 0.30, Z: 0.05}, {X: 0, Y: 0.30, Z: 0.20}, {X: -0.20, Y: 0.30, Z: 0}, {X: 0, Y: 0.30, Z: -0.15},
	}}
)

// markerInSite places a base-frame point in the site by the planar
// convention: the robot heads along (cos yaw, sin yaw) in (x, z), its +X is
// forward, +Y up and +Z to its right, which is the heading turned clockwise
// seen from above: (-sin yaw, cos yaw).
func markerInSite(p geom.Pose2, m geom.Vec3) geom.Vec3 {
	s, c := math.Sincos(p.Yaw)
	return geom.Vec3{X: p.X + m.X*c - m.Z*s, Y: m.Y, Z: p.Z + m.X*s + m.Z*c}
}

// figure is a smooth closed path through the view of the test camera, wide
// times deep around its centre, with the heading along the direction of
// travel.
type figure struct {
	centre      geom.Vec2
	wide, deep  float64
	still       bool // parked at the centre instead
	poseBias    geom.Vec2
	poseSigmaM  float64 // declared 1σ of the pose, per axis
	singleLevel bool    // all marker points at one height
}

func (f figure) at(t float64) geom.Pose2 {
	if f.still {
		return geom.Pose2{X: f.centre.X, Z: f.centre.Z, Yaw: 0.4}
	}
	const wx, wz = 2 * math.Pi / 23, 2 * math.Pi / 37
	return geom.Pose2{
		X: f.centre.X + f.wide*math.Sin(wx*t), Z: f.centre.Z + f.deep*math.Sin(wz*t+0.5),
		Yaw: math.Atan2(f.deep*wz*math.Cos(wz*t+0.5), f.wide*wx*math.Cos(wx*t)),
	}
}

// hand feeds an AutoCalibrator with what a camera would report.
type hand struct {
	t      *testing.T
	cal    *AutoCalibrator
	rng    *rand.Rand
	in     geom.Intrinsics
	pose   geom.Pose3
	offset time.Duration // sensor clock minus site time
	posed  float64       // site seconds up to which poses were fed
}

func newHand(t *testing.T, cfg Config) *hand {
	t.Helper()
	cal, err := NewAutoCalibrator(cfg, handTarget, []spatial.FiducialGeometry{handMarker})
	if err != nil {
		t.Fatal(err)
	}
	h := &hand{t: t, cal: cal, rng: rand.New(rand.NewPCG(21, 22)), in: testLens(), pose: testPose(t), offset: 40 * time.Millisecond}
	// The manifest declares a nominal lens, as a data sheet would.
	nominal := geom.Intrinsics{FX: 800, FY: 800, CX: 640, CY: 360, Width: h.in.Width, Height: h.in.Height}
	h.ingest(spatial.SensorManifest{
		Header: spatial.Header{Device: handCam, Sensor: "camera", Time: handEpoch}, Modality: spatial.KindCamera, Model: "test",
		Intrinsics: &nominal, Attachment: spatial.Attachment{Mode: spatial.AttachFixed},
	})
	return h
}

func (h *hand) ingest(m spatial.Measurement) {
	h.t.Helper()
	if err := h.cal.Ingest(m); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hand) frame(t float64, dets []spatial.Detection) spatial.CameraFrame {
	at := handEpoch.Add(time.Duration(t*float64(time.Second)) + h.offset)
	return spatial.CameraFrame{Header: spatial.Header{Device: handCam, Sensor: "camera", Time: at}, Width: h.in.Width, Height: h.in.Height, PixelSigma: handSigma, Detections: dets}
}

// drive runs the robot along f over site seconds [from, to): poses at 20 Hz
// as the robot's localization reports them, frames at 5 Hz.
func (h *hand) drive(f figure, from, to float64) {
	h.t.Helper()
	variance := f.poseSigmaM * f.poseSigmaM
	for t := from; t < to; t += 0.2 {
		for h.posed = math.Max(h.posed, from-1); h.posed < t+0.3; h.posed += 0.05 {
			p := f.at(h.posed)
			reported := spatial.DevicePose{
				Device: handRobot, Floor: "ground", FrameID: "site", Location: spatial.At(geom.Vec3{X: p.X + f.poseBias.X, Z: p.Z + f.poseBias.Z}, f.poseSigmaM*math.Sqrt2),
				Yaw: p.Yaw, Covariance: geom.Mat3{variance, 0, 0, 0, variance, 0, 0, 0, 0}, Method: "test", Score: 1,
			}
			if err := h.cal.IngestPose(reported, handEpoch.Add(time.Duration(h.posed*float64(time.Second)))); err != nil {
				h.t.Fatal(err)
			}
		}
		var dets []spatial.Detection
		for i, m := range handMarker.Points {
			if f.singleLevel {
				m.Y = 0.4
			}
			if px, ok := h.in.ProjectWorld(h.pose, markerInSite(f.at(t), m)); ok && h.in.InImage(px) {
				dets = append(dets, spatial.Detection{Kind: spatial.DetectFiducial, ID: handMarker.ID, Index: i, Pixel: noisy(h.rng, px, handSigma)})
			}
		}
		h.ingest(h.frame(t, dets))
	}
}

// show holds the target in front of the camera for n frames from site
// second from.
func (h *hand) show(from float64, n int) {
	h.t.Helper()
	for i, view := range boardViews(h.rng, h.in, n, 35*deg, handSigma) {
		dets := make([]spatial.Detection, len(view))
		for j, c := range view {
			dets[j] = spatial.Detection{Kind: spatial.DetectTargetCorner, ID: handTarget.ID, Index: j, Pixel: c.Pixel}
		}
		h.ingest(h.frame(from+0.2*float64(i), dets))
	}
}

func (h *hand) status() Status {
	h.t.Helper()
	st, ok := h.cal.Status(handCam, "camera")
	if !ok {
		h.t.Fatal("camera not under calibration")
	}
	return st
}

// errors are the distances of a status from the hand-built camera.
func (h *hand) errors(st Status) (focal, principal, position, rotation float64) {
	e := st.Estimate
	return focalError(e.Intrinsics, h.in), principalError(e.Intrinsics, h.in), e.CameraToSite.T.Dist(h.pose.T), geom.RotAngle(e.CameraToSite.R, h.pose.R)
}

// wide is a figure that fills the view of the test camera.
var wide = figure{centre: geom.Vec2{X: 4.6, Z: 3.8}, wide: 3.2, deep: 2.6}

func TestAutoCalibratorFromMarkerAlone(t *testing.T) {
	h := newHand(t, Config{})
	h.drive(wide, 0, 120)
	st := h.status()
	if st.State != spatial.CalibrationValid || st.Reason != "" || !st.HasPose {
		t.Fatalf("state %q: %s (%s)", st.State, st.Reason, st.Detail)
	}
	focal, principal, position, rotation := h.errors(st)
	e := st.Estimate
	t.Logf("focal %.3f%% (1σ %.3f) principal %.2f px (1σ %.2f) position %.4f m (1σ %.4f) rotation %.3f° (1σ %.3f) clock %v ± %v, %d points in %d passes, coverage %.2f",
		focal*100, e.FocalSigma*100, principal, e.PrincipalSigmaPx, position, e.TranslationSigmaM, rotation/deg, e.RotationSigmaRad/deg, e.TimeOffset, e.TimeOffsetSigma, st.MarkerPoints, st.MarkerPasses, st.ImageCoverage)
	if focal > 0.003 || principal > 1.5 || position > 0.01 || rotation > 0.1*deg {
		t.Errorf("errors: focal %.3f%%, principal point %.2f px, position %.4f m, rotation %.3f°", focal*100, principal, position, rotation/deg)
	}
	// The uncertainty must be the size of the error: neither a claim the
	// error breaks nor one so wide it says nothing.
	for name, z := range map[string]float64{"focal": focal / e.FocalSigma, "principal point": principal / e.PrincipalSigmaPx, "position": position / e.TranslationSigmaM, "rotation": rotation / e.RotationSigmaRad} {
		if z > 4 || math.IsNaN(z) {
			t.Errorf("%s error is %.1f of its reported sigma", name, z)
		}
	}
	if e.TranslationSigmaM > 0.01 || e.RotationSigmaRad > 0.2*deg || e.FocalSigma > 0.003 {
		t.Errorf("uncertainty too wide for exact poses: %.4f m, %.3f°, %.3f%%", e.TranslationSigmaM, e.RotationSigmaRad/deg, e.FocalSigma*100)
	}
	if d := e.TimeOffset - h.offset; d < -2*time.Millisecond || d > 2*time.Millisecond || e.TimeOffsetSigma <= 0 {
		t.Errorf("clock offset %v ± %v, want %v", e.TimeOffset, e.TimeOffsetSigma, h.offset)
	}
	if e.Residuals.RMSE < 0.8*handSigma*math.Sqrt2 || e.Residuals.RMSE > 1.2*handSigma*math.Sqrt2 || e.Covariance.Rows != 13 {
		t.Errorf("residual RMSE %.3f px for %.1f px noise per axis, covariance %dx%d", e.Residuals.RMSE, handSigma, e.Covariance.Rows, e.Covariance.Cols)
	}
	if st.DepthNearM <= 0 || st.DepthFarM < 1.5*st.DepthNearM || st.ImageCoverage < 0.25 {
		t.Errorf("coverage %.2f, depths %.2f to %.2f m", st.ImageCoverage, st.DepthNearM, st.DepthFarM)
	}

	cal, ok := h.cal.Calibration(handCam, "camera")
	if !ok {
		t.Fatal("no payload for a valid calibration")
	}
	if err := cal.Validate(); err != nil {
		t.Fatal(err)
	}
	if cal.State != spatial.CalibrationValid || cal.Parent != spatial.ParentSite || cal.Location == nil || cal.ResidualUnit != "px" || cal.Revision != st.Revision ||
		!strings.Contains(cal.Method, "marker") || cal.TranslationSigmaM != e.TranslationSigmaM || cal.TimeOffset != e.TimeOffset {
		t.Errorf("payload %+v", cal)
	}
	ids := h.cal.InputIDs(handCam, "camera")
	if len(ids) != 2 || !strings.HasPrefix(ids[0], "cam/camera/camera/") || ids[0] >= ids[1] {
		t.Errorf("inputs %q", ids)
	}
	if cams := h.cal.Cameras(); len(cams) != 1 || cams[0].Device != handCam {
		t.Errorf("cameras %v", cams)
	}
}

func TestAutoCalibratorIsDeterministic(t *testing.T) {
	var got [2]Status
	for i := range got {
		h := newHand(t, Config{})
		h.show(0, 15)
		h.drive(wide, 5, 65)
		got[i] = h.status()
	}
	a, b := got[0].Estimate, got[1].Estimate
	if a.Intrinsics != b.Intrinsics || a.CameraToSite != b.CameraToSite || a.TimeOffset != b.TimeOffset || a.TranslationSigmaM != b.TranslationSigmaM || got[0].Revision != got[1].Revision {
		t.Errorf("two runs over the same input differ:\n%+v\n%+v", a, b)
	}
	if got[0].TargetViews != 15 || !strings.Contains(mustCalibration(t, newHandAfter(t, 15, 65)).Method, "target+marker") {
		t.Errorf("target views %d", got[0].TargetViews)
	}
}

// newHandAfter is a hand that was shown the target and then watched the
// robot until site second until.
func newHandAfter(t *testing.T, views int, until float64) *hand {
	t.Helper()
	h := newHand(t, Config{})
	h.show(0, views)
	h.drive(wide, 5, until)
	return h
}

func mustCalibration(t *testing.T, h *hand) spatial.Calibration {
	t.Helper()
	cal, ok := h.cal.Calibration(handCam, "camera")
	if !ok {
		t.Fatalf("no calibration: %+v", h.status())
	}
	return cal
}

// Evidence that does not constrain the camera must leave it estimating, with
// the reason, whatever a least-squares fit could be made to return.
func TestAutoCalibratorRefuses(t *testing.T) {
	t.Run("nothing seen", func(t *testing.T) {
		h := newHand(t, Config{})
		if st := h.status(); st.State != spatial.CalibrationEstimating || st.Reason != ReasonNoEvidence || st.HasPose {
			t.Errorf("%+v", st)
		}
		if _, ok := h.cal.Calibration(handCam, "camera"); ok {
			t.Error("a calibration without evidence")
		}
		if _, ok := h.cal.Status("other", "camera"); ok {
			t.Error("status for an unknown camera")
		}
	})
	t.Run("target only", func(t *testing.T) {
		h := newHand(t, Config{})
		h.show(0, 20)
		st := h.status()
		if st.State != spatial.CalibrationEstimating || st.Reason != ReasonNoSiteEvidence || st.HasPose || st.TargetViews != 20 {
			t.Fatalf("%+v", st)
		}
		// The lens is known already; only the pose is missing.
		if e := focalError(st.Estimate.Intrinsics, h.in); e > 0.01 || st.Estimate.FocalSigma <= 0 {
			t.Errorf("focal error %.3f%% ± %.3f%% from the target", e*100, st.Estimate.FocalSigma*100)
		}
		if _, ok := h.cal.Calibration(handCam, "camera"); ok {
			t.Error("a site calibration from the target alone")
		}
	})
	t.Run("parked robot", func(t *testing.T) {
		h := newHand(t, Config{})
		h.drive(figure{centre: wide.centre, still: true}, 0, 60)
		if st := h.status(); st.State != spatial.CalibrationEstimating || st.Reason != ReasonNoSiteEvidence || st.MarkerPoints != len(handMarker.Points) {
			t.Errorf("%s (%s), %d marker points", st.Reason, st.Detail, st.MarkerPoints)
		}
	})
	t.Run("one corner of the image", func(t *testing.T) {
		h := newHand(t, Config{})
		h.drive(figure{centre: geom.Vec2{X: 6.5, Z: 1.2}, wide: 0.4, deep: 0.3}, 0, 120)
		if st := h.status(); st.State != spatial.CalibrationEstimating || st.Reason != ReasonCoverage || st.ImageCoverage >= 0.25 {
			t.Errorf("%s (%s), coverage %.2f", st.Reason, st.Detail, st.ImageCoverage)
		}
	})
	t.Run("points on one plane", func(t *testing.T) {
		// A marker with all its points at one height on a level floor: lens
		// and pose trade against each other without limit.
		planar := wide
		planar.singleLevel = true
		h := newHand(t, Config{})
		h.drive(planar, 0, 120)
		st := h.status()
		if st.State != spatial.CalibrationEstimating || (st.Reason != ReasonUncertain && st.Reason != ReasonUnconstrained) {
			focal, principal, position, rotation := h.errors(st)
			t.Errorf("%q %s (%s): errors %.2f%%, %.1f px, %.3f m, %.2f°", st.State, st.Reason, st.Detail, focal*100, principal, position, rotation/deg)
		}
		t.Logf("planar evidence: %s: %s", st.Reason, st.Detail)
	})
	t.Run("camera on a robot", func(t *testing.T) {
		h := newHand(t, Config{})
		h.ingest(spatial.SensorManifest{
			Header: spatial.Header{Device: handRobot, Sensor: "camera", Time: handEpoch}, Modality: spatial.KindCamera, Model: "test",
			Attachment: spatial.Attachment{Mode: spatial.AttachMounted, Carrier: handRobot},
		})
		if _, ok := h.cal.Status(handRobot, "camera"); ok || len(h.cal.Cameras()) != 1 {
			t.Error("a mounted camera was put under site calibration")
		}
	})
}

// A robot whose localization is off, and says so in its covariance, must
// widen the camera's uncertainty by its share, not drag the pose along
// unannounced.
func TestAutoCalibratorWeighsPoseCovariance(t *testing.T) {
	biased := wide
	biased.poseBias, biased.poseSigmaM = geom.Vec2{X: 0.04, Z: -0.03}, 0.05
	h := newHand(t, Config{})
	h.show(0, 20)
	h.drive(biased, 5, 125)
	st := h.status()
	_, _, position, _ := h.errors(st)
	t.Logf("position off by %.3f m, 1σ %.3f m: %s %s", position, st.Estimate.TranslationSigmaM, st.State, st.Detail)
	if !st.HasPose || position > 3*st.Estimate.TranslationSigmaM {
		t.Errorf("position off by %.3f m with a reported 1σ of %.3f m", position, st.Estimate.TranslationSigmaM)
	}
	// One robot on one unbroken drive is one sample of its error: 5 cm
	// cannot become a valid 2.5 cm calibration.
	if st.State == spatial.CalibrationValid || st.Reason != ReasonUncertain || st.Estimate.TranslationSigmaM < 0.025 {
		t.Errorf("%q %s (%s), 1σ %.3f m", st.State, st.Reason, st.Detail, st.Estimate.TranslationSigmaM)
	}
}

// A camera that is knocked keeps reporting; its evidence then contradicts
// itself. The calibrator must stop calling the result valid, and after a
// Reset find the new pose, with the lens it already knew.
func TestAutoCalibratorAfterCameraMoved(t *testing.T) {
	h := newHandAfter(t, 20, 95)
	before := h.status()
	if before.State != spatial.CalibrationValid {
		t.Fatalf("before the knock: %s (%s)", before.Reason, before.Detail)
	}
	old := h.pose
	h.pose = geom.Pose3{R: old.R.Mul(geom.RotExp(geom.Vec3{X: 0.03, Y: 0.045, Z: -0.02})), T: old.T.Add(geom.Vec3{X: 0.02, Y: -0.015, Z: 0.01})}
	h.drive(wide, 95, 125)
	st := h.status()
	if st.State != spatial.CalibrationEstimating || st.Reason != ReasonResidual || st.Outliers == 0 {
		t.Fatalf("after the knock: %q %s (%s), %d outliers", st.State, st.Reason, st.Detail, st.Outliers)
	}

	h.cal.Reset(handCam, "camera", true)
	if st := h.status(); st.Reason != ReasonNoEvidence || st.HasPose {
		t.Fatalf("after Reset: %+v", st)
	}
	h.drive(wide, 125, 185)
	st = h.status()
	focal, principal, position, rotation := h.errors(st)
	t.Logf("recalibrated: focal %.3f%% principal %.2f px position %.4f m (1σ %.4f) rotation %.3f° (1σ %.3f): %s %s", focal*100, principal, position, st.Estimate.TranslationSigmaM, rotation/deg, st.Estimate.RotationSigmaRad/deg, st.State, st.Detail)
	if st.State != spatial.CalibrationValid || position > 0.02 || rotation > 0.2*deg || focal > 0.005 || st.Revision == before.Revision || st.TargetViews != 0 {
		t.Errorf("recalibration: %q %s (%s), position %.4f m, rotation %.3f°, focal %.3f%%", st.State, st.Reason, st.Detail, position, rotation/deg, focal*100)
	}
	if moved := st.Estimate.CameraToSite.T.Dist(before.Estimate.CameraToSite.T); moved < 0.015 {
		t.Errorf("the new pose is %.4f m from the old one; the camera moved 0.027 m", moved)
	}
	cal := mustCalibration(t, h)
	if !strings.Contains(cal.Method, "lens+marker") || len(h.cal.InputIDs(handCam, "camera")) < 4 {
		t.Errorf("method %q, inputs %q", cal.Method, h.cal.InputIDs(handCam, "camera"))
	}

	// Without the lens, everything is forgotten.
	h.cal.Reset(handCam, "camera", false)
	if st := h.status(); st.Reason != ReasonNoEvidence || len(h.cal.InputIDs(handCam, "camera")) != 0 {
		t.Errorf("after a full Reset: %+v", st)
	}
	h.cal.Reset("other", "camera", true) // unknown cameras are ignored
}

func TestAutoCalibratorRejectsBadInput(t *testing.T) {
	if _, err := NewAutoCalibrator(Config{}, spatial.TargetGeometry{ID: "board", Rows: 1, Cols: 0, Spacing: 0.1}, nil); err == nil {
		t.Error("invalid target accepted")
	}
	if _, err := NewAutoCalibrator(Config{}, spatial.TargetGeometry{}, []spatial.FiducialGeometry{{ID: "x"}}); err == nil {
		t.Error("fiducial without points accepted")
	}
	h := newHand(t, Config{})
	if err := h.cal.Ingest(h.frame(1, []spatial.Detection{{Kind: "nonsense"}})); err == nil {
		t.Error("invalid frame accepted")
	}
	pose := spatial.DevicePose{Device: handRobot, Floor: "ground", FrameID: "site", Method: "test", Score: 1}
	if err := h.cal.IngestPose(pose, handEpoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := h.cal.IngestPose(pose, handEpoch.Add(time.Second)); err == nil {
		t.Error("a pose that is not after the previous one was accepted")
	}
	pose.Method = ""
	if err := h.cal.IngestPose(pose, handEpoch.Add(2*time.Second)); err == nil {
		t.Error("invalid pose accepted")
	}
	if err := h.cal.SetLandmarks([]spatial.Landmark{{ID: ""}}); err == nil {
		t.Error("invalid landmark accepted")
	}
}
