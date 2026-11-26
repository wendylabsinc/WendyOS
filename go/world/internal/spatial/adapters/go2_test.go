package adapters

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

const tol = 1e-9

var go2Captured = time.Unix(1_700_000_000, 0)

func go2Pose(x, y, yaw float64) sitenav.Pose {
	return sitenav.Pose{Position: sitenav.Point{X: x, Y: y}, Height: 0.32, Yaw: yaw, Received: go2Captured.Add(20 * time.Millisecond), Captured: go2Captured}
}

func near2(a, b geom.Vec2) bool { return a.Dist(b) < tol }

func nearPose(a, b geom.Pose2) bool {
	return near2(a.Pos(), b.Pos()) && math.Abs(geom.AngleDiff(a.Yaw, b.Yaw)) < tol
}

// rosLaser is a ROS LaserScan of 360 one-degree beams from −π, all missing.
func rosLaser(robot, mount go2nav.Pose) go2nav.Scan {
	s := go2nav.Scan{Frame: "odom", CapturedUnixNS: go2Captured.UnixNano(), AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / 360, RangeMin: 0.1, RangeMax: 12, Robot: robot, SensorMount: mount, MountKnown: true, Ranges: make([]float64, 360)}
	for i := range s.Ranges {
		s.Ranges[i] = math.Inf(1)
	}
	return s
}

// The top risk of the real-data path. ROS: x forward, y left, yaw
// counter-clockwise. Spatial: X forward, Z right, yaw clockwise.
func TestROSHandedness(t *testing.T) {
	if got := PointFromROS(1, 2, 3); got != (geom.Vec3{X: 1, Y: 3, Z: -2}) {
		t.Fatalf("PointFromROS(1, 2, 3) = %+v", got)
	}

	// A counter-clockwise quarter turn becomes yaw −π/2.
	odometry, err := Go2Odometry(Config{}, "go2", "odom", go2Pose(2, 3, math.Pi/2))
	if err != nil {
		t.Fatal(err)
	}
	if want := (geom.Pose2{X: 2, Z: -3, Yaw: -math.Pi / 2}); !nearPose(odometry.Pose, want) || odometry.Pose.Yaw > 0 {
		t.Fatalf("quarter turn counter-clockwise: pose %+v, want %+v", odometry.Pose, want)
	}
	if !odometry.ElevationKnown || odometry.Elevation != 0.32 || !odometry.Time.Equal(go2Captured) {
		t.Fatalf("odometry elevation or time: %+v", odometry)
	}

	for _, yaw := range []float64{0, math.Pi / 2, -2.1, 3} {
		pose := go2Pose(2, 3, yaw)
		sin, cos := math.Sincos(yaw)
		// One metre to the robot's left and two metres ahead of it, in odom.
		left := sitenav.Vertex{X: 2 - sin, Y: 3 + cos, Z: pose.Height}
		ahead := sitenav.Vertex{X: 2 + 2*cos, Y: 3 + 2*sin, Z: pose.Height}

		// Through the voxel cloud.
		scan, err := Go2CloudScan(Config{}, "go2", "lidar", pose, []sitenav.Vertex{left, ahead}, go2Captured)
		if err != nil {
			t.Fatal(err)
		}
		// Beams run from −π: the left point (yaw −π/2) comes before the one ahead.
		pts := scan.Points()
		if len(pts) != 2 || !near2(pts[0], geom.Vec2{X: 0, Z: -1}) || !near2(pts[1], geom.Vec2{X: 2, Z: 0}) {
			t.Fatalf("yaw %v: cloud points in the body frame %+v, want left at Z=-1 and ahead at X=2", yaw, pts)
		}
		// Placing the body-frame scan with the converted odometry recovers the
		// converted odom points: the three ROS conversions agree.
		odometry, err := Go2Odometry(Config{}, "go2", "odom", pose)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := odometry.Pose.Apply(pts[0]), PointFromROS(left.X, left.Y, 0).XZ(); !near2(got, want) {
			t.Fatalf("yaw %v: left point placed at %+v, want %+v", yaw, got, want)
		}

		// Through a ROS LaserScan: beam 270 looks along +π/2 (left), beam 180
		// straight ahead.
		ros := rosLaser(go2nav.Pose{X: 2, Y: 3, Yaw: yaw}, go2nav.Pose{})
		ros.Ranges[270], ros.Ranges[180] = 1, 2
		laser, paired, mount, err := Go2LaserScan(Config{}, "go2", "lidar", "odom", ros)
		if err != nil {
			t.Fatal(err)
		}
		if paired.Pose != odometry.Pose || mount != (geom.Pose2{}) {
			t.Fatalf("yaw %v: laser odometry %+v mount %+v", yaw, paired.Pose, mount)
		}
		pts = laser.Points()
		if len(pts) != 2 || !near2(pts[0], geom.Vec2{X: 2, Z: 0}) || !near2(pts[1], geom.Vec2{X: 0, Z: -1}) {
			t.Fatalf("yaw %v: laser points in the lidar frame %+v, want ahead at X=2 and left at Z=-1", yaw, pts)
		}
	}
}

