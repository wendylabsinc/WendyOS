package twinrobot

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

// ObjectView binds camera evidence to measured geometry and the controller
// generation. It cannot be constructed from model-supplied coordinates.
type ObjectView struct {
	at                          time.Time
	pose                        sitenav.Pose
	alignment                   sitenav.Alignment
	frame                       string
	sequence, generation, epoch uint64
	samples                     []sitenav.Vertex
}

type ObjectTarget struct {
	Frame       string  `json:"frame"`
	X           float64 `json:"x"`
	Z           float64 `json:"z"`
	ApproachX   float64 `json:"approach_x"`
	ApproachZ   float64 `json:"approach_z"`
	RangeMeters float64 `json:"range_meters"`
	Support     int     `json:"support"`
	Calibration string  `json:"calibration"`
}

// Community front_camera_720 preset, normalized to preserve aspect-ratio
// resizing. The MCP capture path enforces native 1280x720 before decoding.
// This is the existing Apple RobotLensCalibration.go2Preset: community
// go2_ros2_sdk front_camera_720.yaml (b440609591a249e7bdd4bbc88e056a3660575447),
// with URDF mount (157ff950351e5f535e03c5d85a16b7d594b3dbcc). It is an estimate,
// not a measured calibration of the connected robot.
const objectFX, objectFY = 864.39938 / 1280, 863.73849 / 720
const objectCX, objectCY = 639.19798 / 1280, 373.28118 / 720
const objectCameraForward, objectCameraLeft, objectCameraAboveBase = .32715, -.00003, .04297

func validObjectPose(p sitenav.Pose) bool {
	for _, value := range []float64{p.Position.X, p.Position.Y, p.Height, p.Yaw, p.Roll, p.Pitch} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	// Keep the odometry receiver's upright/standing envelope. Accounting for
	// measured attitude is not permission to range from a fallen robot.
	return p.Position.Valid() && p.Height >= .10 && p.Height <= .75 && math.Cos(p.Roll)*math.Cos(p.Pitch) >= .90
}

func objectPostureChanged(a, b sitenav.Pose) bool {
	return math.Abs(a.Height-b.Height) > .03 ||
		math.Abs(math.Remainder(a.Roll-b.Roll, 2*math.Pi)) > .03 ||
		math.Abs(math.Remainder(a.Pitch-b.Pitch, 2*math.Pi)) > .03
}

func objectSamples(o sitenav.Observations) []sitenav.Vertex {
	points := append([]sitenav.Vertex(nil), o.Surfaces.Rejected...)
	for _, p := range o.Surfaces.Surfaces {
		points = append(points, p.Position)
	}
	return points
}

func (r *SiteRobot) BeginObjectView(received int64, width, height int) (*ObjectView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	at := time.Unix(0, received).Add(-r.observed.ClockOffset)
	if width <= 0 || height <= 0 || math.Abs(float64(width)/float64(height)-16.0/9) > .015 {
		return nil, errors.New("object ranging requires an uncropped, unrotated 16:9 Go2 front-camera frame")
	}
	if now.Sub(at) < -50*time.Millisecond || now.Sub(at) > 3*time.Second {
		return nil, errors.New("camera receipt is stale or clocks disagree")
	}
	if err := r.observed.Ready(now); err != nil {
		return nil, err
	}
	if !validObjectPose(r.observed.Pose) {
		return nil, errors.New("object ranging requires a valid measured standing camera pose")
	}
	if r.frame == "" || r.moving || r.intentActive || r.closed || r.stopUnconfirmed {
		return nil, errors.New("object ranging needs a stationary, localized Go2")
	}
	pose := r.observed.Pose
	for _, p := range r.poseHistory {
		if math.Abs(p.Received.Sub(at).Seconds()) < math.Abs(pose.Received.Sub(at).Seconds()) {
			pose = p
		}
	}
	if math.Abs(pose.Received.Sub(at).Seconds()) > .15 || math.Abs(r.observed.CloudAt.Sub(at).Seconds()) > .75 {
		return nil, errors.New("no receipt-time-matched camera, odometry and LiDAR evidence")
	}
	if !validObjectPose(pose) || objectPostureChanged(pose, r.observed.Pose) {
		return nil, errors.New("Go2 camera posture changed since the frame")
	}
	if pose.Position.Distance(r.observed.Pose.Position) > .03 || math.Abs(math.Remainder(pose.Yaw-r.observed.Pose.Yaw, 2*math.Pi)) > .03 {
		return nil, errors.New("Go2 moved since the camera frame")
	}
	return &ObjectView{at: at, pose: pose, alignment: r.alignment, frame: r.frame, sequence: r.commandSequence, generation: r.intentGeneration, epoch: r.poseEpoch, samples: objectSamples(r.observed)}, nil
}

