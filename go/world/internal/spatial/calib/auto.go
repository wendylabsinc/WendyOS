package calib

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Reason says why a camera is not yet calibrated.
type Reason string

const (
	// ReasonNoEvidence: nothing usable has been seen yet.
	ReasonNoEvidence Reason = "no_evidence"
	// ReasonNoSiteEvidence: too few site-frame correspondences for a pose. The
	// intrinsics may already be known from the target (Status.Estimate).
	ReasonNoSiteEvidence Reason = "no_site_evidence"
	// ReasonCoverage: the evidence covers too little of the image, or of the
	// depth range when no target view pins the intrinsics.
	ReasonCoverage Reason = "coverage"
	// ReasonUnconstrained: the evidence admits no consistent camera, or leaves
	// some parameter unobservable.
	ReasonUnconstrained Reason = "unconstrained"
	// ReasonUncertain: there is a solution, but its uncertainty is above a
	// Config limit. Status.Detail names the limit.
	ReasonUncertain Reason = "uncertain"
	// ReasonResidual: the solution does not explain the detections to within
	// Config.MaxReprojectionRMSE.
	ReasonResidual Reason = "residual"
)

// Status is the calibration state of one camera.
type Status struct {
	Device spatial.DeviceID
	Sensor spatial.SensorID
	// State is CalibrationValid or CalibrationEstimating.
	State  spatial.CalibrationState
	Reason Reason // empty when valid
	Detail string // human-readable elaboration of Reason

	// Evidence harvested so far.
	TargetViews  int
	MarkerPasses int // groups of marker sightings, one per robot and PoseErrorWindow
	MarkerPoints int
	Landmarks    int // mapped static fiducials in view
	// Outliers is the number of site-frame points set aside because the
	// solution misses them by more than Config.OutlierSigmas.
	Outliers int

	// ImageCoverage is the fraction of image cells holding evidence.
	ImageCoverage float64
	// DepthNearM and DepthFarM are the 5th and 95th percentile depth of the
	// site-frame points under the current solution; zero without one.
	DepthNearM, DepthFarM float64

	// Estimate is the current solution: complete when HasPose, intrinsics
	// only when the target has been seen but the site has not, zero otherwise.
	Estimate Estimate
	HasPose  bool
	// Revision names the solution; it changes whenever the solution does.
	Revision string
}

type poseSample struct {
	t         time.Time
	pose      geom.Pose2
	elevation float64
	cov       geom.Mat3
}

// sighting is one frame's detections of one robot marker, waiting for the
// robot poses that bracket it.
type sighting struct {
	cam     *camState
	at      time.Time
	sigma   float64
	dets    []spatial.Detection
	inputID string
}

// pass is the sightings of one marker by one camera within one
// PoseErrorWindow: they share the robot's localization error.
type pass struct {
	marker   string
	start    time.Time
	last     time.Time // latest kept sighting
	points   []point
	cov      geom.Mat3 // sum of the robot pose covariances over kept sightings
	n        int
	lastPose geom.Pose2
}

// mid is the middle of the time the pass covers.
func (ps *pass) mid() time.Time { return ps.start.Add(ps.last.Sub(ps.start) / 2) }

// tagStat accumulates the detections of one corner of a static fiducial.
type tagStat struct {
	u, v, sigma2 float64
	n            int
}

type tagObs struct {
	id      string
	corners []tagStat // by point index
}

type camState struct {
	ref           spatial.SensorRef
	nominal       *geom.Intrinsics // declared by the manifest; a starting point only
	lens          *lensPrior       // carried over a Reset
	lensInputs    []string         // the inputs behind lens
	width, height int
	views         [][]Correspondence
	viewSigma     []float64
	passes        []*pass
	current       map[string]*pass // by marker id
	tags          []*tagObs
	tagIndex      map[string]int
	cells         []bool
	first, last   map[string]string // evidence kind -> input id of the first and last frame used
	dirty         bool
	solves        int
	status        Status
}

