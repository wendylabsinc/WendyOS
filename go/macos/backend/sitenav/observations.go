package sitenav

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pierrec/lz4/v4"
)

const PoseTopic = "rt/lf/sportmodestate"
const CloudTopic = "rt/utlidar/voxel_map_compressed"
const JointTopic = "rt/lf/lowstate"
const MaxAge = 750 * time.Millisecond

var ErrDelayedObservation = errors.New("Robot observations are delayed or the gateway clock is out of sync.")
var ErrStalePoseCapture = errors.New("Go2 pose capture is stale or its clock is out of sync.")
var ErrDuplicatePose = errors.New("Repeated Go2 odometry packet.")

// These packets do not advance freshness and do not establish a new odometry
// frame. Keep the last accepted pose so a rejected packet cannot erase the
// continuity check applied to the next one.
func TransientPoseError(err error) bool {
	return errors.Is(err, ErrDelayedObservation) || errors.Is(err, ErrStalePoseCapture) || errors.Is(err, ErrDuplicatePose)
}

type Pose struct {
	Position    Point
	Height, Yaw float64
	Received    time.Time
	Captured    time.Time
}
type Observations struct {
	// Fixed authenticated gateway-to-client offset measured when connecting.
	ClockOffset            time.Duration
	Pose                   Pose
	Grid                   Grid
	LocalNavigationSamples []Vertex
	CloudAt                time.Time
	CloudCapture           float64
	CloudReceived          time.Time
	AllowArrivalTime       bool
	UsingArrivalTime       bool
	TimingUnavailable      bool
	Landmarks              []Point
	Surfaces               SurfaceCloud
	FloorHeight            float64
	Joints                 []float64
	JointsAt               time.Time
	Fault                  string
	FloorFault             string
	PoseFault              string
}

// Timing changes invalidate the old map; a new packet must pass all checks.
func (o *Observations) SetArrivalTiming(enabled bool) {
	o.AllowArrivalTime = enabled
	o.UsingArrivalTime = false
	o.CloudAt = time.Time{}
	o.CloudReceived = time.Time{}
	o.CloudCapture = 0
	o.Grid = Grid{}
	o.Landmarks = nil
	o.Surfaces = SurfaceCloud{}
	o.FloorFault = ""
	o.Fault = "Waiting for a new LiDAR packet after changing timing mode."
}

func Fresh(stamp, now time.Time) bool {
	return !stamp.IsZero() && now.Sub(stamp) >= -50*time.Millisecond && now.Sub(stamp) <= MaxAge
}
func (o *Observations) Ready(now time.Time) error {
	if err := o.freshCloud(now); err != nil {
		return err
	}
	if o.FloorFault != "" {
		return errors.New(o.FloorFault)
	}
	return nil
}

// Position matching needs fresh structural returns, not navigable floor.
// Confirming a visual alignment cannot bypass Ready's navigation checks.
func (o *Observations) LocalizationReady(now time.Time) error {
	if err := o.freshCloud(now); err != nil {
		return err
	}
	return localizationStructure(o.Landmarks)
}

func (o *Observations) freshCloud(now time.Time) error {
	if !Fresh(o.Pose.Received, now) {
		if o.PoseFault != "" {
			return errors.New(o.PoseFault)
		}
		return errors.New("Waiting for fresh Go2 odometry.")
	}
	if !Fresh(o.Pose.Captured, now) {
		return errors.New("Go2 odometry has no fresh capture timestamp. Movement is unavailable.")
	}
	if !Fresh(o.CloudAt, now) {
		if o.Fault != "" {
			return errors.New(o.Fault)
		}
		return errors.New("Waiting for a timestamped floor and obstacle scan.")
	}
	return nil
}

