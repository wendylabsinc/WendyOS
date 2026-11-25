package twinrobot

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Go2 front camera, 1280x720, as fractions of the image size. From
// go2_ros2_sdk front_camera_720.yaml, the same preset the Apple clients use.
// Lens distortion is ignored: ranging tolerates a few degrees of bearing error.
const go2LensFocalX, go2LensCenterX = 864.39938 / 1280, 639.19798 / 1280
const go2LensFocalY, go2LensCenterY = 863.73849 / 720, 373.28118 / 720

// Camera mount from Unitree's Go2 URDF (front_camera joint) plus the standing
// base height. Calibrate these against the robot before trusting floor ranges.
const go2CameraForward = 0.327
const go2CameraHeight = 0.36

const viewRangeMinimum, viewRangeMaximum = 0.3, 6.0

// ErrOperatorLease rejects an order because no rendering client renewed the
// 600 ms operator lease. MovementLeaseLostMessage is the snapshot message when
// the same loss stops a movement already under way.
var ErrOperatorLease = errors.New("The map is no longer active.")

const MovementLeaseLostMessage = "Movement stopped because the map is no longer active."

// ErrNoViewRange means the camera saw the object but neither LiDAR nor the
// floor geometry can say how far away it is.
var ErrNoViewRange = errors.New("the object is visible but its distance is unknown")

// viewTarget places a camera detection in the odometry frame. u is the box
// centre and bottom its lower edge, both 0..1 of the image. LiDAR gives the
// first obstacle along the bearing; the box's lower edge gives where the floor
// is touched. The nearer wins: it is the object when LiDAR saw it, the floor
// contact when LiDAR saw only the wall behind it, and the table edge when the
// object is raised.
func viewTarget(grid sitenav.Grid, pose sitenav.Pose, u, bottom float64) (sitenav.Point, error) {
	if !(u >= 0 && u <= 1) || !(bottom >= 0 && bottom <= 1) {
		return sitenav.Point{}, errors.New("invalid camera detection")
	}
	offset := math.Atan((u - go2LensCenterX) / go2LensFocalX) // positive is right of centre
	bearing := pose.Yaw - offset
	camera := sitenav.Point{X: pose.Position.X + go2CameraForward*math.Cos(pose.Yaw), Y: pose.Position.Y + go2CameraForward*math.Sin(pose.Yaw)}
	distance, ranged := grid.RayHit(camera, bearing, viewRangeMinimum, viewRangeMaximum)
	if below := bottom - go2LensCenterY; below > 0.03 {
		floor := go2CameraHeight * go2LensFocalY / below / math.Cos(offset)
		if floor <= viewRangeMaximum && (!ranged || floor < distance) {
			distance, ranged = floor, true
		}
	}
	if !ranged {
		return sitenav.Point{}, ErrNoViewRange
	}
	return sitenav.Point{X: camera.X + distance*math.Cos(bearing), Y: camera.Y + distance*math.Sin(bearing)}, nil
}

// exploreTarget picks the measured clear floor that is farthest from every
// viewpoint already used, within one bounded walk. Site alignment never adds
// candidates: only floor that LiDAR currently reports can be chosen.
func exploreTarget(grid sitenav.Grid, pose sitenav.Pose, visited []sitenav.Point) (sitenav.Point, error) {
	type candidate struct {
		point sitenav.Point
		score float64
	}
	var candidates []candidate
	for _, p := range grid.ObservedClearFloor() {
		distance := p.Distance(pose.Position)
		if distance < 1.2 || distance > 4 {
			continue
		}
		novelty := 4.0
		for _, v := range visited {
			novelty = math.Min(novelty, p.Distance(v))
		}
		if novelty >= 1.2 {
			candidates = append(candidates, candidate{p, novelty - distance/10})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.point.X != b.point.X {
			return a.point.X < b.point.X
		}
		return a.point.Y < b.point.Y
	})
	for _, c := range candidates {
		if grid.ClearPose(c.point, sitenav.Heading(pose.Position, c.point)) {
			return c.point, nil
		}
	}
	return sitenav.Point{}, errors.New("LiDAR shows no unexplored clear floor within reach")
}

func (r *SiteRobot) findReadyLocked() error {
	if r.closed || r.ctx.Err() != nil || r.frame == "" {
		return errors.New("Align Go2 with this site first.")
	}
	return r.observed.Ready(time.Now())
}

// ExploreTarget returns the next search viewpoint in site coordinates. Visited
// points use the route convention (site X, -Z). It issues no command.
func (r *SiteRobot) ExploreTarget(visited []sitenav.Point) (x, z float64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = r.findReadyLocked(); err != nil {
		return 0, 0, err
	}
	local := make([]sitenav.Point, len(visited))
	for i, p := range visited {
		local[i] = r.alignment.ToRobot(p)
	}
	target, err := exploreTarget(r.observed.Grid, r.observed.Pose, local)
	if err != nil {
		return 0, 0, err
	}
	p := r.alignment.ToSite(target)
	return p.X, -p.Y, nil
}

// LocateInView converts a detection in a frame captured at the current,
// stationary pose into site coordinates. It issues no command.
func (r *SiteRobot) LocateInView(u, bottom float64) (x, z float64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err = r.findReadyLocked(); err != nil {
		return 0, 0, err
	}
	if r.moving {
		return 0, 0, errors.New("Go2 moved after the camera view was requested.")
	}
	target, err := viewTarget(r.observed.Grid, r.observed.Pose, u, bottom)
	if err != nil {
		return 0, 0, err
	}
	p := r.alignment.ToSite(target)
	return p.X, -p.Y, nil
}

// Bark plays the agent's bark clip through Go2's speaker. No motion is sent.
func (r *SiteRobot) Bark() error {
	ctx, cancel := context.WithTimeout(r.ctx, unitree.BarkTimeout+2*time.Second)
	defer cancel()
	_, err := r.client.Command(ctx, &pb.UnitreeCommandRequest{Serial: r.serial, Command: pb.UnitreeCommand(unitree.CommandBark)})
	return err
}