// AutoCalibrator calibrates fixed cameras from what they happen to see. Feed
// it measurements and robot poses in time order; it is not safe for
// concurrent use. Cameras are discovered from their SensorManifest and only
// those declared spatial.AttachFixed are calibrated: a robot-mounted camera
// has no site pose, and its mount is the business of sensor registration.
//
// Evidence, all harvested without input:
//   - complete views of the planar target (intrinsics),
//   - marker points of robots whose pose estimates were ingested (site pose,
//     intrinsics, clock offset),
//   - static fiducials that appear in the landmark map, when one is set.
//
// ponytail: evidence is kept until Reset and capped (MaxTargetViews,
// MaxPasses); once a cap is reached new evidence of that kind is ignored.
// Upgrade path: replace the least informative view or pass instead.
type AutoCalibrator struct {
	cfg       Config
	target    spatial.TargetGeometry
	markers   map[string]spatial.FiducialGeometry // carried by a robot
	static    map[string]spatial.FiducialGeometry
	landmarks map[string]spatial.Landmark
	cams      map[spatial.SensorRef]*camState
	order     []spatial.SensorRef
	poses     map[spatial.DeviceID][]poseSample
	pending   map[spatial.DeviceID][]sighting
}

// NewAutoCalibrator returns a calibrator that knows the shape of the
// calibration target and of the fiducials it may see. A zero target (empty
// ID) disables target views.
func NewAutoCalibrator(cfg Config, target spatial.TargetGeometry, fiducials []spatial.FiducialGeometry) (*AutoCalibrator, error) {
	a := &AutoCalibrator{
		cfg: cfg.withDefaults(), target: target,
		markers: map[string]spatial.FiducialGeometry{}, static: map[string]spatial.FiducialGeometry{}, landmarks: map[string]spatial.Landmark{},
		cams: map[spatial.SensorRef]*camState{}, poses: map[spatial.DeviceID][]poseSample{}, pending: map[spatial.DeviceID][]sighting{},
	}
	if target.ID != "" {
		if err := target.Validate(); err != nil {
			return nil, err
		}
	}
	for _, f := range fiducials {
		if err := f.Validate(); err != nil {
			return nil, err
		}
		if f.Carrier != "" {
			a.markers[f.ID] = f
		} else {
			a.static[f.ID] = f
		}
	}
	return a, nil
}

// SetLandmarks replaces the landmark map: static fiducials with estimated
// site poses. Detections of them then constrain a camera's pose, each
// landmark with its own survey error of Landmark.UncertaintyM.
func (a *AutoCalibrator) SetLandmarks(landmarks []spatial.Landmark) error {
	next := make(map[string]spatial.Landmark, len(landmarks))
	for _, l := range landmarks {
		if err := l.Validate(); err != nil {
			return err
		}
		next[l.ID] = l
	}
	a.landmarks = next
	for _, c := range a.cams {
		c.dirty = true
	}
	return nil
}

// Cameras lists the cameras under calibration in order of discovery.
func (a *AutoCalibrator) Cameras() []spatial.SensorRef {
	return append([]spatial.SensorRef(nil), a.order...)
}

// Reset discards the evidence and the solution of one camera. The next
// solution has a new Revision.
//
// With keepLens, what the current solution knows about the intrinsics is
// carried over as a prior with its covariance. That is the call for a camera
// found to have moved: a knock changes the mount, not the lens, so only the
// pose has to be found again. Without it the camera starts from nothing, as
// it must after its lens was changed or refocused.
func (a *AutoCalibrator) Reset(device spatial.DeviceID, sensor spatial.SensorID, keepLens bool) {
	ref := spatial.SensorRef{Device: device, Sensor: sensor}
	old := a.cams[ref]
	if old == nil {
		return
	}
	c := newCamState(ref, old.nominal)
	c.width, c.height, c.solves = old.width, old.height, old.solves
	if keepLens {
		c.lens, c.lensInputs = old.lens, old.lensInputs
		if st, _ := a.Status(device, sensor); st.Estimate.Covariance.Rows >= numLens {
			if lens := lensFrom(st.Estimate); lens != nil {
				c.lens, c.lensInputs = lens, a.InputIDs(device, sensor)
			}
		}
	}
	a.cams[ref] = c
	for dev, list := range a.pending {
		kept := list[:0]
		for _, s := range list {
			if s.cam != old {
				kept = append(kept, s)
			}
		}
		a.pending[dev] = kept
	}
}