func TestGo2LaserScanMountAndMissingReturns(t *testing.T) {
	// Robot facing ROS +y, lidar 0.28 m ahead of the base and 0.1 m to its
	// left, turned a further quarter turn counter-clockwise.
	ros := rosLaser(go2nav.Pose{X: 2, Y: 3, Yaw: math.Pi / 2}, go2nav.Pose{X: 0.28, Y: 0.1, Yaw: math.Pi / 2})
	ros.Ranges[180] = 1.5 // straight ahead of the lidar: odom −x
	ros.Ranges[0] = 13    // beyond RangeMax
	scan, odometry, mount, err := Go2LaserScan(Config{}, "go2", "lidar", "odom", ros)
	if err != nil {
		t.Fatal(err)
	}
	if want := (geom.Pose2{X: 0.28, Z: -0.1, Yaw: -math.Pi / 2}); !nearPose(mount, want) {
		t.Fatalf("mount %+v, want %+v", mount, want)
	}
	// odom: base (2, 3); lidar (2−0.1, 3+0.28); hit 1.5 m along −x.
	if got, want := odometry.Pose.Compose(mount).Apply(scan.Points()[0]), PointFromROS(1.9-1.5, 3.28, 0).XZ(); !near2(got, want) {
		t.Fatalf("hit placed at %+v, want %+v", got, want)
	}
	if odometry.ElevationKnown {
		t.Fatal("a scan without a tilt record declares no elevation")
	}
	for _, i := range []int{0, 5} { // beyond range, and missing
		if _, _, state := scan.Beam(i); state != spatial.BeamInvalid {
			t.Fatalf("beam %d: state %v, want no information", i, state)
		}
	}
	free, _, _, err := Go2LaserScan(Config{NoReturnIsFree: true}, "go2", "lidar", "odom", ros)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, state := free.Beam(5); state != spatial.BeamNoReturn {
		t.Fatalf("NoReturnIsFree: state %v", state)
	}

	level := [4]float64{math.Cos(0.01), 0, math.Sin(0.01), 0} // pitch 0.02 rad
	ros.TiltedPose = &go2nav.TiltedPose{QuaternionWXYZ: level, OdomPositionXYZ: [3]float64{2, 3, 0.31}, OdomCapturedUnixNS: ros.CapturedUnixNS - 4_000_000}
	_, odometry, _, err = Go2LaserScan(Config{}, "go2", "lidar", "odom", ros)
	if err != nil {
		t.Fatal(err)
	}
	if !odometry.ElevationKnown || odometry.Elevation != 0.31 || odometry.Time.UnixNano() != ros.TiltedPose.OdomCapturedUnixNS {
		t.Fatalf("paired odometry %+v", odometry)
	}
	ros.TiltedPose.QuaternionWXYZ = [4]float64{math.Cos(0.05), 0, math.Sin(0.05), 0} // pitch 0.1 rad
	if _, _, _, err = Go2LaserScan(Config{}, "go2", "lidar", "odom", ros); !errors.Is(err, ErrTilted) {
		t.Fatalf("tilted scan: %v", err)
	}
	if _, _, _, err = Go2LaserScan(Config{MaxTilt: 0.2}, "go2", "lidar", "odom", ros); err != nil {
		t.Fatalf("tilt within a raised limit: %v", err)
	}
	ros.Frame = "base_link"
	if _, _, _, err = Go2LaserScan(Config{}, "go2", "lidar", "odom", ros); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong frame: %v", err)
	}
	ros.Frame, ros.CapturedUnixNS = "odom", 0
	if _, _, _, err = Go2LaserScan(Config{}, "go2", "lidar", "odom", ros); !errors.Is(err, ErrNoTime) {
		t.Fatalf("no stamp: %v", err)
	}
}