func validObjectBox(b [4]float64) bool {
	for _, v := range b {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return b[0] >= 0 && b[1] >= 0 && b[2] >= .02 && b[3] >= .02 && b[0]+b[2] <= 1 && b[1]+b[3] <= 1
}

// projectedObject uses measured 3D samples, including their vertical image
// position. A foreground/background disagreement produces no navigable target.
// No ground-plane intersection or model-estimated distance is used.
func projectedObject(points []sitenav.Vertex, pose sitenav.Pose, b [4]float64) (sitenav.Point, int, error) {
	if !validObjectBox(b) {
		return sitenav.Point{}, 0, errors.New("invalid object bounding box")
	}
	if !validObjectPose(pose) {
		return sitenav.Point{}, 0, errors.New("invalid measured camera pose")
	}
	type hit struct {
		point sitenav.Point
		depth float64
	}
	var hits []hit
	var background []float64
	c, s := math.Cos(pose.Yaw), math.Sin(pose.Yaw)
	cp, sp := math.Cos(pose.Pitch), math.Sin(pose.Pitch)
	cr, sr := math.Cos(pose.Roll), math.Sin(pose.Roll)
	seen := map[[3]int]bool{}
	for _, p := range points {
		if math.IsNaN(p.X) || math.IsInf(p.X, 0) || math.IsNaN(p.Y) || math.IsInf(p.Y, 0) || math.IsNaN(p.Z) || math.IsInf(p.Z, 0) {
			continue
		}
		dx, dy := p.X-pose.Position.X, p.Y-pose.Position.Y
		// Odometry is Rz(yaw)*Ry(pitch)*Rx(roll), with body axes forward,
		// left, up. Invert the complete measured attitude before subtracting
		// the body-mounted camera offset. This rotates the mount as well as
		// the optical rays, matching macOS's full camera/mount transform.
		x, y, z := dx*c+dy*s, -dx*s+dy*c, p.Z-pose.Height
		bodyX, bodyZ := cp*x-sp*z, sp*x+cp*z
		forward := bodyX - objectCameraForward
		left := cr*y + sr*bodyZ - objectCameraLeft
		up := -sr*y + cr*bodyZ - objectCameraAboveBase
		if forward < .3 || forward > 5 || p.Z < pose.Height-.15 {
			continue
		}
		x, y = -left/forward, -up/forward
		r2 := x*x + y*y
		radial := 1 - .354630*r2 + .102054*r2*r2
		xd := x*radial + 2*(-.001614)*x*y + (-.001249)*(r2+2*x*x)
		yd := y*radial + (-.001614)*(r2+2*y*y) + 2*(-.001249)*x*y
		u, v := objectCX+objectFX*xd, objectCY+objectFY*yd
		if math.IsNaN(u) || math.IsInf(u, 0) || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if v >= b[1] && v <= b[1]+b[3] && ((u >= b[0]-.2*b[2] && u < b[0]) || (u > b[0]+b[2] && u <= b[0]+1.2*b[2])) {
			background = append(background, forward)
		}
		// Central region reduces foreground edges and lens-distortion sensitivity.
		if u < b[0]+.15*b[2] || u > b[0]+.85*b[2] || v < b[1]+.1*b[3] || v > b[1]+.9*b[3] || u < .1 || u > .9 {
			continue
		}
		key := [3]int{int(math.Floor(p.X / .1)), int(math.Floor(p.Y / .1)), int(math.Floor(p.Z / .1))}
		if seen[key] {
			continue
		}
		seen[key] = true
		hits = append(hits, hit{sitenav.Point{X: p.X, Y: p.Y}, forward})
	}
	if len(hits) < 3 {
		return sitenav.Point{}, 0, errors.New("object is visible but has insufficient matching 3D LiDAR returns")
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].depth < hits[j].depth })
	if hits[len(hits)-1].depth-hits[0].depth > .45 {
		return sitenav.Point{}, len(hits), errors.New("object depth is ambiguous between foreground and background returns")
	}
	for _, depth := range background {
		if math.Abs(depth-hits[len(hits)/2].depth) < .25 {
			return sitenav.Point{}, len(hits), errors.New("matching depth extends outside the object box and may be a background wall")
		}
	}
	var point sitenav.Point
	for _, h := range hits {
		point.X += h.point.X
		point.Y += h.point.Y
	}
	point.X /= float64(len(hits))
	point.Y /= float64(len(hits))
	return point, len(hits), nil
}