// lensFrom turns the intrinsic part of an estimate into a prior. The
// covariance is inverted as a correlation matrix: its parameters differ in
// scale by many orders of magnitude.
func lensFrom(e Estimate) *lensPrior {
	in := e.Intrinsics
	lens := &lensPrior{mean: [numLens]float64{in.FX, in.FY, in.CX, in.CY, in.K1, in.K2}}
	var sigma [numLens]float64
	corr := geom.NewMat(numLens, numLens)
	for i := range sigma {
		if sigma[i] = math.Sqrt(e.Covariance.At(i, i)); !(sigma[i] > 0) {
			return nil
		}
	}
	for i := range sigma {
		for j := range sigma {
			corr.Set(i, j, e.Covariance.At(i, j)/(sigma[i]*sigma[j]))
		}
	}
	inv, err := geom.Inverse(corr)
	if err != nil {
		return nil
	}
	for i := range sigma {
		for j := range sigma {
			lens.info[i][j] = (inv.At(i, j) + inv.At(j, i)) / 2 / (sigma[i] * sigma[j])
		}
	}
	return lens
}

func newCamState(ref spatial.SensorRef, nominal *geom.Intrinsics) *camState {
	return &camState{ref: ref, nominal: nominal, current: map[string]*pass{}, tagIndex: map[string]int{}, first: map[string]string{}, last: map[string]string{}, dirty: true}
}

// Ingest takes one measurement. A SensorManifest of a fixed camera puts it
// under calibration; a CameraFrame of such a camera is searched for
// evidence; everything else is ignored. An invalid measurement is an error.
func (a *AutoCalibrator) Ingest(m spatial.Measurement) error {
	switch v := m.(type) {
	case spatial.SensorManifest:
		if err := v.Validate(); err != nil {
			return err
		}
		ref := spatial.SensorRef{Device: v.Device, Sensor: v.Sensor}
		if v.Modality == spatial.KindCamera && v.Attachment.Mode == spatial.AttachFixed && a.cams[ref] == nil {
			a.cams[ref] = newCamState(ref, v.Intrinsics)
			a.order = append(a.order, ref)
		}
	case spatial.CameraFrame:
		c := a.cams[spatial.SensorRef{Device: v.Device, Sensor: v.Sensor}]
		if c == nil {
			return nil
		}
		if err := v.Validate(); err != nil {
			return err
		}
		a.frame(c, v)
	}
	return nil
}

func (c *camState) used(kind, inputID string) {
	if _, ok := c.first[kind]; !ok {
		c.first[kind] = inputID
	}
	c.last[kind] = inputID
	c.dirty = true
}

func (a *AutoCalibrator) cover(c *camState, px geom.Pixel) {
	cols := a.cfg.CoverageCells
	rows := max(1, int(math.Round(float64(cols)*float64(c.height)/float64(c.width))))
	if len(c.cells) != cols*rows {
		c.cells = make([]bool, cols*rows)
	}
	i, j := int(px.U/float64(c.width)*float64(cols)), int(px.V/float64(c.height)*float64(rows))
	if px.U >= 0 && px.V >= 0 && i < cols && j < rows {
		c.cells[j*cols+i] = true
	}
}