func TestGo2CloudScanFlattening(t *testing.T) {
	pose := go2Pose(0, 0, 0) // base 0.32 m above the odom floor
	cloud := []sitenav.Vertex{
		{X: 3, Y: 0, Z: 0.50},     // ahead, in the band, hidden by the next
		{X: 2, Y: 0, Z: 0.32},     // ahead, nearest
		{X: 1.5, Y: 0, Z: 0},      // floor: below the band
		{X: 1, Y: 0, Z: 1.0},      // overhang: above the band
		{X: 0, Y: -0.05, Z: 0.32}, // right, closer than RangeMin
		{X: 0, Y: -4, Z: 0.32},    // right, behind the too-near point
		{X: 0, Y: 11, Z: 0.32},    // left, beyond RangeMax
		{X: 0, Y: 0, Z: 0.32},     // on the origin: no bearing
	}
	scan, err := Go2CloudScan(Config{}, "go2", "lidar", pose, cloud, go2Captured.Add(150*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Ranges) != 360 || scan.RangeMin != 0.10 || scan.RangeMax != 10 || scan.RangeSigma != 0.03 || !scan.Time.Equal(go2Captured) {
		t.Fatalf("defaults or stamp: %+v", scan)
	}
	for i := range scan.Ranges {
		_, r, state := scan.Beam(i)
		switch {
		case i == 180 && (state != spatial.BeamHit || r != 2):
			t.Fatalf("ahead: range %v state %v, want the nearest in-band point", r, state)
		case i != 180 && state != spatial.BeamInvalid:
			t.Fatalf("beam %d: state %v range %v, want no information", i, state, r)
		}
	}
	free, err := Go2CloudScan(Config{NoReturnIsFree: true}, "go2", "lidar", pose, cloud, go2Captured)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, state := free.Beam(90); state != spatial.BeamNoReturn { // left: only a point beyond range
		t.Fatalf("empty bin under NoReturnIsFree: %v", state)
	}
	if _, _, state := free.Beam(270); state != spatial.BeamInvalid { // right: blocked by a too-near point
		t.Fatalf("blocked bin under NoReturnIsFree: %v", state)
	}

	if _, err = Go2CloudScan(Config{}, "go2", "lidar", pose, cloud, go2Captured.Add(300*time.Millisecond)); !errors.Is(err, ErrSkew) {
		t.Fatalf("stale cloud: %v", err)
	}
	if _, err = Go2CloudScan(Config{}, "go2", "lidar", pose, append(cloud, sitenav.Vertex{X: 1, Y: 1, Z: math.NaN()}), go2Captured); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NaN voxel: %v", err)
	}
	if _, err = Go2CloudScan(Config{Beams: 3}, "go2", "lidar", pose, cloud, go2Captured); !errors.Is(err, ErrConfig) {
		t.Fatalf("bad config: %v", err)
	}
	if _, err = Go2CloudScan(Config{}, "go 2", "lidar", pose, cloud, go2Captured); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad device id: %v", err)
	}

	unstamped := pose
	unstamped.Captured = time.Time{}
	if _, err = Go2Odometry(Config{}, "go2", "odom", unstamped); !errors.Is(err, ErrNoTime) {
		t.Fatalf("pose without a capture stamp: %v", err)
	}
	odometry, err := Go2Odometry(Config{AllowArrivalTime: true}, "go2", "odom", unstamped)
	if err != nil || !odometry.Time.Equal(pose.Received) {
		t.Fatalf("arrival time: %v %v", odometry.Time, err)
	}
}

