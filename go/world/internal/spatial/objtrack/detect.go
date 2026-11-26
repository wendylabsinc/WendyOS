package objtrack

import (
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Detection is one object candidate from one sensor sample, in the site
// frame. LidarDetections and CameraDetections fill the geometry and the
// class; Tracker.Ingest adds the rest. A caller with its own detector fills
// all of it and calls Tracker.Observe.
type Detection struct {
	Position geom.Vec2 // centre of the body on the floor plane
	Cov      geom.Cov2 // covariance of Position, m²
	RadiusM  float64   // half-width of the body as seen; 0 when unknown
	// Bias says how much of Cov is not noise but an error the sensor makes
	// in every sample alike, its calibration: a row-major 2×3 matrix B such
	// that this error moves Position by B·u, where u is the same unknown
	// triple (zero mean, unit variance, independent) for every detection by
	// the sensor. B·Bᵀ is part of Cov. The tracker estimates u for a sensor
	// from its detections of localized devices and removes it from all of
	// them. Zero when the sensor has no such error or it is not modelled.
	Bias [6]float64

	Class      string  // detector class; empty for lidar
	Confidence float64 // detector confidence of Class in [0, 1]

	Floor      spatial.FloorID   // floor the body stands on
	ElevationM float64           // World elevation of that floor plane
	Sensor     spatial.SensorRef // the observing sensor
	Input      string            // spatial.InputID of the measurement, for ancestry; may be empty
}

// PoseSigma is the 1σ uncertainty of where a sensor sat when it sampled:
// its position along each axis and its rotation.
type PoseSigma struct {
	PositionM   float64
	RotationRad float64
}

// Background is the static structure of one level: what a detection must not
// be explained by.
type Background struct {
	grids  []*grid.Grid
	fields []*grid.Field
}

// NewBackground builds the background from the occupancy maps of the floors
// that share a level (one map for most levels; a ground floor and the yard
// outside it are two). The maps are not fused: a cell is structure when any
// of them holds it occupied, because a thin wall that one mapper saw square
// on is erased by the grazing beams of another when their evidence is
// summed. The grids must not change afterwards.
func NewBackground(grids ...*grid.Grid) (*Background, error) {
	if len(grids) == 0 {
		return nil, ErrNoMap
	}
	b := &Background{}
	for _, g := range grids {
		if g == nil {
			return nil, ErrNoMap
		}
		f, err := g.Field()
		if err != nil {
			return nil, err
		}
		b.grids, b.fields = append(b.grids, g), append(b.fields, f)
	}
	return b, nil
}

// Static reports whether an occupied cell lies within tolerance of p.
func (b *Background) Static(p geom.Vec2, tolerance float64) bool {
	for _, f := range b.fields {
		if f.Distance(p) < tolerance {
			return true
		}
	}
	return false
}

// Free reports whether a map has seen p as free space. Where none has, a
// lidar return says nothing about what moves.
func (b *Background) Free(p geom.Vec2) bool {
	for _, g := range b.grids {
		if g.StateAt(p) == grid.Free {
			return true
		}
	}
	return false
}

// LidarDetections subtracts the static map from one scan and clusters what
// is left. sensor is the pose of the scan frame in the site frame (device
// pose composed with the lidar mount) and sigma its uncertainty.
//
// A return is dropped as static when an occupied cell lies within
// StaticMarginM plus StaticSigmas standard deviations of the return's
// position, and as uninformative when the map has not seen its cell free.
// The remaining returns are joined in beam order. A cluster becomes a
// detection when it has at least MinClusterPoints returns, is between
// MinRadiusM and MaxRadiusM wide, does not continue into structure and is
// not cut off by a nearer return at either edge (a partial view does not
// locate the body's centre, so it is refused rather than guessed).
// Position is the centroid pushed DepthFactor half-widths away from the
// sensor, since the returns lie on the near surface. Floor, ElevationM,
// Sensor and Input are left empty.
//
// It returns ErrNoMap without a background: with nothing to subtract, every
// wall would be a detection.
func LidarDetections(s spatial.Scan, sensor geom.Pose2, sigma PoseSigma, bg *Background, cfg Config) ([]Detection, error) {
	if bg == nil {
		return nil, ErrNoMap
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	type hit struct {
		p      geom.Vec2
		r      float64
		beam   int
		moving bool
	}
	variance := func(r float64) float64 {
		return s.RangeSigma*s.RangeSigma + sigma.PositionM*sigma.PositionM + r*r*sigma.RotationRad*sigma.RotationRad
	}
	gap := func(r float64) float64 { return math.Max(cfg.ClusterGapM, 2.5*r*math.Abs(s.AngleIncrement)) }
	var hits []hit
	for i := range s.Ranges {
		a, r, state := s.Beam(i)
		if state != spatial.BeamHit {
			continue
		}
		p := sensor.Apply(geom.Heading(a).Scale(r))
		static := bg.Static(p, cfg.StaticMarginM+cfg.StaticSigmas*math.Sqrt(variance(r)))
		hits = append(hits, hit{p, r, i, !static && bg.Free(p)})
	}
	// Consecutive moving returns closer than the gap form a cluster. One that
	// continues, within the gap, into a return the map explains (or cannot
	// judge) is the unmapped edge of that structure: a door jamb, a closed
	// door, the end of a wall the mapper's beams grazed away.
	type cluster struct {
		first, last int // indices into hits; last may run past len(hits) across the seam
		attached    bool
	}
	near := func(i, j int) bool { return hits[i].p.Dist(hits[j].p) <= gap(math.Max(hits[i].r, hits[j].r)) }
	var clusters []cluster
	for i, h := range hits {
		if !h.moving {
			if n := len(clusters); n > 0 && clusters[n-1].last == i-1 && near(i-1, i) {
				clusters[n-1].attached = true
			}
			continue
		}
		if n := len(clusters); n > 0 && clusters[n-1].last == i-1 && near(i-1, i) {
			clusters[n-1].last = i
			continue
		}
		clusters = append(clusters, cluster{first: i, last: i, attached: i > 0 && !hits[i-1].moving && near(i-1, i)})
	}
	// A full sweep meets itself: the last return is next to the first.
	m := len(hits)
	full := math.Abs(float64(len(s.Ranges))*s.AngleIncrement) > 2*math.Pi-1e-6
	if n := len(clusters); n > 0 && m > 1 && full && near(m-1, 0) {
		head, tail := &clusters[0], &clusters[n-1]
		switch {
		case head.first == 0 && tail.last == m-1 && n > 1:
			tail.last, tail.attached = head.last+m, tail.attached || head.attached
			clusters = clusters[1:]
		case head.first == 0 && !hits[m-1].moving:
			head.attached = true
		case tail.last == m-1 && !hits[0].moving:
			tail.attached = true
		}
	}
	// occluded reports whether the return next to a cluster's edge (the
	// neighbouring beam, allowing one dropout) is nearer than that edge:
	// then the body continues behind whatever returned it, and the visible
	// part does not say where its centre is.
	occluded := func(edge, neighbour int) bool {
		if neighbour < 0 || neighbour >= m {
			if !full {
				return false
			}
			neighbour = (neighbour + m) % m
		}
		e, o := hits[edge%m], hits[neighbour]
		apart := e.beam - o.beam
		if apart < 0 {
			apart = -apart
		}
		if full {
			apart = min(apart, len(s.Ranges)-apart)
		}
		return apart <= 2 && o.r < e.r
	}

	origin := sensor.Pos()
	var out []Detection
	for _, c := range clusters {
		n := c.last - c.first + 1
		if c.attached || n < cfg.MinClusterPoints || n >= m || occluded(c.first, c.first-1) || occluded(c.last, c.last%m+1) {
			continue
		}
		at := func(k int) geom.Vec2 { return hits[(c.first+k)%len(hits)].p }
		var centroid geom.Vec2
		for k := 0; k < n; k++ {
			centroid = centroid.Add(at(k))
		}
		centroid = centroid.Scale(1 / float64(n))
		ray := centroid.Sub(origin)
		rng := ray.Norm()
		if rng == 0 {
			continue
		}
		ray = ray.Scale(1 / rng)
		half, extent := 0.0, 0.0
		for k := 0; k < n; k++ {
			d := at(k).Sub(centroid)
			half = math.Max(half, math.Abs(d.Cross(ray)))
			extent = math.Max(extent, d.Norm())
		}
		// A body is at least as wide as the beams that found it are apart.
		radius := half + rng*math.Abs(s.AngleIncrement)/2
		if extent > cfg.MaxRadiusM || radius < cfg.MinRadiusM {
			continue
		}
		// How far behind its near surface a body's centre lies depends on a
		// shape the scan does not show: ShapeSigma radii along the ray, half
		// of that across it.
		shape := cfg.ShapeSigma * cfg.ShapeSigma * radius * radius
		across := variance(rng) + shape/4
		along := 3 * shape / 4 // on top of across
		out = append(out, Detection{
			Position: centroid.Add(ray.Scale(cfg.DepthFactor * radius)),
			Cov:      geom.Cov2{across + along*ray.X*ray.X, along * ray.X * ray.Z, along * ray.X * ray.Z, across + along*ray.Z*ray.Z},
			RadiusM:  radius,
		})
	}
	return out, nil
}

// Camera is a calibrated camera at the instant of a frame.
type Camera struct {
	Intrinsics geom.Intrinsics
	ToSite     geom.Pose3 // optical frame to site
	Sigma      PoseSigma  // uncertainty of ToSite
	// FloorElevationM is the World elevation of the floor the camera looks
	// at: the plane foot pixels are placed on.
	FloorElevationM float64
}

// CameraDetections places every object detection of a frame on the floor.
// The foot pixel's ray is intersected with the floor plane; its covariance
// is the pixel noise and the camera's rotation uncertainty carried through
// that projection (which stretches along the view direction as the ray
// flattens), plus the camera's position uncertainty (its height error
// stretched the same way) and half the ShapeSigma share of the radius.
// RadiusM is half the box width at the foot's range.
//
// Bias is the rotation uncertainty's share: how far a rotation of the camera
// by Sigma.RotationRad about each of its three optical axes moves this floor
// point. That error is the same in every frame.
//
// Detections under MinConfidence, rays that miss the floor, positions less
// certain than MaxDetectionSigmaM and, when bg is not nil, positions within
// StaticMarginM of an occupied cell are dropped. Fiducial and target
// detections are ignored. Floor, ElevationM, Sensor and Input are left empty.
func CameraDetections(f spatial.CameraFrame, cam Camera, bg *Background, cfg Config) ([]Detection, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := cam.Intrinsics.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	in := cam.Intrinsics
	foot := func(toSite geom.Pose3, px geom.Pixel) (geom.Vec3, bool) {
		origin, dir, err := in.PixelRay(toSite, px)
		if err != nil {
			return geom.Vec3{}, false
		}
		return geom.RayPlaneY(origin, dir, cam.FloorElevationM)
	}
	// The camera turned by a small angle about each optical axis, to see
	// where a rotation error puts a floor point.
	const turn = 1e-4
	var turned [3]geom.Pose3
	for k, axis := range []geom.Vec3{{X: turn}, {Y: turn}, {Z: turn}} {
		turned[k] = geom.Pose3{R: cam.ToSite.R.Mul(geom.RotExp(axis)), T: cam.ToSite.T}
	}
	pixelSigma := math.Max(cfg.FootPixelSigma, f.PixelSigma)
	pixelVar := pixelSigma * pixelSigma
	posVar := cam.Sigma.PositionM * cam.Sigma.PositionM
	var out []Detection
next:
	for _, d := range f.Detections {
		if d.Kind != spatial.DetectObject || d.Confidence < cfg.MinConfidence {
			continue
		}
		p, ok := foot(cam.ToSite, d.Pixel)
		pu, okU := foot(cam.ToSite, geom.Pixel{U: d.Pixel.U + 1, V: d.Pixel.V})
		pv, okV := foot(cam.ToSite, geom.Pixel{U: d.Pixel.U, V: d.Pixel.V + 1})
		if !ok || !okU || !okV {
			continue
		}
		ju, jv := pu.Sub(p).XZ(), pv.Sub(p).XZ() // metres on the floor per pixel
		var bias [6]float64
		for k := range turned {
			q, ok := foot(turned[k], d.Pixel)
			if !ok {
				continue next
			}
			j := q.Sub(p).XZ().Scale(cam.Sigma.RotationRad / turn)
			bias[k], bias[3+k] = j.X, j.Z
		}
		radius := 0.0
		if d.BBox != nil {
			radius = (d.BBox.Max.U - d.BBox.Min.U) / 2 * p.Dist(cam.ToSite.T) / in.FX
		}
		extra := posVar + cfg.ShapeSigma*cfg.ShapeSigma*radius*radius/4
		// An error in the camera's height moves the floor point along the
		// view direction by range over height times as much.
		view := p.Sub(cam.ToSite.T)
		lever := view.XZ().Scale(1 / view.Y)
		cov := geom.Cov2{
			pixelVar*(ju.X*ju.X+jv.X*jv.X) + extra + posVar*lever.X*lever.X, pixelVar*(ju.X*ju.Z+jv.X*jv.Z) + posVar*lever.X*lever.Z,
			pixelVar*(ju.X*ju.Z+jv.X*jv.Z) + posVar*lever.X*lever.Z, pixelVar*(ju.Z*ju.Z+jv.Z*jv.Z) + extra + posVar*lever.Z*lever.Z,
		}.Add(project(bias, geom.Identity3()))
		if !(cov.MaxSigma() <= cfg.MaxDetectionSigmaM) || (bg != nil && bg.Static(p.XZ(), cfg.StaticMarginM)) {
			continue
		}
		out = append(out, Detection{Position: p.XZ(), Cov: cov, RadiusM: radius, Class: d.Class, Confidence: d.Confidence, Bias: bias})
	}
	return out, nil
}

// project returns B·P·Bᵀ for a 2×3 B and a symmetric 3×3 P.
func project(b [6]float64, p geom.Mat3) geom.Cov2 {
	var bp [6]float64
	for i := 0; i < 2; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				bp[3*i+j] += b[3*i+k] * p[3*k+j]
			}
		}
	}
	var out geom.Cov2
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			for k := 0; k < 3; k++ {
				out[2*i+j] += bp[3*i+k] * b[3*j+k]
			}
		}
	}
	return out
}

// siteTime converts a sensor-clock timestamp to site time.
func siteTime(sensorClock time.Time, offset time.Duration) time.Time { return sensorClock.Add(-offset) }