func (o *Observations) Receive(topic string, raw []byte, isBinary bool, received, now time.Time) (err error) {
	defer func() {
		if topic == PoseTopic {
			if err == nil {
				o.PoseFault = ""
			} else {
				o.PoseFault = err.Error()
			}
		}
		if err != nil && topic == CloudTopic {
			o.CloudAt = time.Time{}
			o.Fault = err.Error()
		}
	}()
	if !Fresh(received, now) {
		return ErrDelayedObservation
	}
	if len(raw) > 8<<20 {
		return errors.New("Robot observation is too large.")
	}
	if topic == CloudTopic {
		return o.cloud(raw, isBinary, received, now)
	}
	if isBinary {
		return errors.New("Unexpected binary robot state.")
	}
	var envelope struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Topic != topic {
		return errors.New("Invalid robot state envelope.")
	}
	if topic == JointTopic {
		var state struct {
			Motors []struct {
				Q *float64 `json:"q"`
			} `json:"motor_state"`
		}
		if json.Unmarshal(envelope.Data, &state) != nil || len(state.Motors) < 12 {
			return errors.New("Joint positions unavailable.")
		}
		q := make([]float64, 12)
		for i := range q {
			v := state.Motors[i].Q
			if v == nil || !finite(*v) || math.Abs(*v) > 4 {
				return errors.New("Invalid joint position.")
			}
			q[i] = *v
		}
		o.Joints = q
		o.JointsAt = received
		return nil
	}
	if topic != PoseTopic {
		return errors.New("Unsupported robot topic.")
	}
	var state struct {
		Position []float64 `json:"position"`
		Stamp    struct {
			Sec     int64 `json:"sec"`
			Nanosec int64 `json:"nanosec"`
		} `json:"stamp"`
		IMU struct {
			Quaternion []float64 `json:"quaternion"`
		} `json:"imu_state"`
	}
	if json.Unmarshal(envelope.Data, &state) != nil || len(state.Position) != 3 || len(state.IMU.Quaternion) != 4 {
		return errors.New("Go2 odometry needs position and orientation.")
	}
	for _, v := range append(state.Position, state.IMU.Quaternion...) {
		if !finite(v) {
			return errors.New("Invalid Go2 pose.")
		}
	}
	q := state.IMU.Quaternion
	norm := 0.0
	for _, v := range q {
		norm += v * v
	}
	if math.Abs(norm-1) > 0.02 {
		return errors.New("Invalid Go2 orientation.")
	}
	var capture time.Time
	if state.Stamp.Sec > 0 && state.Stamp.Nanosec >= 0 && state.Stamp.Nanosec < 1_000_000_000 {
		capture = time.Unix(state.Stamp.Sec, state.Stamp.Nanosec).Add(-o.ClockOffset)
		if !Fresh(capture, now) {
			return ErrStalePoseCapture
		}
	}
	if !capture.IsZero() && capture.Equal(o.Pose.Captured) {
		return ErrDuplicatePose
	}
	if !capture.IsZero() && !o.Pose.Captured.IsZero() && capture.Before(o.Pose.Captured) {
		return errors.New("Go2 odometry restarted or repeated. Align the robot again.")
	}
	p := Point{state.Position[0], state.Position[1]}
	height := state.Position[2]
	if !p.Valid() || height < 0.10 || height > 0.75 {
		return errors.New("Go2 must be standing on level ground.")
	}
	yaw := math.Atan2(2*(q[0]*q[3]+q[1]*q[2]), 1-2*(q[2]*q[2]+q[3]*q[3]))
	up := 1 - 2*(q[1]*q[1]+q[2]*q[2])
	if up < 0.90 {
		return errors.New("Go2 is tilted. Movement stopped.")
	}
	if !o.Pose.Received.IsZero() {
		dt := received.Sub(o.Pose.Received).Seconds()
		if dt <= 0 || p.Distance(o.Pose.Position) > 0.15+1.2*dt || math.Abs(math.Remainder(yaw-o.Pose.Yaw, 2*math.Pi)) > 0.2+1.5*dt {
			return errors.New("Go2 pose jumped. Align the robot again.")
		}
	}
	o.Pose = Pose{p, height, yaw, received, capture}
	return nil
}

