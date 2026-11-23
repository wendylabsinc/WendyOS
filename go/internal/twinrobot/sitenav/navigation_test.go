package sitenav

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/pierrec/lz4/v4"
	"math"
	"strings"
	"testing"
	"time"
)

func floorGrid() Grid {
	g := Grid{Floor: map[Cell]bool{}, Obstacles: map[Cell]bool{}}
	for y := -30; y < 50; y++ {
		for x := -30; x < 80; x++ {
			g.Floor[Cell{x, y}] = true
		}
	}
	return g
}

func roundedCloudFrame(t *testing.T, now time.Time) []byte {
	t.Helper()
	raw := cloudFrame(t, now, "odom", []float64{0, 0, 0})
	n := int(binary.LittleEndian.Uint32(raw[4:]))
	var env map[string]any
	if err := json.Unmarshal(raw[12:12+n], &env); err != nil {
		t.Fatal(err)
	}
	env["data"].(map[string]any)["stamp"] = json.RawMessage(fmt.Sprintf("%.6e", float64(now.UnixNano())/1e9))
	header, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	compact := make([]byte, 4)
	binary.LittleEndian.PutUint16(compact, uint16(len(header)))
	return append(append(compact, header...), raw[12+n:]...)
}

func TestCloudSupervisedArrivalTiming(t *testing.T) {
	now := time.Unix(1792945475, 123456789)
	newObserved := func() Observations {
		return Observations{Pose: Pose{Position: Point{2, 2}, Height: .3, Received: now, Captured: now}}
	}
	raw := roundedCloudFrame(t, now)
	o := newObserved()
	if o.Receive(CloudTopic, raw, true, now, now) == nil {
		t.Fatal("strict default accepted coarse timing")
	}
	o.SetArrivalTiming(true)
	if o.Ready(now) == nil {
		t.Fatal("mode change reused a cached scan")
	}
	if err := o.Receive(CloudTopic, raw, true, now, now); err != nil {
		t.Fatal(err)
	}
	if o.Ready(now) != nil || !o.UsingArrivalTime || !o.TimingUnavailable || !o.Grid.Clear(Point{2, 2}) {
		t.Fatal("explicit arrival mode rejected valid observed floor")
	}
	if o.Ready(now.Add(MaxAge+time.Nanosecond)) == nil {
		t.Fatal("stopped stream stayed ready")
	}
	if o.Receive(CloudTopic, raw, true, now, now) == nil || o.Ready(now) == nil {
		t.Fatal("duplicate gateway delivery renewed readiness")
	}
	// New packets may legitimately share the same coarse source stamp.
	later := now.Add(100 * time.Millisecond)
	o.Pose.Received, o.Pose.Captured = later, later
	if err := o.Receive(CloudTopic, raw, true, later, later); err != nil {
		t.Fatal(err)
	}
	o.SetArrivalTiming(false)
	if o.Ready(later) == nil || o.UsingArrivalTime {
		t.Fatal("strict mode retained an arrival-timed map")
	}
	if o.Receive(CloudTopic, raw, true, later, later) == nil {
		t.Fatal("strict mode lost capture check")
	}
	for _, fault := range []string{"old-arrival", "old-pose", "missing-pose-capture", "old-epoch", "pose-arrival-skew", "precise-old-capture"} {
		t.Run(fault, func(t *testing.T) {
			o := newObserved()
			o.SetArrivalTiming(true)
			received, packet := now, raw
			switch fault {
			case "old-arrival":
				received = now.Add(-time.Second)
			case "old-pose":
				o.Pose.Captured = now.Add(-time.Second)
			case "missing-pose-capture":
				o.Pose.Captured = time.Time{}
			case "old-epoch":
				packet = roundedCloudFrame(t, now.Add(-time.Hour))
			case "precise-old-capture":
				packet = cloudFrame(t, now.Add(-time.Second), "odom", []float64{0, 0, 0})
			case "pose-arrival-skew":
				o.Pose.Received = now.Add(-300 * time.Millisecond)
			}
			if o.Receive(CloudTopic, packet, true, received, now) == nil || o.Ready(now) == nil {
				t.Fatal("arrival mode bypassed remaining freshness checks")
			}
		})
	}
}