func (a *AutoCalibrator) frame(c *camState, f spatial.CameraFrame) {
	if f.Width != c.width || f.Height != c.height {
		if c.width != 0 { // a different image size is a different camera model
			solves := c.solves
			*c = *newCamState(c.ref, nil)
			c.solves = solves
		}
		c.width, c.height = f.Width, f.Height
	}
	sigma := f.PixelSigma
	if !(sigma > 0) {
		sigma = a.cfg.PixelSigma
	}
	id := spatial.InputID(f)
	var view []Correspondence
	seen := map[int]bool{}
	byMarker := map[string][]spatial.Detection{}
	var markerOrder []string
	for _, d := range f.Detections {
		switch {
		case d.Kind == spatial.DetectTargetCorner && a.target.ID != "" && d.ID == a.target.ID:
			if p, ok := a.target.Corner(d.Index); ok && !seen[d.Index] {
				seen[d.Index] = true
				view = append(view, Correspondence{Point: p, Pixel: d.Pixel})
			}
		case d.Kind != spatial.DetectFiducial:
		case a.markers[d.ID].ID != "":
			if byMarker[d.ID] == nil {
				markerOrder = append(markerOrder, d.ID)
			}
			byMarker[d.ID] = append(byMarker[d.ID], d)
		case a.static[d.ID].ID != "" && d.Index < len(a.static[d.ID].Points):
			i, ok := c.tagIndex[d.ID]
			if !ok {
				i = len(c.tags)
				c.tagIndex[d.ID] = i
				c.tags = append(c.tags, &tagObs{id: d.ID, corners: make([]tagStat, len(a.static[d.ID].Points))})
			}
			s := &c.tags[i].corners[d.Index]
			s.u, s.v, s.sigma2, s.n = s.u+d.Pixel.U, s.v+d.Pixel.V, s.sigma2+sigma*sigma, s.n+1
			a.cover(c, d.Pixel)
			c.used("landmark", id)
		}
	}
	// A target view counts only when every corner was found, so that all
	// views share one geometry and a partly hidden target cannot bias it.
	if a.target.ID != "" && len(view) == a.target.Count() && len(c.views) < a.cfg.MaxTargetViews {
		sort.Slice(view, func(i, j int) bool {
			return view[i].Point.Y < view[j].Point.Y || (view[i].Point.Y == view[j].Point.Y && view[i].Point.X < view[j].Point.X)
		})
		shift := math.Inf(1)
		if n := len(c.views); n > 0 {
			shift = 0
			for i := range view {
				shift += view[i].Pixel.Dist(c.views[n-1][i].Pixel) / float64(len(view))
			}
		}
		if shift >= a.cfg.MinViewShiftPx {
			c.views, c.viewSigma = append(c.views, view), append(c.viewSigma, sigma)
			for _, p := range view {
				a.cover(c, p.Pixel)
			}
			c.used("target", id)
		}
	}
	for _, marker := range markerOrder {
		carrier := a.markers[marker].Carrier
		s := sighting{cam: c, at: f.Time, sigma: sigma, dets: byMarker[marker], inputID: id}
		// Forget sightings the pose stream never caught up with.
		list := a.pending[carrier]
		for len(list) > 0 && f.Time.Sub(list[0].at) > a.cfg.PoseBuffer {
			list = list[1:]
		}
		if !a.resolve(carrier, s) {
			list = append(list, s)
		}
		a.pending[carrier] = list
	}
}

// IngestPose takes the pose estimate of a robot at a site instant, as
// localization publishes it, in the site frame. Poses of one device must
// arrive in time order. Marker sightings are placed in the site by
// interpolating between the two estimates that bracket the frame.
func (a *AutoCalibrator) IngestPose(p spatial.DevicePose, at time.Time) error {
	if err := p.Validate(); err != nil {
		return err
	}
	buf := a.poses[p.Device]
	if n := len(buf); n > 0 && !at.After(buf[n-1].t) {
		return fmt.Errorf("calib: pose of %s at %s is not after the previous one", p.Device, at.Format(time.RFC3339Nano))
	}
	buf = append(buf, poseSample{t: at, pose: p.Pose2(), elevation: p.PositionM[1], cov: p.Covariance})
	for len(buf) > 2 && at.Sub(buf[0].t) > a.cfg.PoseBuffer {
		buf = buf[1:]
	}
	a.poses[p.Device] = buf
	list := a.pending[p.Device]
	kept := list[:0]
	for _, s := range list {
		if !a.resolve(p.Device, s) {
			kept = append(kept, s)
		}
	}
	a.pending[p.Device] = kept
	return nil
}

// resolve places a sighting using the carrier's pose buffer. It returns false
// when the poses have not reached the sighting's timestamp yet.
func (a *AutoCalibrator) resolve(carrier spatial.DeviceID, s sighting) bool {
	buf := a.poses[carrier]
	if len(buf) == 0 || s.at.After(buf[len(buf)-1].t) {
		return false
	}
	i := sort.Search(len(buf), func(i int) bool { return !buf[i].t.Before(s.at) }) // first sample at or after
	if i == 0 {
		if buf[0].t.Equal(s.at) && len(buf) > 1 {
			i = 1
		} else {
			return true // older than the buffer: dropped
		}
	}
	p, q := buf[i-1], buf[i]
	span := q.t.Sub(p.t)
	if span > a.cfg.MaxPoseGap {
		return true
	}
	a.harvest(s, p, q, float64(s.at.Sub(p.t))/float64(span), span.Seconds())
	return true
}

