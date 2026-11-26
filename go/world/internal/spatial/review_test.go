package spatial

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func simInput(id string) world.Input {
	return world.Input{ID: id, Origin: world.Simulated, Simulation: &simulated}
}

// Measurements that share a sensor and an instant must still have distinct
// ancestry ids: one UWB round, two sound sources heard at once, two gases
// from one sensor, and a manifest stamped like its sensor's first sample.
func TestInputIDIsInjective(t *testing.T) {
	var ms []Measurement
	for i := 0; i < 8; i++ {
		ms = append(ms, Range{Header: header("uwb"), Peer: DeviceID(fmt.Sprintf("beacon-%d", i)), Distance: 3, Sigma: 0.1})
	}
	ms = append(ms,
		Range{Header: header("uwb"), Peer: "beacon-0", PeerSensor: "uwb-b", Distance: 3, Sigma: 0.1},
		Bearing{Header: header("mic"), Azimuth: 0.1, Sigma: 0.05, Source: "alarm"},
		Bearing{Header: header("mic"), Azimuth: 0.9, Sigma: 0.05, Source: "compressor"},
		Bearing{Header: header("mic"), Azimuth: 2, Sigma: 0.05},
		Scalar{Header: header("gas"), Quantity: "co2", Value: 600, Unit: "ppm", Sigma: 10},
		Scalar{Header: header("gas"), Quantity: "voc", Value: 0.2, Unit: "ppm", Sigma: 0.05},
		Odometry{Header: header("odom")},
		SensorManifest{Header: header("odom"), Modality: KindOdometry, Model: "wheel"},
		SensorManifest{Header: header("uwb"), Modality: KindUwb, Model: "dw3000"},
	)
	seen := map[string]Measurement{}
	var inputs []world.Input
	for _, m := range ms {
		id := InputID(m)
		if other, dup := seen[id]; dup {
			t.Fatalf("%+v and %+v share the input id %q", other, m, id)
		}
		if !strings.HasPrefix(id, "robot-1/") || strings.Count(id, "/") < 3 {
			t.Fatalf("input id %q", id)
		}
		seen[id] = m
		inputs = append(inputs, simInput(id))
	}
	if _, err := InferredProvenance("localize", "1", "wendy-test", inputs); err != nil {
		t.Fatalf("one instant of distinct measurements must be valid ancestry: %v", err)
	}
	if got := InputID(ms[0]); got != "robot-1/uwb/uwb/1800000000000000000/beacon-0" {
		t.Fatalf("range input id %q", got)
	}
	if got := InputID(ms[15]); got != "robot-1/odom/manifest/1800000000000000000" {
		t.Fatalf("manifest input id %q", got)
	}
}

func TestReduceInputs(t *testing.T) {
	// 3 robots, 600 scans each, with one duplicate and ranges to 8 beacons.
	var all []world.Input
	for r := 1; r <= 3; r++ {
		for k := 0; k < 600; k++ {
			all = append(all, simInput(fmt.Sprintf("robot-%d/lidar/lidar2d/%d", r, 1000+k*7)))
		}
	}
	for b := 0; b < 8; b++ {
		all = append(all, simInput(fmt.Sprintf("robot-1/uwb/uwb/5000/beacon-%d", b)))
	}
	all = append(all, all[17], simInput("survey-2026"))
	if _, err := InferredProvenance("occupancy", "1", "wendy-test", all); err == nil {
		t.Fatalf("%d inputs accepted, limit %d", len(all), MaxInputs)
	}
	got := ReduceInputs(all)
	var ids []string
	for _, in := range got {
		ids = append(ids, in.ID)
	}
	want := []string{
		"robot-1/lidar/lidar2d/1000", "robot-1/lidar/lidar2d/5193", "robot-1/uwb/uwb/5000/beacon-0", "robot-1/uwb/uwb/5000/beacon-7",
		"robot-2/lidar/lidar2d/1000", "robot-2/lidar/lidar2d/5193", "robot-3/lidar/lidar2d/1000", "robot-3/lidar/lidar2d/5193", "survey-2026",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("reduced to %v\nwant %v", ids, want)
	}
	if _, err := InferredProvenance("occupancy", "1", "wendy-test", got); err != nil {
		t.Fatal(err)
	}
	// The same evidence in another order reduces to the same list.
	reversed := make([]world.Input, len(all))
	for i, in := range all {
		reversed[len(all)-1-i] = in
	}
	if !reflect.DeepEqual(ReduceInputs(reversed), got) {
		t.Fatal("ReduceInputs depends on input order")
	}
	// A list that fits is kept whole, sorted and without duplicates.
	few := ReduceInputs([]world.Input{simInput("b/s/k/2"), simInput("a/s/k/1"), simInput("b/s/k/2"), simInput("b/s/k/1"), simInput("b/s/k/3")})
	if len(few) != 4 || few[0].ID != "a/s/k/1" || few[3].ID != "b/s/k/3" {
		t.Fatalf("short list: %+v", few)
	}
	// More streams than fit are thinned evenly, ends kept.
	var wide []world.Input
	for d := 0; d < 100; d++ {
		wide = append(wide, simInput(fmt.Sprintf("dev-%03d/cam/camera/1", d)), simInput(fmt.Sprintf("dev-%03d/cam/camera/2", d)))
	}
	thin := ReduceInputs(wide)
	if len(thin) != MaxInputs || thin[0].ID != "dev-000/cam/camera/1" || thin[MaxInputs-1].ID != "dev-099/cam/camera/2" {
		t.Fatalf("thinned to %d: %v … %v", len(thin), thin[0].ID, thin[len(thin)-1].ID)
	}
	if len(ReduceInputs(nil)) != 0 {
		t.Fatal("empty ancestry")
	}
}

