package go2nav

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func cloudFixture() (CloudObservation, Profile) {
	s, p := tiltFixture()
	ranges := make([]float64, CloudRayCount)
	for i := range ranges {
		ranges[i] = 6
	}
	return CloudObservation{Model: CloudModelV1, SourceDigest: CloudSourceDigest, Frame: "utlidar_lidar",
		PublisherGID: strings.Repeat("01", 24), SerializedSHA256: strings.Repeat("ab", 32),
		CapturedUnixNS: s.CapturedUnixNS, Scan: s, Ranges: ranges}, p
}

func cloudOnlyRings(o *CloudObservation, rings ...int) {
	for i := range o.Ranges {
		o.Ranges[i] = math.Inf(1)
	}
	for _, r := range rings {
		for a := 0; a < CloudAzimuths; a++ {
			o.Ranges[r*CloudAzimuths+a] = 6
		}
	}
	copy(o.Scan.Ranges, o.Ranges[12*CloudAzimuths:13*CloudAzimuths])
}

func cloudCell(t *testing.T, e CloudEvidence, p Point) CloudCellEvidence {
	t.Helper()
	c := at(p)
	for _, v := range e.Cells {
		if v.X == c.X && v.Y == c.Y {
			return v
		}
	}
	t.Fatal("cell missing from bounded evidence")
	return CloudCellEvidence{}
}

func TestCloudExplicitModelAndBounds(t *testing.T) {
	bad := map[string]func(*CloudObservation, *Profile){
		"model":                           func(o *CloudObservation, p *Profile) { o.Model = "" },
		"source":                          func(o *CloudObservation, p *Profile) { o.SourceDigest = "unverified" },
		"frame":                           func(o *CloudObservation, p *Profile) { o.Frame = "base_link" },
		"gid":                             func(o *CloudObservation, p *Profile) { o.PublisherGID = strings.Repeat("0", 48) },
		"digest":                          func(o *CloudObservation, p *Profile) { o.SerializedSHA256 = "not-a-digest" },
		"stamp":                           func(o *CloudObservation, p *Profile) { o.CapturedUnixNS++ },
		"short":                           func(o *CloudObservation, p *Profile) { o.Ranges = o.Ranges[:8999] },
		"long":                            func(o *CloudObservation, p *Profile) { o.Ranges = append(o.Ranges, 6) },
		"nan":                             func(o *CloudObservation, p *Profile) { o.Ranges[0] = math.NaN() },
		"negative inf":                    func(o *CloudObservation, p *Profile) { o.Ranges[0] = math.Inf(-1) },
		"range":                           func(o *CloudObservation, p *Profile) { o.Ranges[0] = 12 + 2*CloudRangeQuantizationM },
		"horizontal disagreement":         func(o *CloudObservation, p *Profile) { o.Ranges[4320] = 5 },
		"horizontal missing disagreement": func(o *CloudObservation, p *Profile) { o.Ranges[4320] = math.Inf(1) },
		"not360":                          func(o *CloudObservation, p *Profile) { o.Scan.Ranges = o.Scan.Ranges[:359] },
		"shifted azimuth":                 func(o *CloudObservation, p *Profile) { o.Scan.AngleMin = 0 },
		"no pose":                         func(o *CloudObservation, p *Profile) { o.Scan.TiltedPose = nil },
		"no slab":                         func(o *CloudObservation, p *Profile) { p.TiltedScan = nil },
		"height":                          func(o *CloudObservation, p *Profile) { o.Scan.TiltedPose.BaseHeightMinM += .01 },
		"missing temporal uncertainty":    func(o *CloudObservation, p *Profile) { o.Scan.TiltedPose.OdomCapturedUnixNS -= 1000000 },
		"epoch":                           func(o *CloudObservation, p *Profile) { o.Scan.WorldEpoch = 0 },
	}
	for name, f := range bad {
		t.Run(name, func(t *testing.T) {
			o, p := cloudFixture()
			f(&o, &p)
			g, e, err := BuildCloud(o, area(), p)
			if !errors.Is(err, ErrInvalid) || g != nil || len(e.Cells) != 0 {
				t.Fatalf("got %v %v", g, err)
			}
		})
	}
	o, p := cloudFixture()
	o.Scan.AngleMin = float64(float32(-math.Pi))
	o.Scan.AngleIncrement = float64(float32(2 * math.Pi / 360))
	if _, _, e := BuildCloud(o, area(), p); e != nil {
		t.Fatal(e)
	}
}