// Unitree publishes an accumulated voxel map in odom. Capture timing is the
// default. Explicit supervised arrival timing supports rounded firmware stamps,
// without claiming to verify sensor capture age. Absent voxels are never free.
// Only observed level floor is traversable. This supports supervised waypoint
// movement; the accumulated map is not a safety-rated live obstacle sensor.
// Wire metadata: https://docs.dimensional.org/development/large_file_management/
func (o *Observations) cloud(raw []byte, isBinary bool, received, now time.Time) error {
	fail := func(message string) error { o.CloudAt = time.Time{}; o.Fault = message; return errors.New(message) }
	if !isBinary || len(raw) < 4 {
		return fail("Unsupported Go2 LiDAR frame.")
	}
	// Firmware uses either the extended 12-byte header or a 4-byte header.
	// The latter carries JSON length in its first uint16, not a format marker.
	offset, n := 4, int(binary.LittleEndian.Uint16(raw))
	if n == 2 && binary.LittleEndian.Uint16(raw[2:]) == 0 {
		if len(raw) < 12 {
			return fail("Invalid Go2 LiDAR header.")
		}
		offset, n = 12, int(binary.LittleEndian.Uint32(raw[4:]))
	}
	if n < 2 || n > 16384 || n > len(raw)-offset {
		return fail("Invalid Go2 LiDAR header.")
	}
	var envelope struct {
		Topic string `json:"topic"`
		Data  struct {
			Size       int         `json:"src_size"`
			Origin     []float64   `json:"origin"`
			Resolution float64     `json:"resolution"`
			Frame      string      `json:"frame_id"`
			Capture    json.Number `json:"stamp"`
		} `json:"data"`
	}
	if json.Unmarshal(raw[offset:offset+n], &envelope) != nil || envelope.Topic != CloudTopic {
		return fail("Invalid Go2 LiDAR metadata.")
	}
	m := envelope.Data
	capture, captureErr := m.Capture.Float64()
	if captureErr != nil || !finite(capture) || capture <= 0 {
		return fail("The Go2 obstacle map has no valid capture timestamp.")
	}
	// Some Go2 firmware serializes Unix seconds with six mantissa decimals,
	// e.g. 1.792944e+09. A live packet then has only 1,000-second precision.
	// Report that loss explicitly; receive time cannot restore capture time.
	precision := timestampResolution(m.Capture.String())
	o.TimingUnavailable = precision > .25
	if o.TimingUnavailable && !o.AllowArrivalTime {
		return fail("LiDAR is arriving, but its capture timestamp is too coarse for navigation.")
	}
	captured := time.Unix(0, int64(capture*1e9)).Add(-o.ClockOffset)
	if !Fresh(o.Pose.Received, now) || !Fresh(o.Pose.Captured, now) {
		return fail("LiDAR needs fresh, timestamped Go2 odometry.")
	}
	usingArrival := o.TimingUnavailable && o.AllowArrivalTime
	if usingArrival {
		// Only the known rounded-clock case is supported. Keep the coarse epoch
		// sanity check, reject clock rollback and duplicate gateway deliveries.
		if precision > 1000 || math.Abs(captured.Sub(o.Pose.Captured).Seconds()) > precision/2+MaxAge.Seconds() || capture < o.CloudCapture ||
			!received.After(o.CloudReceived) || math.Abs(received.Sub(o.Pose.Received).Seconds()) > .2 {
			return fail("LiDAR arrivals and odometry are delayed, repeated or out of sync.")
		}
		captured = received
	} else if !Fresh(captured, now) || capture <= o.CloudCapture || math.Abs(captured.Sub(o.Pose.Captured).Seconds()) > .2 {
		return fail("LiDAR and odometry are stale or out of sync.")
	}
	if m.Frame != "odom" || m.Size <= 0 || m.Size > 2<<20 || len(m.Origin) != 3 || !finite(m.Resolution) || m.Resolution < .02 || m.Resolution > .10 {
		return fail("LiDAR frame, resolution or bounds are unsupported.")
	}
	for _, v := range m.Origin {
		if !finite(v) || math.Abs(v) > 10000 {
			return fail("Invalid LiDAR origin.")
		}
	}
	buf := make([]byte, m.Size)
	decoded, err := lz4.UncompressBlock(raw[offset+n:], buf)
	if err != nil || decoded != m.Size {
		return fail("Could not decode Go2 LiDAR.")
	}
	var points []Vertex
	// Bit and axis order match the existing Go2 ROS bridge decoder. Z is up.
	for i, b := range buf {
		if b == 0 {
			continue
		}
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(7-bit)) == 0 {
				continue
			}
			x := float64((i%16)*8+bit)*m.Resolution + m.Origin[0]
			y := float64((i%2048)/16)*m.Resolution + m.Origin[1]
			z := float64(i/2048)*m.Resolution + m.Origin[2]
			p := Point{x, y}
			if p.Distance(o.Pose.Position) > 10 {
				continue
			}
			points = append(points, Vertex{x, y, z})
			if len(points) > 500000 {
				return fail("LiDAR contains too many points for local navigation.")
			}
		}
	}
	// Decode structure before floor validation. A blocked navigation floor must
	// not disable a visual position suggestion from the same valid packet.
	o.Landmarks = structuralLandmarks(points, o.Pose.Height)
	o.CloudCapture = capture
	o.CloudReceived = received
	o.UsingArrivalTime = usingArrival
	o.CloudAt = captured
	o.Fault = ""
	o.Grid = Grid{}
	o.FloorFault = ""
	o.Surfaces = SurfaceCloud{}
	floor, err := observedFloorHeight(points, o.Pose.Position)
	if err != nil {
		o.FloorFault = err.Error()
		return nil
	}
	// Include supported lower walls even when upper returns pass readiness.
	// Passing a minimum count is not a reason to discard the rest of the room;
	// doing so also switches the matching geometry abruptly at that threshold.
	seen := make(map[Cell]bool, len(o.Landmarks))
	for _, p := range o.Landmarks {
		seen[cell(p)] = true
	}
	for _, p := range lowerWallLandmarks(points, floor, o.Pose.Height) {
		if !seen[cell(p)] {
			o.Landmarks = append(o.Landmarks, p)
			seen[cell(p)] = true
		}
	}
	o.Surfaces = ExtractSurfaces(points, floor)
	g := navigationGrid(points, floor)
	o.LocalNavigationSamples = nil
	for _, v := range points {
		if (Point{v.X, v.Y}).Distance(o.Pose.Position) <= 1 {
			o.LocalNavigationSamples = append(o.LocalNavigationSamples, v)
		}
	}
	if len(g.Floor) < 100 {
		o.FloorFault = "The current scan does not show enough level floor."
		return nil
	}
	o.Grid = g
	o.FloorHeight = floor
	return nil
}