// What World transport would refuse must be refused at ToComponent.
func TestToComponentTransportBounds(t *testing.T) {
	version := world.Version{Epoch: 1, Sequence: 1}
	pose := samplePayloads()[2].(DevicePose)
	prov := testProvenance(t, "localize")
	many := prov
	many.Inputs = nil
	for i := 0; i <= MaxInputs; i++ {
		many.Inputs = append(many.Inputs, simInput(fmt.Sprintf("robot-1/lidar/lidar2d/%d", i)))
	}
	if _, err := ToComponent(testScope, version, siteFrame, testTiming(), time.Minute, many, pose); err == nil {
		t.Fatalf("%d inputs accepted", len(many.Inputs))
	}
	// A payload inside its own limit under ancestry that pushes the whole
	// record past a signed envelope.
	long := prov
	long.Inputs = nil
	for i := 0; i < MaxInputs; i++ {
		long.Inputs = append(long.Inputs, simInput(fmt.Sprintf("%s/%d", strings.Repeat("d", 600), i)))
	}
	blob := hugePayload{Blob: strings.Repeat("x", MaxPayloadBytes-64)}
	if _, err := ToComponent(testScope, version, siteFrame, testTiming(), time.Minute, prov, blob); err != nil {
		t.Fatalf("payload inside the limit: %v", err)
	}
	if _, err := ToComponent(testScope, version, siteFrame, testTiming(), time.Minute, long, blob); err == nil || !strings.Contains(err.Error(), "component is") {
		t.Fatalf("oversized component: %v", err)
	}
	// A device-local pose must not be published under the site frame, where
	// region queries would report it as located in the site.
	local := pose
	local.FrameID = "map/robot-1"
	if _, err := ToComponent(testScope, version, siteFrame, testTiming(), time.Minute, prov, local); err == nil {
		t.Fatal("device-local pose accepted under the site frame")
	}
	if _, err := ToComponent(testScope, version, world.FrameRef{ID: "map/robot-1", Revision: "1"}, testTiming(), time.Minute, prov, local); err != nil {
		t.Fatalf("device-local pose under its own frame: %v", err)
	}
	alignment := samplePayloads()[15].(FrameAlignment)
	alignment.ParentFrameID, alignment.ChildFrameID = alignment.ChildFrameID, alignment.ParentFrameID
	if _, err := ToComponent(testScope, version, siteFrame, testTiming(), time.Minute, prov, alignment); err == nil {
		t.Fatal("alignment into another frame accepted under the site frame")
	}
}

func TestOdometryNoiseCovariance(t *testing.T) {
	n := OdometryNoise{TransPerMetre: 0.02, YawPerMetre: 0.002, YawPerRadian: 0.01}
	// 5 cm forward: the white part (k·√d = 4.5 mm) dwarfs the bias (k·d = 1 mm).
	c := n.Covariance(geom.Pose2{X: 0.05})
	near := func(got, want float64, what string) {
		t.Helper()
		if math.Abs(got-want) > 1e-12*(1+math.Abs(want)) {
			t.Fatalf("%s = %g, want %g", what, got, want)
		}
	}
	near(c[0], 0.02*0.02*(0.05+0.05*0.05), "forward variance")
	near(c[4], 0.02*0.02*0.05, "lateral variance")
	near(c[8], 0.002*0.002*(0.05+0.05*0.05), "yaw variance")
	near(c[1], 0, "forward-lateral covariance of a forward step")
	// The scale bias lies along the direction of travel, whatever that is.
	d := geom.Pose2{X: 0.6, Z: 0.8, Yaw: -0.5}
	c = n.Covariance(d)
	along, across := geom.Vec2{X: 0.6, Z: 0.8}, geom.Vec2{X: -0.8, Z: 0.6}
	quad := func(v geom.Vec2) float64 { return v.X*(c[0]*v.X+c[1]*v.Z) + v.Z*(c[3]*v.X+c[4]*v.Z) }
	near(quad(along), 0.02*0.02*(1+1), "variance along travel after 1 m")
	near(quad(across), 0.02*0.02, "variance across travel after 1 m")
	near(c[8], 0.002*0.002*2+0.01*0.01*(0.5+0.25), "yaw variance after 1 m and 0.5 rad")
	if !covariance(c[:], 3) || c[2] != 0 || c[5] != 0 {
		t.Fatalf("covariance: %v", c)
	}
	if n.Covariance(geom.Pose2{}) != (geom.Mat3{}) {
		t.Fatal("no motion, no error")
	}
	if _, err := c.Inverse(); err != nil {
		t.Fatalf("a moving step's covariance must invert to an information matrix: %v", err)
	}
}