func TestCloudWholeRingUnionDoesNotPoolMissingBrackets(t *testing.T) {
	o, p := cloudFixture()
	cloudOnlyRings(&o, 11, 13)
	_, e, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	q := Point{1.05, .05}
	c := cloudCell(t, e, q)
	if c.State != "free" || c.WitnessRings != (1<<11|1<<13) {
		t.Fatalf("whole-ring witnesses: %+v", c)
	}
	// Each ring has a different missing bracket. A pointwise maximum over
	// rings would invent a complete ring; neither ring certifies this cell.
	o.Ranges[11*360+180] = math.Inf(1)
	o.Ranges[13*360+181] = math.Inf(1)
	_, e, err = BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	c = cloudCell(t, e, q)
	if c.State != "unknown" || c.WitnessRings != 0 {
		t.Fatalf("partial rings pooled: %+v", c)
	}
	// Restoring one ring is sufficient, without restoring the other ring.
	o.Ranges[13*360+181] = 6
	_, e, err = BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	c = cloudCell(t, e, q)
	if c.State != "free" || c.WitnessRings != 1<<13 {
		t.Fatalf("independent witness lost: %+v", c)
	}
}

func TestCloudFloorAmbiguityNeedsIndependentCompleteWitness(t *testing.T) {
	o, p := cloudFixture()
	cloudOnlyRings(&o, 12)
	// A nominal floor return is ambiguous after the complete height/error
	// interval. Only a separate horizontal ring can resolve its touched cells.
	o.Ranges[180] = .82
	_, e, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	r := e.Rays[180]
	if r.Class != "ambiguous" || r.EndZMinM >= 0 || r.EndZMaxM <= 0 {
		t.Fatalf("floor evidence %+v", r)
	}
	c := cloudCell(t, e, r.EndpointXY)
	if c.State != "free" || c.AmbiguousEndpointRings != 1 || c.ResolvedAmbiguousRings != 1 || c.WitnessRings&(1<<12) == 0 {
		t.Fatalf("floor resolution %+v", c)
	}
	o.Ranges[12*360+180] = math.Inf(1)
	o.Scan.Ranges[180] = math.Inf(1)
	_, e, err = BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	c = cloudCell(t, e, r.EndpointXY)
	if c.State != "unknown" || c.ResolvedAmbiguousRings != 0 || c.Reason != "ambiguous_endpoint" {
		t.Fatalf("missing witness erased ambiguity %+v", c)
	}
}

func TestCloudDefiniteEndpointVetoAndOutsideNominalFootprint(t *testing.T) {
	for _, r := range []float64{.7, 2.77} {
		t.Run(fmt.Sprint(r), func(t *testing.T) {
			o, p := cloudFixture()
			cloudOnlyRings(&o, 12, 13)
			o.Ranges[12*360+180] = r
			o.Scan.Ranges[180] = r
			_, e, err := BuildCloud(o, area(), p)
			if err != nil {
				t.Fatal(err)
			}
			ray := e.Rays[12*360+180]
			if ray.Class != "definite_interior" {
				t.Fatalf("endpoint %+v", ray)
			}
			q := ray.EndpointXY
			if r > 2 {
				q = Point{2.95, .05}
				if area().contains(ray.EndpointXY) {
					t.Fatal("fixture endpoint must be outside area")
				}
			}
			c := cloudCell(t, e, q)
			if c.State != "occupied" || c.DefiniteEndpointRings&(1<<12) == 0 {
				t.Fatalf("endpoint failed to veto %+v", c)
			}
		})
	}
}

func TestCloudAboveMinimumHeightIsNotIgnored(t *testing.T) {
	o, p := cloudFixture()
	cloudOnlyRings(&o)
	o.Ranges[24*360+180] = 1.5
	_, e, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	r := e.Rays[24*360+180]
	if r.EndZMinM <= p.TiltedScan.ObstacleMinHeightM || r.Class != "ambiguous" {
		t.Fatalf("above-top return %+v", r)
	}
	c := cloudCell(t, e, r.EndpointXY)
	if c.AmbiguousEndpointRings != 1<<24 || c.State != "unknown" {
		t.Fatalf("minimum treated as ceiling %+v", c)
	}
}