func TestClockOffsetPreservesFreshnessAndReplayChecks(t *testing.T) {
	now := time.Unix(1792969000, 123456789)
	for _, offset := range []time.Duration{75 * time.Millisecond, -120 * time.Millisecond} {
		remote := now.Add(offset)
		o := Observations{ClockOffset: offset}
		o.SetArrivalTiming(true)
		rawPose := poseEnvelope(remote, []float64{2, 2, .3}, true)
		// The stream reader translates gateway receipt time. Receive translates
		// embedded sensor capture times using the same fixed clock offset.
		if err := o.Receive(PoseTopic, rawPose, false, now, now); err != nil {
			t.Fatal(err)
		}
		if !o.Pose.Captured.Equal(now) {
			t.Fatal("capture was not translated")
		}
		cloud := roundedCloudFrame(t, remote)
		if err := o.Receive(CloudTopic, cloud, true, now, now); err != nil {
			t.Fatal(err)
		}
		if err := o.Ready(now); err != nil {
			t.Fatal(err)
		}
		if o.Ready(now.Add(MaxAge+time.Nanosecond)) == nil {
			t.Fatal("clock offset extended freshness")
		}
		if o.Receive(CloudTopic, cloud, true, now, now) == nil {
			t.Fatal("offset admitted repeated delivery")
		}
		old := poseEnvelope(remote.Add(-time.Second), []float64{2, 2, .3}, true)
		if o.Receive(PoseTopic, old, false, now, now) == nil {
			t.Fatal("offset admitted stale capture")
		}
		// Later clock steps do not silently retune the fixed calibration.
		future := poseEnvelope(remote.Add(time.Second), []float64{2, 2, .3}, true)
		if o.Receive(PoseTopic, future, false, now, now) == nil {
			t.Fatal("clock step refreshed pose")
		}
	}
}
func TestPlanAroundObstacleAndRejectUnknown(t *testing.T) {
	g := floorGrid()
	for y := -5; y <= 15; y++ {
		g.Obstacles[Cell{20, y}] = true
	}
	path, err := g.Plan(Point{}, Point{4, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(path) < 3 {
		t.Fatal("route did not detour around obstacle")
	}
	for i := 1; i < len(path); i++ {
		if !g.Segment(path[i-1], path[i]) {
			t.Fatal("route crosses a blocked footprint")
		}
	}
	for y := -30; y < 50; y++ {
		for x := 35; x < 45; x++ {
			delete(g.Floor, Cell{x, y})
		}
	}
	if _, err = g.Plan(Point{}, Point{5, 0}); err == nil {
		t.Fatal("planned across unknown floor")
	}
	for _, p := range []Point{{math.NaN(), 0}, {math.Inf(1), 0}, {9, 0}} {
		if _, err = g.Plan(Point{}, p); err == nil {
			t.Fatal("accepted invalid/too far goal", p)
		}
	}
}
func TestAlignmentRoundTripAndHeading(t *testing.T) {
	a := Alignment{Point{2, 3}, Point{10, -6}, math.Pi / 2}
	p := a.ToSite(Point{3, 3})
	if p.Distance(Point{10, -5}) > 1e-9 {
		t.Fatal(p)
	}
	if a.ToRobot(p).Distance(Point{3, 3}) > 1e-9 {
		t.Fatal("wrong inverse")
	}
	forward, turn := Velocity(Point{}, 0, Point{0, 2})
	if forward != 0 || turn <= 0 {
		t.Fatal("must turn before walking")
	}
	forward, turn = Velocity(Point{}, 0, Point{2, 0})
	if forward != WalkingSpeed || turn != 0 {
		t.Fatal("invalid forward velocity", forward, turn)
	}
}
func poseEnvelope(now time.Time, position []float64, stamp bool) []byte {
	d := map[string]any{"position": position, "imu_state": map[string]any{"quaternion": []float64{1, 0, 0, 0}}}
	if stamp {
		d["stamp"] = map[string]any{"sec": now.Unix(), "nanosec": now.Nanosecond()}
	}
	raw, _ := json.Marshal(map[string]any{"topic": PoseTopic, "data": d})
	return raw
}
func TestPoseFreshnessReplayAndUnstampedVisualization(t *testing.T) {
	now := time.Now()
	o := Observations{}
	raw := poseEnvelope(now, []float64{0, 0, .3}, true)
	if err := o.Receive(PoseTopic, raw, false, now, now); err != nil {
		t.Fatal(err)
	}
	if err := o.Receive(PoseTopic, raw, false, now, now); err == nil {
		t.Fatal("replayed capture accepted")
	}
	if err := o.Receive(PoseTopic, poseEnvelope(now.Add(-time.Second), []float64{0, 0, .3}, true), false, now, now); err == nil {
		t.Fatal("stale capture accepted")
	}
	if err := o.Receive(PoseTopic, poseEnvelope(now.Add(time.Millisecond), []float64{5, 0, .3}, true), false, now.Add(time.Millisecond), now.Add(time.Millisecond)); err == nil {
		t.Fatal("pose jump accepted")
	}
	o = Observations{}
	if err := o.Receive(PoseTopic, poseEnvelope(now, []float64{0, 0, .3}, false), false, now, now); err != nil {
		t.Fatal(err)
	}
	if !Fresh(o.Pose.Received, now) || o.Ready(now) == nil {
		t.Fatal("unstamped pose must be visual-only")
	}
}
func cloudFrame(t *testing.T, now time.Time, frame string, origin []float64) []byte {
	t.Helper()
	raw := make([]byte, 2048)
	for i := range raw {
		raw[i] = 255
	}
	compressed := make([]byte, lz4.CompressBlockBound(len(raw)))
	n, err := lz4.CompressBlock(raw, compressed, nil)
	if err != nil || n == 0 {
		t.Fatal(err)
	}
	meta := map[string]any{"src_size": len(raw), "origin": origin, "resolution": .05, "frame_id": frame, "stamp": float64(now.UnixNano()) / 1e9}
	header, _ := json.Marshal(map[string]any{"topic": CloudTopic, "data": meta})
	out := make([]byte, 12)
	binary.LittleEndian.PutUint16(out, 2)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(header)))
	return append(append(out, header...), compressed[:n]...)
}
func TestCloudCaptureFrameFloorAndReplay(t *testing.T) {
	now := time.Now()
	o := Observations{Pose: Pose{Position: Point{2, 2}, Height: .3, Received: now, Captured: now}}
	raw := cloudFrame(t, now, "odom", []float64{0, 0, 0})
	if err := o.Receive(CloudTopic, raw, true, now, now); err != nil {
		t.Fatal(err)
	}
	if !o.Grid.Clear(Point{2, 2}) || o.Ready(now) != nil {
		t.Fatal("valid observed floor rejected")
	}
	if err := o.Receive(CloudTopic, raw, true, now, now); err == nil || o.Ready(now) == nil {
		t.Fatal("replayed map renewed readiness")
	}
	if err := o.Receive(CloudTopic, cloudFrame(t, now.Add(time.Millisecond), "base_link", []float64{0, 0, 0}), true, now, now); err == nil {
		t.Fatal("wrong frame accepted")
	}
	if err := o.Receive(CloudTopic, raw[:5], true, now, now); err == nil {
		t.Fatal("truncated frame accepted")
	}
	_ = o.Receive(CloudTopic, cloudFrame(t, now.Add(time.Millisecond), "odom", []float64{0, 0, .4}), true, now, now)
	if o.Ready(now) == nil {
		t.Fatal("obstacle layer treated as floor")
	}
}