// A ROS LaserScan sweeps counter-clockwise; this package's angles run from
// +X toward +Z, clockwise seen from above.
func TestScanFromROS(t *testing.T) {
	// ROS beam 0 at +90° (left of the robot), beam 2 at −90° (right).
	s := ScanFromROS(header("lidar"), math.Pi/2, -math.Pi/2, 0.1, 10, 0.02,
		[]float64{2, math.Inf(1), 3, math.NaN(), math.Inf(-1), 0, -1, 0.05, 11})
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	pts := s.Points()
	if len(pts) != 2 || pts[0].Dist(geom.Vec2{X: 0, Z: -2}) > 1e-12 || pts[1].Dist(geom.Vec2{X: 0, Z: 3}) > 1e-12 {
		t.Fatalf("left return must land at −z and right return at +z: %v", pts)
	}
	want := []BeamState{BeamHit, BeamNoReturn, BeamHit, BeamInvalid, BeamInvalid, BeamInvalid, BeamInvalid, BeamInvalid, BeamNoReturn}
	for i, w := range want {
		if _, _, state := s.Beam(i); state != w {
			t.Fatalf("beam %d: state %d, want %d", i, state, w)
		}
	}
	// The same convention as geom.Pose2FromROS: a point seen at ROS (x, y).
	if p := geom.Pose2FromROS(1, 2, 0); p.X != 1 || p.Z != -2 {
		t.Fatalf("pose: %+v", p)
	}
}

func TestLayerTile(t *testing.T) {
	gas, classes := sampleLayer(true), sampleLayer(false)
	if gas.EntityKey() != "layer/gas.co2/ground/1/-1" || gas.Type() != TypeLayerTile {
		t.Fatalf("key %q", gas.EntityKey())
	}
	if gas.Point().Dist(geom.Vec3{X: 0.3, Z: -0.1}) > 1e-12 || math.Abs(gas.UncertaintyM-0.1*math.Sqrt2) > 1e-12 {
		t.Fatalf("layer tile location: %+v", gas.Location)
	}
	// 612 ppm on a 4 ppm lattice from 400 is exact; 5000 ppm saturates at the
	// top level instead of wrapping into "no data".
	if v, ok := gas.Value(1, 1); !ok || v != 612 {
		t.Fatalf("value: %g %v", v, ok)
	}
	if v, ok := gas.Value(2, 1); !ok || v != 400+254*4 {
		t.Fatalf("saturated value: %g %v", v, ok)
	}
	if gas.ValueCode(-50) != 1 || gas.ValueCode(math.NaN()) != 1 || gas.ValueCode(401.9) != 1 || gas.ValueCode(402.1) != 2 {
		t.Fatal("value codes must round to the nearest level and never be the no-data code")
	}
	for _, at := range [][2]int{{0, 0}, {-1, 0}, {4, 0}, {0, 4}} {
		if _, ok := gas.Value(at[0], at[1]); ok {
			t.Fatalf("cell %v has no data", at)
		}
	}
	if _, ok := gas.Class(1, 1); ok {
		t.Fatal("a scalar layer has no classes")
	}
	if c, ok := classes.Class(1, 1); !ok || c != "wall" {
		t.Fatalf("class: %q %v", c, ok)
	}
	if c, ok := classes.Class(2, 1); !ok || c != "furniture" {
		t.Fatalf("class: %q %v", c, ok)
	}
	if _, ok := classes.Class(0, 0); ok {
		t.Fatal("unclassified cell")
	}
	if _, ok := classes.Value(1, 1); ok {
		t.Fatal("a class layer has no values")
	}
	// A full tile of either kind fits a component.
	full := LayerTile{Floor: "ground", Layer: "gas.co2", Side: MaxTileSide, CellSizeM: 0.05, Cells: make([]byte, MaxTileSide*MaxTileSide), Unit: "ppm", ValueStep: 1}
	for i := range full.Cells {
		full.Cells[i] = byte(i*31 + 7)
	}
	full.Seal(0)
	if _, err := ToComponent(testScope, world.Version{Epoch: 1, Sequence: 1}, siteFrame, testTiming(), time.Minute, testProvenance(t, "crossmodal"), full); err != nil {
		t.Fatalf("full layer tile: %v", err)
	}
	if o := TileOrigin(-2, 3, 4, 0.05); math.Abs(o[0]+0.4) > 1e-12 || math.Abs(o[1]-0.6) > 1e-12 {
		t.Fatalf("tile origin %v", o)
	}
}