func (a *AutoCalibrator) harvest(s sighting, p, q poseSample, u, span float64) {
	c, geometry := s.cam, a.markers[s.dets[0].ID]
	pose := p.pose.Interpolate(q.pose, u)
	ps := c.current[geometry.ID]
	if ps != nil {
		// A marker that has not moved since its last kept sighting, by driving
		// or by turning, adds nothing.
		lever := 0.0
		for _, m := range geometry.Points {
			lever = math.Max(lever, m.XZ().Norm())
		}
		if pose.Pos().Dist(ps.lastPose.Pos())+lever*math.Abs(geom.AngleDiff(pose.Yaw, ps.lastPose.Yaw)) < a.cfg.MinMarkerTravelM {
			return
		}
	}
	if ps == nil || s.at.Sub(ps.start) > a.cfg.PoseErrorWindow {
		if len(c.passes) >= a.cfg.MaxPasses {
			return
		}
		ps = &pass{marker: geometry.ID, start: s.at}
	}
	base := geom.Pose3FromPose2(pose, p.elevation+(q.elevation-p.elevation)*u)
	from, to := geom.Pose3FromPose2(p.pose, p.elevation), geom.Pose3FromPose2(q.pose, q.elevation)
	kept := false
	for _, d := range s.dets {
		if d.Index >= len(geometry.Points) {
			continue
		}
		m := geometry.Points[d.Index]
		// ponytail: the point's motion across a clock offset is linear, at the
		// velocity between the two bracketing poses. A frame taken as the
		// robot stops or starts is off by up to speed x offset and ends up
		// among Status.Outliers; and white jitter in the pose stream, divided
		// by the short interval between two poses, biases the clock offset
		// towards zero (the residual gate then keeps the camera estimating).
		// Upgrade path: keep the pose track per pass, re-interpolate it at the
		// current offset in every solve and take the velocity over a longer
		// baseline.
		ps.points = append(ps.points, point{
			px: d.Pixel, w: 1 / s.sigma, p: base.Apply(m), v: to.Apply(m).Sub(from.Apply(m)).Scale(1 / span),
			tau: 2*float64(s.at.Sub(ps.start))/float64(a.cfg.PoseErrorWindow) - 1, l: base.Rotate(m),
		})
		a.cover(c, d.Pixel)
		kept = true
	}
	if !kept {
		return
	}
	if ps.n == 0 {
		c.passes, c.current[geometry.ID] = append(c.passes, ps), ps
	}
	ps.cov, ps.n, ps.lastPose, ps.last = ps.cov.Add(p.cov), ps.n+1, pose, s.at
	c.used("marker", s.inputID)
}

// upright is the information, per rad², of a rotation that does not happen:
// a robot on a floor has a heading error and no tilt.
const upright = 1e12

// passPriors are the information matrices of the localization error shared
// by a pass. Over site (x, y, z): of the position error at the middle of the
// window, from the mean planar pose covariance, and of its change over half
// a window, which for an error with correlation time T is that covariance
// times 2·(1 − exp(−half/T)). Over a rotation vector: of the heading error,
// which turns the marker about the robot's origin. Nil means the poses are
// declared exact in that respect.
//
// A pass is not alone: the same robot's error at another pass of this camera
// correlates with it by ρ = exp(−Δt/T). Each pass keeps its own block, so
// instead its covariance is multiplied by inflate, the sum of ρ over all
// passes of that marker, itself included: the design effect of correlated
// samples. For evenly spaced passes this leaves them the information a chain
// with that correlation holds about an error common to all of them.
//
// ponytail: the chain is not modelled, only its information about a common
// error; neighbouring windows may differ by more than the correlation allows.
// Upgrade path: one error track per marker with knots at the window
// boundaries and a block-tridiagonal elimination.
func (a *AutoCalibrator) passPriors(ps *pass, inflate float64) (offset, drift, spin *geom.Mat3) {
	c := ps.cov.Scale(1 / float64(ps.n))
	cov := geom.Mat3{c[0], 0, c[1], 0, a.cfg.ElevationSigmaM * a.cfg.ElevationSigmaM, 0, c[3], 0, c[4]}
	if !(cov[0] > 1e-12) || !(cov[8] > 1e-12) {
		return nil, nil, nil
	}
	info, err := cov.Scale(inflate).Inverse()
	if err != nil {
		return nil, nil, nil
	}
	change := 2 * (1 - math.Exp(-float64(a.cfg.PoseErrorWindow)/2/float64(a.cfg.PoseErrorCorrelation)))
	driftInfo := info.Scale(1 / change)
	if yaw := c[8] * inflate; yaw > 1e-12 {
		spin = &geom.Mat3{upright, 0, 0, 0, 1 / yaw, 0, 0, 0, upright}
	}
	return &info, &driftInfo, spin
}