func (r *SiteRobot) LocateObjectView(view *ObjectView, box [4]float64) (ObjectTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.locateObjectViewLocked(view, box)
}

func (r *SiteRobot) locateObjectViewLocked(view *ObjectView, box [4]float64) (ObjectTarget, error) {
	var result ObjectTarget
	now := time.Now()
	if !validObjectPose(r.observed.Pose) {
		return result, errors.New("camera pose is invalid; find the object again")
	}
	if view == nil || now.Sub(view.at) > 2*time.Minute || now.Before(view.at) || r.closed || r.moving || r.intentActive || r.stopUnconfirmed || r.frame != view.frame || r.alignment != view.alignment || r.commandSequence != view.sequence || r.intentGeneration != view.generation || r.poseEpoch != view.epoch {
		return result, errors.New("object evidence expired or the robot/controller alignment changed; find the object again")
	}
	if err := r.observed.Ready(now); err != nil {
		return result, err
	}
	if !validObjectPose(view.pose) || objectPostureChanged(view.pose, r.observed.Pose) {
		return result, errors.New("Go2 camera posture changed; find the object again")
	}
	if view.pose.Position.Distance(r.observed.Pose.Position) > .03 || math.Abs(math.Remainder(view.pose.Yaw-r.observed.Pose.Yaw, 2*math.Pi)) > .03 {
		return result, errors.New("Go2 moved since the camera frame; find the object again")
	}
	point, count, err := projectedObject(view.samples, view.pose, box)
	if err != nil {
		return result, err
	}
	current, _, err := projectedObject(objectSamples(r.observed), r.observed.Pose, box)
	if err != nil {
		return result, err
	}
	if point.Distance(current) > .2 {
		return result, errors.New("the object's measured geometry changed; find it again")
	}
	distance := point.Distance(r.observed.Pose.Position)
	if distance < .9 {
		return result, errors.New("object is already within the approach stand-off")
	}
	// Plan on raw measured floor, never the supervised unknown-floor policy.
	direction := sitenav.Heading(r.observed.Pose.Position, point)
	var approach sitenav.Point
	found := false
	for _, standOff := range []float64{.9, 1.1, 1.3} {
		if distance <= standOff {
			continue
		}
		p := sitenav.Point{X: point.X - standOff*math.Cos(direction), Y: point.Y - standOff*math.Sin(direction)}
		if _, err := r.observed.Grid.PlanDetour(r.observed.Pose.Position, r.observed.Pose.Yaw, p); err == nil {
			approach = p
			found = true
			break
		}
	}
	if !found {
		return result, errors.New("object is visible and ranged, but no approach route is supported by measured clear floor")
	}
	target, goal := r.alignment.ToSite(point), r.alignment.ToSite(approach)
	return ObjectTarget{Frame: view.frame, X: target.X, Z: -target.Y, ApproachX: goal.X, ApproachZ: -goal.Y, RangeMeters: distance, Support: count, Calibration: "go2-front-720p-community-estimated"}, nil
}