// Require substantial vertical support above body height. Short chair sides
// and isolated tabletop/clutter returns must not dominate wall matching.
// This deliberately does not classify navigable floor.
func structuralLandmarks(points []Vertex, bodyHeight float64) []Point {
	return wallStripLandmarks(points, bodyHeight-.05, bodyHeight+1.2)
}

type verticalColumn struct {
	low, high float64
	levels    uint64
}

func collectVerticalColumns(points []Vertex, bottom, top float64) map[Cell]verticalColumn {
	columns := map[Cell]verticalColumn{}
	for _, v := range points {
		if !finite(v.X) || !finite(v.Y) || !finite(v.Z) || v.Z < bottom || v.Z > top {
			continue
		}
		level := math.Floor((v.Z - bottom) / .05)
		if level < 0 || level >= 64 {
			continue
		}
		key := cell(Point{v.X, v.Y})
		c, exists := columns[key]
		if !exists {
			c.low, c.high = v.Z, v.Z
		}
		c.low, c.high = math.Min(c.low, v.Z), math.Max(c.high, v.Z)
		c.levels |= 1 << uint(level)
		columns[key] = c
	}
	return columns
}

func verticalColumns(points []Vertex, bottom, top float64) map[Cell]bool {
	columns := collectVerticalColumns(points, bottom, top)
	result := map[Cell]bool{}
	for c, v := range columns {
		if v.high-v.low >= .50-1e-6 && bits.OnesCount64(v.levels) >= 9 {
			result[c] = true
		}
	}
	return result
}

// Lower surfaces require at least six tall columns spanning 60 cm along a
// narrow horizontal strip. This rejects isolated legs and flat tabletops.
// These are localization observations only, never navigation free space.
func lowerWallLandmarks(points []Vertex, floor, bodyHeight float64) []Point {
	if !finite(floor) || !finite(bodyHeight) || bodyHeight-floor < .10 || bodyHeight-floor > .75 {
		return nil
	}
	return wallStripLandmarks(points, floor+.10, bodyHeight+1.2)
}