// siteGroups builds the site-frame observation groups of a camera: one per
// marker pass and one per mapped landmark in view.
func (a *AutoCalibrator) siteGroups(c *camState) (groups []group, markerPoints, landmarks int, moving bool) {
	for _, ps := range c.passes {
		inflate := 0.0
		for _, other := range c.passes {
			if other.marker == ps.marker {
				inflate += math.Exp(-math.Abs(float64(ps.mid().Sub(other.mid()))) / float64(a.cfg.PoseErrorCorrelation))
			}
		}
		g := group{points: ps.points}
		g.prior, g.drift, g.spin = a.passPriors(ps, inflate)
		groups = append(groups, g)
		markerPoints += len(ps.points)
		for i := range ps.points {
			moving = moving || ps.points[i].v.Norm2() > 0
		}
	}
	for _, t := range c.tags {
		l, ok := a.landmarks[t.id]
		if !ok {
			continue
		}
		g := group{}
		for i, s := range t.corners {
			if s.n == 0 {
				continue
			}
			n := float64(s.n)
			sigma := math.Max(math.Sqrt(s.sigma2/n)/math.Sqrt(n), a.cfg.PixelSigmaFloor)
			m := a.static[t.id].Points[i]
			g.points = append(g.points, point{px: geom.Pixel{U: s.u / n, V: s.v / n}, w: 1 / sigma, p: l.TagToSite.Apply(m), l: l.TagToSite.Rotate(m)})
		}
		if len(g.points) == 0 {
			continue
		}
		// UncertaintyM is the radial 1σ of the landmark's position and
		// RotationSigmaRad that of its orientation, about any axis.
		if axis := l.UncertaintyM / math.Sqrt(3); axis > 1e-6 {
			w := 1 / (axis * axis)
			g.prior = &geom.Mat3{w, 0, 0, 0, w, 0, 0, 0, w}
			if l.RotationSigmaRad > 1e-6 {
				w = 1 / (l.RotationSigmaRad * l.RotationSigmaRad)
				g.spin = &geom.Mat3{w, 0, 0, 0, w, 0, 0, 0, w}
			}
		}
		groups = append(groups, g)
		landmarks++
	}
	return groups, markerPoints, landmarks, moving
}

// Status returns the calibration state of a camera, solving first when new
// evidence has arrived since the last call. ok is false for a camera that is
// not under calibration.
func (a *AutoCalibrator) Status(device spatial.DeviceID, sensor spatial.SensorID) (Status, bool) {
	c := a.cams[spatial.SensorRef{Device: device, Sensor: sensor}]
	if c == nil {
		return Status{}, false
	}
	if c.dirty {
		c.dirty = false
		c.solves++
		c.status = a.solve(c)
		c.status.Revision = Feature + "-" + strconv.Itoa(c.solves)
	}
	return c.status, true
}

