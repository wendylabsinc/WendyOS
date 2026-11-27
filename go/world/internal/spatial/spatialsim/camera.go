package spatialsim

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

const (
	minBoxPixels = 8.0 // an object box smaller than this on either side is not detected
	targetFacing = 0.3 // smallest cosine between the target normal and the view ray
)

// tagPoints are the corners of a wall tag in its own frame: x along the
// wall, y up, z out of the wall. Detection.Index refers to this order.
var tagPoints = []geom.Vec3{
	{X: -tagSide / 2, Y: -tagSide / 2}, {X: tagSide / 2, Y: -tagSide / 2}, {X: tagSide / 2, Y: tagSide / 2}, {X: -tagSide / 2, Y: tagSide / 2},
}

// cameraPose returns the optical-frame-to-site pose of a device's camera and
// the level it looks at.
func (s *Site) cameraPose(d *device, t time.Duration) (geom.Pose3, *level) {
	if d.route == nil {
		return d.cameraAt(t), d.level
	}
	base, e, lv := s.place(d, t)
	return geom.Pose3FromPose2(base, e).Compose(d.cameraAt(t)), lv
}

// boardPose is the target-to-site pose during a show. The target sweeps a
// Lissajous path through the camera's view at 1.3 to 3.1 m and tilts up to
// about 35° on two axes, relative to where the camera was when the show began.
func (s *Site) boardPose(sh TargetShow, t time.Duration) (geom.Pose3, *level) {
	cam, lv := s.cameraPose(s.byID[sh.Camera], sh.Start)
	tau := (t - sh.Start).Seconds()
	wave := func(period, phase float64) float64 { return math.Sin(2*math.Pi*tau/period + phase) }
	z := 2.2 + 0.9*wave(13, 0)
	centre := geom.Vec3{X: 0.4 * z * wave(7.3, 0), Y: 0.2 * z * wave(5.1, 0), Z: z}
	rot := geom.RotX(0.6 * wave(9.7, 0)).Mul(geom.RotY(0.6 * wave(6.1, 1))).Mul(geom.RotZ(0.3 * wave(11.3, 0)))
	half := geom.Vec3{X: float64(s.target.Cols-1) * s.target.Spacing / 2, Y: float64(s.target.Rows-1) * s.target.Spacing / 2}
	return cam.Compose(geom.Pose3{R: rot, T: centre.Sub(rot.MulVec(half))}), lv
}

func jitter(r *rand.Rand, px geom.Pixel, sigma float64) geom.Pixel {
	return geom.Pixel{U: px.U + sigma*r.NormFloat64(), V: px.V + sigma*r.NormFloat64()}
}