// Sparse returns may sample different heights in neighboring wall columns.
// Each column still spans 50 cm and has at least three height samples; the
// whole neighborhood must form a thin plane, with nine height bins overall.
// No missing columns or height samples are invented.
func wallStripLandmarks(points []Vertex, bottom, top float64) []Point {
	columns := collectVerticalColumns(points, bottom, top)
	for c, v := range columns {
		if v.high-v.low < .50-1e-6 || bits.OnesCount64(v.levels) < 3 {
			delete(columns, c)
		}
	}
	var result []Point
	for c := range columns {
		var neighbors []Point
		var levels uint64
		for y := -4; y <= 4; y++ {
			for x := -4; x <= 4; x++ {
				if x*x+y*y > 20 {
					continue
				}
				if v, ok := columns[Cell{c.X + x, c.Y + y}]; ok {
					neighbors = append(neighbors, Point{float64(x) * CellSize, float64(y) * CellSize})
					levels |= v.levels
				}
			}
		}
		if _, planar := planarWallDirection(neighbors); planar && bits.OnesCount64(levels) >= 9 {
			result = append(result, center(c))
		}
	}
	return result
}

// The voxel map can contain occupied volume below a walking surface. Its lowest
// voxel is not the floor. Find a dominant upper surface in nearby columns with
// body clearance. Outlying columns remain obstacles/unknown in navigationGrid;
// accepting a floor reference never makes the whole neighborhood traversable.
func observedFloorHeight(points []Vertex, origin Point) (float64, error) {
	tops := map[Cell]float64{}
	for _, v := range points {
		p := Point{v.X, v.Y}
		d := p.Distance(origin)
		if d < .35 || d > 1.5 || v.Z > .9 {
			continue
		}
		c := cell(p)
		if z, ok := tops[c]; !ok || v.Z > z {
			tops[c] = v.Z
		}
	}
	for c, z := range tops {
		if z < -.25 || z > .20 {
			delete(tops, c)
		}
	}
	fail := errors.New("LiDAR cannot identify a level floor around Go2. Movement is unavailable.")
	if len(tops) < 100 {
		return 0, fail
	}
	heights := make([]float64, 0, len(tops))
	for _, z := range tops {
		heights = append(heights, z)
	}
	sort.Float64s(heights)
	height, support := heights[0], 0
	// Select the strongest horizontal band, not the midpoint between competing
	// surfaces. A second disjoint surface with comparable support is ambiguous.
	counts := map[float64]int{}
	for i, z := range heights {
		if i > 0 && math.Abs(z-heights[i-1]) < 1e-6 {
			continue
		}
		n := 0
		for _, h := range heights {
			if math.Abs(h-z) <= .06+1e-6 {
				n++
			}
		}
		counts[z] = n
		if n > support {
			height, support = z, n
		}
	}
	if support < 100 || float64(support)/float64(len(tops)) < .6 {
		return 0, fail
	}
	for z, n := range counts {
		if math.Abs(z-height) > .12 && float64(n)*1.5 >= float64(support) {
			return 0, fail
		}
	}
	var quadrants [4]int
	for c, z := range tops {
		if math.Abs(z-height) > .06+1e-6 {
			continue
		}
		p := center(c)
		q := 0
		if p.X >= origin.X {
			q += 1
		}
		if p.Y >= origin.Y {
			q += 2
		}
		quadrants[q]++
	}
	for _, n := range quadrants {
		if n < 15 {
			return 0, fail
		}
	}
	return height, nil
}