func (a *AutoCalibrator) solve(c *camState) Status {
	cfg := a.cfg
	st := Status{Device: c.ref.Device, Sensor: c.ref.Sensor, State: spatial.CalibrationEstimating, TargetViews: len(c.views), MarkerPasses: len(c.passes)}
	covered := 0
	for _, hit := range c.cells {
		if hit {
			covered++
		}
	}
	if len(c.cells) > 0 {
		st.ImageCoverage = float64(covered) / float64(len(c.cells))
	}
	fail := func(r Reason, format string, args ...any) Status {
		st.Reason, st.Detail = r, fmt.Sprintf(format, args...)
		return st
	}
	site, markerPoints, landmarks, moving := a.siteGroups(c)
	st.MarkerPoints, st.Landmarks = markerPoints, landmarks
	sitePoints := 0
	for i := range site {
		sitePoints += len(site[i].points)
	}
	if len(c.views) == 0 && sitePoints == 0 {
		return fail(ReasonNoEvidence, "no target view, robot marker or mapped landmark seen yet")
	}

	// Intrinsics from the target, when it has been shown.
	var known *geom.Intrinsics
	var viewPoses []geom.Pose3
	targetNote := ""
	if len(c.views) > 0 {
		e, poses, err := calibrateIntrinsics(c.views, c.width, c.height, cfg)
		if err == nil {
			st.Estimate, known, viewPoses = e, &e.Intrinsics, poses
		} else {
			targetNote = "; target views unusable: " + err.Error()
		}
	}
	if sitePoints < cfg.MinSitePoints {
		return fail(ReasonNoSiteEvidence, "%d site-frame correspondences, need %d%s", sitePoints, cfg.MinSitePoints, targetNote)
	}
	// A lens carried over a Reset was fitted on the coverage of its time.
	if c.lens == nil && st.ImageCoverage < cfg.MinImageCoverage {
		return fail(ReasonCoverage, "evidence covers %.0f%% of the image, need %.0f%%", st.ImageCoverage*100, cfg.MinImageCoverage*100)
	}
	switch {
	case known != nil:
	case c.lens != nil:
		m := c.lens.mean
		known = &geom.Intrinsics{FX: m[0], FY: m[1], CX: m[2], CY: m[3], K1: m[4], K2: m[5], Width: c.width, Height: c.height}
	default:
		// Declared intrinsics are a starting point for the search, nothing
		// more: they are refined with everything else and carry no weight.
		known = c.nominal
	}

	// A starting camera from the site points as they stand, then everything
	// jointly.
	var flat []Correspondence
	for i := range site {
		for _, p := range site[i].points {
			flat = append(flat, Correspondence{Point: p.p, Pixel: p.px})
		}
	}
	cam, err := ransacCamera(flat, known, c.width, c.height, cfg)
	if err != nil {
		return fail(ReasonUnconstrained, "no camera agrees with the site-frame correspondences: %v%s", err, targetNote)
	}
	b := &bundle{huber: cfg.HuberSigmas, iters: cfg.MaxIterations, absolute: true, lens: c.lens}
	b.free = append(append(b.free, freeIntrinsics...), freePose...)
	if moving {
		b.free = append(b.free, parDT)
	}
	for i := range viewPoses {
		b.groups = append(b.groups, viewGroup(c.views[i], c.viewSigma[i]))
	}
	b.groups = append(b.groups, site...)
	start := newState(cam, len(b.groups))
	copy(start.poses, viewPoses)
	res, err := b.solve(start)
	if err == nil {
		// Site points the robust solution cannot explain are set aside and the
		// rest is solved again, so that the reported residual and covariance
		// are those of the evidence the solution rests on. Target views keep
		// all their corners: a view is whole or absent.
		k := 0
		for gi := range b.groups {
			g := &b.groups[gi]
			if g.rigid {
				k += len(g.points)
				continue
			}
			kept := make([]point, 0, len(g.points))
			for _, p := range g.points {
				if res.dist[k]*p.w <= cfg.OutlierSigmas {
					kept = append(kept, p)
				}
				k++
			}
			st.Outliers += len(g.points) - len(kept)
			g.points = kept
		}
		if st.Outliers > 0 {
			res, err = b.solve(res.state)
		}
	}
	if err != nil && !errors.Is(err, geom.ErrNoConvergence) {
		return fail(ReasonUnconstrained, "joint solve: %v%s", err, targetNote)
	}
	if res.cov.Rows == 0 {
		return fail(ReasonUnconstrained, "joint solve did not converge%s", targetNote)
	}
	e := b.estimate(res)
	if moving {
		// The offset is measured against the timestamps of the pose stream
		// and is no better known than they are.
		e.TimeOffsetSigma = time.Duration(math.Hypot(float64(e.TimeOffsetSigma), float64(cfg.PoseTimeSigma)))
		last := e.Covariance.Rows - 1 // parDT is the last free parameter
		e.Covariance.Set(last, last, e.Covariance.At(last, last)+cfg.PoseTimeSigma.Seconds()*cfg.PoseTimeSigma.Seconds())
	}
	st.Estimate, st.HasPose = e, true

	var depths []float64
	toCam := e.CameraToSite.Inverse()
	for i := range site {
		for _, p := range site[i].points {
			depths = append(depths, toCam.Apply(p.p).Z)
		}
	}
	sort.Float64s(depths)
	st.DepthNearM, st.DepthFarM = depths[len(depths)/20], depths[len(depths)-1-len(depths)/20]

	const deg = 180 / math.Pi
	switch {
	case err != nil:
		return fail(ReasonUnconstrained, "joint solve did not converge in %d iterations%s", cfg.MaxIterations, targetNote)
	case float64(st.Outliers) > cfg.MaxOutlierFraction*float64(sitePoints):
		return fail(ReasonResidual, "%d of %d site-frame points disagree with the solution, limit %.0f%%%s", st.Outliers, sitePoints, cfg.MaxOutlierFraction*100, targetNote)
	case len(viewPoses) == 0 && c.lens == nil && !(st.DepthNearM > 0 && st.DepthFarM/st.DepthNearM >= cfg.MinDepthRatio):
		return fail(ReasonCoverage, "site points span depths %.2f to %.2f m, need a ratio of %.2f without target views%s", st.DepthNearM, st.DepthFarM, cfg.MinDepthRatio, targetNote)
	case e.FocalSigma > cfg.MaxFocalSigma:
		return fail(ReasonUncertain, "focal length 1σ %.2f%%, limit %.2f%%%s", e.FocalSigma*100, cfg.MaxFocalSigma*100, targetNote)
	case e.PrincipalSigmaPx > cfg.MaxPrincipalSigmaPx:
		return fail(ReasonUncertain, "principal point 1σ %.2f px, limit %.2f px%s", e.PrincipalSigmaPx, cfg.MaxPrincipalSigmaPx, targetNote)
	case e.TranslationSigmaM > cfg.MaxTranslationSigmaM:
		return fail(ReasonUncertain, "position 1σ %.3f m, limit %.3f m%s", e.TranslationSigmaM, cfg.MaxTranslationSigmaM, targetNote)
	case e.RotationSigmaRad > cfg.MaxRotationSigmaRad:
		return fail(ReasonUncertain, "rotation 1σ %.2f°, limit %.2f°%s", e.RotationSigmaRad*deg, cfg.MaxRotationSigmaRad*deg, targetNote)
	case e.Residuals.RMSE > cfg.MaxReprojectionRMSE:
		return fail(ReasonResidual, "reprojection RMSE %.2f px, limit %.2f px%s", e.Residuals.RMSE, cfg.MaxReprojectionRMSE, targetNote)
	}
	st.State = spatial.CalibrationValid
	return st
}