// A voxel packet recorded from a Go2, decoded by sitenav itself.
func TestGo2RecordedVoxelPacket(t *testing.T) {
	raw, err := os.ReadFile("../../../../internal/twinrobot/sitenav/testdata/occupied-wall-cloud.bin")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("sitenav's recorded packet is not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 125000000)
	o := sitenav.Observations{Pose: sitenav.Pose{Position: sitenav.Point{X: -.034937, Y: .01939}, Height: .31861, Yaw: -.59125239859, Received: now, Captured: now}}
	if _, err := Go2LandmarkScan(Config{}, "go2", "lidar", &o); !errors.Is(err, ErrNoTime) {
		t.Fatalf("before any cloud: %v", err)
	}
	if err := o.Receive(sitenav.CloudTopic, raw, true, now, now); err != nil {
		t.Fatal(err)
	}
	scan, err := Go2LandmarkScan(Config{}, "go2", "lidar", &o)
	if err != nil {
		t.Fatal(err)
	}
	odometry, err := Go2Odometry(Config{}, "go2", "odom", o.Pose)
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	for i := range scan.Ranges {
		angle, r, state := scan.Beam(i)
		if state != spatial.BeamHit {
			continue
		}
		hits++
		// Every hit is a recorded landmark at its true range, moved sideways
		// by at most half a bin.
		found := false
		for _, l := range o.Landmarks {
			body := odometry.Pose.InverseApply(PointFromROS(l.X, l.Y, 0).XZ())
			if math.Abs(body.Norm()-r) < tol && math.Abs(geom.AngleDiff(body.Angle(), angle)) <= scan.AngleIncrement/2+tol {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("beam %d at %.3f m matches no recorded landmark", i, r)
		}
	}
	if hits == 0 || hits > len(o.Landmarks) {
		t.Fatalf("%d hits from %d landmarks", hits, len(o.Landmarks))
	}
}

// A laser scan recorded from a Go2: every return must land where go2nav's own
// ROS-convention arithmetic puts it.
func TestGo2RecordedLaserScan(t *testing.T) {
	raw, err := os.ReadFile("../../go2nav/testdata/corridor-d.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("go2nav's recorded scan is not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Scan go2nav.Scan `json:"scan"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	s := fixture.Scan
	scan, odometry, mount, err := Go2LaserScan(Config{}, "go2", "lidar", "odom", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Ranges) != len(s.Ranges) || len(scan.Ranges) < 180 {
		t.Fatalf("%d beams from %d", len(scan.Ranges), len(s.Ranges))
	}
	sensor := odometry.Pose.Compose(mount)
	sin, cos := math.Sincos(s.Robot.Yaw)
	ox, oy := s.Robot.X+cos*s.SensorMount.X-sin*s.SensorMount.Y, s.Robot.Y+sin*s.SensorMount.X+cos*s.SensorMount.Y
	hits := 0
	for i, r := range s.Ranges {
		angle, got, state := scan.Beam(i)
		if math.IsInf(r, 1) {
			if state != spatial.BeamInvalid {
				t.Fatalf("beam %d: a missing return became state %v", i, state)
			}
			continue
		}
		hits++
		a := s.Robot.Yaw + s.SensorMount.Yaw + s.AngleMin + float64(i)*s.AngleIncrement
		want := PointFromROS(ox+r*math.Cos(a), oy+r*math.Sin(a), 0).XZ()
		if placed := sensor.Apply(geom.Heading(angle).Scale(got)); state != spatial.BeamHit || placed.Dist(want) > tol {
			t.Fatalf("beam %d: placed at %+v, want %+v (state %v)", i, placed, want, state)
		}
	}
	if hits < 100 {
		t.Fatalf("only %d returns in the recorded scan", hits)
	}
}