// CameraFrame is the detector output of a camera at the instant, through its
// true intrinsics and pose:
//   - fiducial points of other robots' markers and of wall tags,
//   - every corner of the calibration target when the whole target is in view
//     and faces the camera,
//   - class-labelled objects (furniture, actors, other devices) with a foot
//     pixel and a box. Object detections carry no id.
//
// Detections are ordered markers, tags, target, objects, false positive.
func (s *Site) CameraFrame(dev spatial.DeviceID, at time.Time) (spatial.CameraFrame, bool) {
	d, sn, t := s.sensor(dev, spatial.KindCamera, at)
	if d == nil {
		return spatial.CameraFrame{}, false
	}
	c := &s.cfg
	in := d.intrinsics
	cam, lv := s.cameraPose(d, t)
	toCam := cam.Inverse()
	sc := s.scene(lv, t)
	r := s.rng(d.id, "camera", int64(t))
	self := string(d.id)
	frame := spatial.CameraFrame{Header: s.header(d, sn, t), Width: in.Width, Height: in.Height, PixelSigma: c.PixelSigma, Detections: []spatial.Detection{}}
	project := func(p geom.Vec3) (geom.Pixel, bool) {
		if p.Dist(cam.T) > c.DetectRange {
			return geom.Pixel{}, false
		}
		px, ok := in.Project(toCam.Apply(p))
		return px, ok && in.InImage(px)
	}
	point := func(kind spatial.DetectionKind, id string, index int, px geom.Pixel) {
		frame.Detections = append(frame.Detections, spatial.Detection{Kind: kind, ID: id, Index: index, Pixel: jitter(r, px, c.PixelSigma)})
	}

	for _, o := range s.devices {
		if o == d || len(o.marker) == 0 || t < o.start {
			continue
		}
		pose, e, l := s.place(o, t)
		if l != lv {
			continue
		}
		base := geom.Pose3FromPose2(pose, e)
		for i, m := range o.marker {
			p := base.Apply(m)
			if px, ok := project(p); ok && !sc.blocked(cam.T, p, self, string(o.id)) && r.Float64() >= c.FiducialMissRate {
				point(spatial.DetectFiducial, MarkerID(o.id), i, px)
			}
		}
	}

	for _, tag := range lv.tags {
		view := cam.T.Sub(tag.Pose.T)
		normal := tag.Pose.Rotate(geom.Vec3{Z: 1})
		if view.Dot(normal) < c.DetectMinFacing*view.Norm() || sc.blocked(cam.T, tag.Pose.T, self, "") {
			continue
		}
		var px [4]geom.Pixel
		seen := true
		for i, m := range tagPoints {
			var ok bool
			px[i], ok = project(tag.Pose.Apply(m))
			seen = seen && ok
		}
		if seen && r.Float64() >= c.FiducialMissRate {
			for i := range px {
				point(spatial.DetectFiducial, tag.ID, i, px[i])
			}
		}
	}

	for _, sh := range s.shows {
		if t < sh.Start || t >= sh.Start+sh.Duration {
			continue
		}
		board, l := s.boardPose(sh, t)
		centre := board.Apply(geom.Vec3{X: float64(s.target.Cols-1) * s.target.Spacing / 2, Y: float64(s.target.Rows-1) * s.target.Spacing / 2})
		view := centre.Sub(cam.T)
		if l != lv || view.Dot(board.Rotate(geom.Vec3{Z: 1})) < targetFacing*view.Norm() || sc.walled(cam.T.XZ(), centre.XZ()) {
			continue
		}
		px := make([]geom.Pixel, s.target.Count())
		seen := true
		for i := range px {
			corner, _ := s.target.Corner(i)
			var ok bool
			px[i], ok = project(board.Apply(corner))
			seen = seen && ok
		}
		if seen {
			for i := range px {
				point(spatial.DetectTargetCorner, s.target.ID, i, px[i])
			}
		}
	}

	w, h := float64(in.Width), float64(in.Height)
	for i := range sc.solids {
		so := &sc.solids[i]
		if so.id == self {
			continue
		}
		foot, ok := project(so.shape.Centre.Lift(so.base))
		if !ok {
			continue
		}
		lo, hi := geom.Pixel{U: math.Inf(1), V: math.Inf(1)}, geom.Pixel{U: math.Inf(-1), V: math.Inf(-1)}
		for _, corner := range so.corners {
			for _, y := range [2]float64{so.base, so.base + so.shape.Height} {
				px, front := in.Project(toCam.Apply(corner.Lift(y)))
				ok = ok && front
				lo = geom.Pixel{U: math.Min(lo.U, px.U), V: math.Min(lo.V, px.V)}
				hi = geom.Pixel{U: math.Max(hi.U, px.U), V: math.Max(hi.V, px.V)}
			}
		}
		if !ok || sc.blocked(cam.T, so.shape.Centre.Lift(so.base+so.shape.Height/2), self, so.id) {
			continue
		}
		clamp := func(px geom.Pixel) geom.Pixel {
			return geom.Pixel{U: math.Max(0, math.Min(w, px.U)), V: math.Max(0, math.Min(h, px.V))}
		}
		box := spatial.BBox{Min: clamp(jitter(r, lo, c.ObjectPixelSigma)), Max: clamp(jitter(r, hi, c.ObjectPixelSigma))}
		det := spatial.Detection{Kind: spatial.DetectObject, Pixel: jitter(r, foot, c.ObjectPixelSigma), Class: so.class, Confidence: 0.7 + 0.29*r.Float64(), BBox: &box}
		missed, confused := r.Float64() < c.ObjectMissRate, r.Float64() < c.ClassConfusionRate
		if missed || box.Max.U-box.Min.U < minBoxPixels || box.Max.V-box.Min.V < minBoxPixels {
			continue
		}
		if confused && len(s.classes) > 1 {
			for det.Class == so.class {
				det.Class = s.classes[r.IntN(len(s.classes))]
			}
		}
		frame.Detections = append(frame.Detections, det)
	}

	if r.Float64() < c.FalsePositiveRate && len(s.classes) > 0 {
		bw, bh := 20+100*r.Float64(), 20+180*r.Float64()
		u, v := r.Float64()*(w-bw), r.Float64()*(h-bh)
		frame.Detections = append(frame.Detections, spatial.Detection{
			Kind: spatial.DetectObject, Pixel: geom.Pixel{U: u + bw/2, V: v + bh}, Class: s.classes[r.IntN(len(s.classes))], Confidence: 0.3 + 0.5*r.Float64(),
			BBox: &spatial.BBox{Min: geom.Pixel{U: u, V: v}, Max: geom.Pixel{U: u + bw, V: v + bh}},
		})
	}
	return frame, true
}