func navigationGrid(points []Vertex, floor float64) Grid {
	g := Grid{map[Cell]bool{}, map[Cell]bool{}}
	lower := map[Cell]float64{}
	for _, v := range points {
		c, h := cell(Point{v.X, v.Y}), v.Z-floor
		if math.Abs(h) <= .06+1e-6 {
			g.Floor[c] = true
		}
		if h > .08 && h < .9 {
			g.Obstacles[c] = true
		}
		if h < -.08 {
			if old, ok := lower[c]; !ok || v.Z > old {
				lower[c] = v.Z
			}
		}
	}
	// Buried occupancy below an observed floor is not a hole. A coherent lower
	// surface with no floor above it is a drop. Isolated lower voxels alone do
	// not establish a surface; those cells stay unknown, not free.
	buried := map[Cell]float64{}
	for c, z := range lower {
		if g.Floor[c] {
			buried[c] = z
			delete(lower, c)
		}
	}
	// An occluded patch can contain accumulated occupied volume with locally
	// flat shelves at several depths. A small neighborhood on a shelf is not
	// evidence of a drop. Validate the entire connected top surface instead.
	// Nonplanar regions remain unknown, never observed free floor.
	visited := make(map[Cell]bool, len(lower))
	for seed := range lower {
		if visited[seed] {
			continue
		}
		visited[seed] = true
		component := []Cell{seed}
		var samples []Vertex
		continuesBeneathFloor := false
		for i := 0; i < len(component); i++ {
			c := component[i]
			samples = append(samples, Vertex{float64(c.X-seed.X) * CellSize, float64(c.Y-seed.Y) * CellSize, lower[c]})
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					next := Cell{c.X + dx, c.Y + dy}
					// A lower occupied layer continuing under measured level
					// floor is buried volume, not independent evidence of a drop.
					// Leave the occluded patch unknown; never invent free floor.
					if math.Abs(float64(dx))+math.Abs(float64(dy)) == 1 {
						if z, ok := buried[next]; ok && math.Abs(z-lower[c]) <= .025+1e-6 {
							continuesBeneathFloor = true
						}
					}
					if _, ok := lower[next]; ok && !visited[next] {
						visited[next] = true
						component = append(component, next)
					}
				}
			}
		}
		if !continuesBeneathFloor && observedLowerSurface(samples) {
			for _, c := range component {
				g.Obstacles[c] = true
			}
		}
	}
	return g
}

// Resolution implied by the number of serialized decimal places, in seconds.
func timestampResolution(value string) float64 {
	mantissa, exponent := value, 0
	if at := strings.IndexAny(value, "eE"); at >= 0 {
		mantissa = value[:at]
		parsed, err := strconv.Atoi(value[at+1:])
		if err != nil {
			return math.Inf(1)
		}
		exponent = parsed
	}
	decimals := 0
	if at := strings.IndexByte(mantissa, '.'); at >= 0 {
		decimals = len(mantissa) - at - 1
	}
	return math.Pow10(exponent - decimals)
}

// SurfaceReady requires fresh measurements and usable geometric samples, not
// a traversable departure footprint. It is separate from navigation readiness.
func (o *Observations) SurfaceReady(now time.Time) error {
	if err := o.freshCloud(now); err != nil {
		return err
	}
	if len(o.Surfaces.Surfaces) < 24 {
		return errors.New("Waiting for observed surfaces at multiple heights.")
	}
	return nil
}

// Lower occupied volume is not itself a measured drop. Require a locally
// level top surface spanning both horizontal axes. Irregular buried voxels
// remain unknown floor, so only the explicitly authorized fixed departure
// footprint can tolerate them; they never make a route ahead traversable.
func observedLowerSurface(points []Vertex) bool {
	if len(points) < 4 {
		return false
	}
	var x, y, z float64
	for _, p := range points {
		x += p.X
		y += p.Y
		z += p.Z
	}
	n := float64(len(points))
	x /= n
	y /= n
	z /= n
	var xx, xy, yy, xz, yz float64
	for _, p := range points {
		dx, dy, dz := p.X-x, p.Y-y, p.Z-z
		xx += dx * dx
		xy += dx * dy
		yy += dy * dy
		xz += dx * dz
		yz += dy * dz
	}
	det := xx*yy - xy*xy
	if det < 1e-6 {
		return false
	}
	a, b := (xz*yy-yz*xy)/det, (yz*xx-xz*xy)/det
	if math.Hypot(a, b) > .2 {
		return false
	}
	var residual float64
	for _, p := range points {
		d := p.Z - z - a*(p.X-x) - b*(p.Y-y)
		residual += d * d
		if math.Abs(d) > .05 {
			return false
		}
	}
	return residual/n < .025*.025
}