func TestCloudCompactHeaderAndTimestampPrecision(t *testing.T) {
	now := time.Now()
	extended := cloudFrame(t, now, "odom", []float64{0, 0, 0})
	n := binary.LittleEndian.Uint32(extended[4:])
	compact := make([]byte, 4)
	binary.LittleEndian.PutUint16(compact, uint16(n))
	binary.LittleEndian.PutUint16(compact[2:], uint16(len(extended)-12-int(n)))
	compact = append(compact, extended[12:]...)
	o := Observations{Pose: Pose{Position: Point{2, 2}, Height: .3, Received: now, Captured: now}}
	if err := o.Receive(CloudTopic, compact, true, now, now); err != nil {
		t.Fatal(err)
	}
	if o.Ready(now) != nil || !o.Grid.Clear(Point{2, 2}) {
		t.Fatal("compact frame did not produce observed floor")
	}
	for _, v := range []struct {
		raw  string
		want float64
	}{{"1.792944e+09", 1000}, {"1792944416.816952043", 1e-9}, {"1792944000", 1}} {
		if got := timestampResolution(v.raw); math.Abs(got-v.want) > 1e-12 {
			t.Fatalf("%s: %g", v.raw, got)
		}
	}
	// Same valid packet, but with the firmware's rounded source timestamp.
	var env map[string]any
	if err := json.Unmarshal(extended[12:12+int(n)], &env); err != nil {
		t.Fatal(err)
	}
	env["data"].(map[string]any)["stamp"] = json.RawMessage("1.792944e+09")
	header, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	coarse := make([]byte, 4)
	binary.LittleEndian.PutUint16(coarse, uint16(len(header)))
	coarse = append(append(coarse, header...), extended[12+int(n):]...)
	if err := o.Receive(CloudTopic, coarse, true, now, now); err == nil || !strings.Contains(err.Error(), "too coarse") {
		t.Fatalf("missing precision diagnostic: %v", err)
	}
	if o.Ready(now) == nil {
		t.Fatal("arrival time must not make a coarse capture safe")
	}
	for _, bad := range [][]byte{compact[:3], compact[:9], extended[:8]} {
		if o.Receive(CloudTopic, bad, true, now, now) == nil {
			t.Fatal("truncated header accepted")
		}
	}
}