func TestCloudQuantizationAndFullAttitude(t *testing.T) {
	o, p := cloudFixture()
	o.Scan.Robot.Yaw = .7
	o.Scan.TiltedPose.QuaternionWXYZ = tiltQuaternion(.12, -.11, .7)
	_, e, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Independent Euler sequence, not the production quaternion matrix.
	rotate := func(v [3]float64) [3]float64 {
		r, pp, y := .12, -.11, .7
		a := [3]float64{v[0], math.Cos(r)*v[1] - math.Sin(r)*v[2], math.Sin(r)*v[1] + math.Cos(r)*v[2]}
		b := [3]float64{math.Cos(pp)*a[0] + math.Sin(pp)*a[2], a[1], -math.Sin(pp)*a[0] + math.Cos(pp)*a[2]}
		return [3]float64{math.Cos(y)*b[0] - math.Sin(y)*b[1], math.Sin(y)*b[0] + math.Cos(y)*b[1], b[2]}
	}
	m := rotate([3]float64{.28, 0, .08})
	if distance(e.Origin, Point{m[0], m[1]}) > 1e-12 {
		t.Fatal("mount rotation")
	}
	for i := 0; i < 9000; i += 97 {
		r := e.Rays[i]
		elev := float64(-30+2.5*float64(i/360)) * math.Pi / 180
		a := -math.Pi + float64(i%360)*math.Pi/180
		u := rotate([3]float64{math.Cos(elev) * math.Cos(a), math.Cos(elev) * math.Sin(a), math.Sin(elev)})
		if distance(r.EndpointXY, Point{m[0] + 6*u[0], m[1] + 6*u[1]}) > 1e-11 {
			t.Fatalf("ray %d", i)
		}
		if r.EndpointRadiusM <= e.OriginErrorM+p.TiltedScan.RangeErrorM+CloudRangeQuantizationM {
			t.Fatal("angular endpoint uncertainty omitted")
		}
	}
	if e.AttitudeErrorRad <= o.Scan.YawUncertaintyRad+p.TiltedScan.AttitudeErrorRad {
		t.Fatal("quantization omitted")
	}
}

func TestCloudMissingAllRaysAndEvidenceIsolation(t *testing.T) {
	o, p := cloudFixture()
	cloudOnlyRings(&o)
	g, e, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.cells) != 0 || len(e.Rays) != 9000 {
		t.Fatal("missing rays invented occupancy")
	}
	for _, c := range e.Cells {
		if c.State != "unknown" || c.WitnessRings != 0 {
			t.Fatalf("missing %+v", c)
		}
	}
	o, p = cloudFixture()
	g, e, err = BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !g.ClearSweep(Point{}, Point{.3, 0}, 0) {
		t.Fatal("clear synthetic baseline")
	}
	for i := range o.Ranges {
		o.Ranges[i] = math.Inf(1)
	}
	o.Scan.TiltedPose.QuaternionWXYZ = [4]float64{}
	for i := range e.Cells {
		e.Cells[i].State = "occupied"
	}
	for i := range e.Rays {
		e.Rays[i].SupportM = 0
	}
	if !g.ClearSweep(Point{}, Point{.3, 0}, 0) {
		t.Fatal("mutable evidence/input changed immutable grid")
	}
}

func TestCloudLegacyBuildUnchangedAndDeterministicBounds(t *testing.T) {
	o, p := cloudFixture()
	before, err := Build(o.Scan, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	_, one, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Build(o.Scan, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("legacy Build changed")
	}
	_, two, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(one)
	b, _ := json.Marshal(two)
	if string(a) != string(b) {
		t.Fatal("evidence not deterministic")
	}
	_, e, err := BuildCloud(o, Bounds{Point{-4, -4}, Point{4, 4}}, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Rays) != 9000 || len(e.Cells) != 81*81 {
		t.Fatal("size bounds")
	}
	if _, _, err = BuildCloud(o, Bounds{Point{-4.01, -4}, Point{4, 4}}, p); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversize area")
	}
}

func BenchmarkBuildCloudMaximumGrid(b *testing.B) {
	o, p := cloudFixture()
	bounds := Bounds{Point{-4, -4}, Point{4, 4}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := BuildCloud(o, bounds, p); err != nil {
			b.Fatal(err)
		}
	}
}