// Calibration returns the camera's calibration as the payload to publish. ok
// is false until there is a site pose to report. The payload's State is
// CalibrationValid only when Status says so; an estimating payload carries
// the current best solution with its honest uncertainty and must not be used
// as a calibration.
func (a *AutoCalibrator) Calibration(device spatial.DeviceID, sensor spatial.SensorID) (spatial.Calibration, bool) {
	st, ok := a.Status(device, sensor)
	if !ok || !st.HasPose {
		return spatial.Calibration{}, false
	}
	c := a.cams[spatial.SensorRef{Device: device, Sensor: sensor}]
	e := st.Estimate
	var evidence []string
	if c.lens != nil {
		evidence = append(evidence, "lens")
	}
	for _, kind := range []string{"target", "marker", "landmark"} {
		if _, used := c.first[kind]; used && (kind != "landmark" || st.Landmarks > 0) {
			evidence = append(evidence, kind)
		}
	}
	in := e.Intrinsics
	loc := spatial.At(e.CameraToSite.T, e.TranslationSigmaM)
	return spatial.Calibration{
		Device: device, Sensor: sensor, Kind: spatial.KindCamera, Intrinsics: &in,
		SensorToParent: e.CameraToSite, Parent: spatial.ParentSite,
		TranslationSigmaM: e.TranslationSigmaM, RotationSigmaRad: e.RotationSigmaRad,
		ReprojectionRMSE: e.Residuals.RMSE, Residuals: e.Residuals, ResidualUnit: "px",
		Method: "joint resection: " + strings.Join(evidence, "+"), Revision: st.Revision, State: st.State,
		TimeOffset: e.TimeOffset, TimeOffsetSigma: e.TimeOffsetSigma, Location: &loc,
	}, true
}

// InputIDs lists the measurements the camera's calibration rests on, as
// spatial.InputID values: the first and the last frame that contributed each
// kind of evidence, and those behind a lens carried over a Reset. An engine
// turns them into world.Input ancestry.
func (a *AutoCalibrator) InputIDs(device spatial.DeviceID, sensor spatial.SensorID) []string {
	c := a.cams[spatial.SensorRef{Device: device, Sensor: sensor}]
	if c == nil {
		return nil
	}
	ids := append([]string(nil), c.lensInputs...)
	for _, kind := range []string{"target", "marker", "landmark"} {
		if id, ok := c.first[kind]; ok && (kind != "landmark" || c.status.Landmarks > 0) {
			ids = append(ids, id, c.last[kind])
		}
	}
	sort.Strings(ids)
	return slices.Compact(ids)
}
