package go2nav

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// These tests exercise the conditional finite-angular solid-column model.
// Their synthetic ranges are not measurements of a scene or a physical proof.
func reviewCloud() (CloudObservation, Profile) {
	s, p := tiltFixture()
	for i := range s.Ranges {
		s.Ranges[i] = math.Inf(1)
	}
	o := CloudObservation{Model: CloudModelV1, SourceDigest: CloudSourceDigest,
		Frame: "utlidar_lidar", PublisherGID: strings.Repeat("1", 48),
		SerializedSHA256: strings.Repeat("2", 64), CapturedUnixNS: s.CapturedUnixNS,
		Scan: s, Ranges: make([]float64, CloudRayCount)}
	for i := range o.Ranges {
		o.Ranges[i] = math.Inf(1)
	}
	return o, p
}
func reviewRay(o *CloudObservation, ring, az int, r float64) {
	o.Ranges[ring*360+az] = r
	if ring == 12 {
		o.Scan.Ranges[az] = r
	}
}
func reviewRing(o *CloudObservation, ring int, r float64) {
	for az := 0; az < 360; az++ {
		reviewRay(o, ring, az, r)
	}
}
func reviewCell(t *testing.T, e CloudEvidence, c cell) CloudCellEvidence {
	t.Helper()
	for _, x := range e.Cells {
		if x.X == c.X && x.Y == c.Y {
			return x
		}
	}
	t.Fatalf("missing cell %+v", c)
	return CloudCellEvidence{}
}
func reviewBuild(t *testing.T, o CloudObservation, p Profile) (*Grid, CloudEvidence) {
	t.Helper()
	g, e, err := BuildCloud(o, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	return g, e
}

func TestCloudReviewPartialRingsCannotPoolAcrossWrap(t *testing.T) {
	o, p := reviewCloud()
	c := cell{10, 0} // Its uncertainty-expanded arc crosses projected bearing zero.
	for _, ring := range []int{11, 13} {
		reviewRing(&o, ring, 6)
	}
	_, e := reviewBuild(t, o, p)
	if reviewCell(t, e, c).WitnessRings != (1<<11 | 1<<13) {
		t.Fatal("fixture requires two independent complete witnesses")
	}
	// Each ring supplies a different half of the cell's angular interval.
	// Together they cover all directions, but neither proves the whole cell.
	for az := 0; az < 360; az++ {
		if az >= 180 {
			reviewRay(&o, 11, az, math.Inf(1))
		}
		if az < 180 {
			reviewRay(&o, 13, az, math.Inf(1))
		}
	}
	g, e := reviewBuild(t, o, p)
	if x := reviewCell(t, e, c); x.WitnessRings != 0 || x.State != "unknown" || g.cells[c] == free {
		t.Fatalf("partial rings pooled: %+v", x)
	}
}

func TestCloudReviewAmbiguousEndpointsNeedWholeOtherRing(t *testing.T) {
	for _, tt := range []struct {
		name string
		ring int
		r    float64
	}{
		{"floor", 0, .8}, {"above_minimum_column_height", 24, 1.1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o, p := reviewCloud()
			reviewRing(&o, 12, 6)
			reviewRay(&o, tt.ring, 180, tt.r)
			g, e := reviewBuild(t, o, p)
			ray := e.Rays[tt.ring*360+180]
			if ray.Class != "ambiguous" {
				t.Fatalf("incorrect endpoint class: %+v", ray)
			}
			c := at(ray.EndpointXY)
			x := reviewCell(t, e, c)
			if x.AmbiguousEndpointRings != 1<<tt.ring || x.ResolvedAmbiguousRings != 1<<tt.ring || x.WitnessRings != 1<<12 || g.cells[c] != free {
				t.Fatalf("complete other-ring witness missing: %+v", x)
			}
			// Original timestamps, height and uncertainty remain the same. Losing
			// the horizontal bracketing ray must make this very cell unresolved.
			reviewRay(&o, 12, 180, math.Inf(1))
			g, e = reviewBuild(t, o, p)
			x = reviewCell(t, e, c)
			if x.ResolvedAmbiguousRings != 0 || x.WitnessRings != 0 || x.State != "unknown" || g.cells[c] == free {
				t.Fatalf("missing bracket resolved ambiguity: %+v", x)
			}
		})
	}
}

func TestCloudReviewDefiniteEndpointVetoIncludesOutsideNominal(t *testing.T) {
	for _, r := range []float64{1, 2.73} {
		o, p := reviewCloud()
		reviewRing(&o, 11, 6)
		reviewRay(&o, 12, 180, r)
		g, e := reviewBuild(t, o, p)
		ray := e.Rays[12*360+180]
		c := at(ray.EndpointXY)
		if r > 2 {
			c = cell{29, 0}
			if area().contains(ray.EndpointXY) {
				t.Fatal("fixture endpoint must be outside area")
			}
		}
		x := reviewCell(t, e, c)
		if ray.Class != "definite_interior" || x.DefiniteEndpointRings != 1<<12 || x.WitnessRings&(1<<11) == 0 || g.cells[c] != occupied || x.Reason != "definite_endpoint_conflict" {
			t.Fatalf("lost definite footprint/conflict: ray=%+v cell=%+v", ray, x)
		}
	}
}

func TestCloudReviewEndpointDiskIncludesGrazingCorners(t *testing.T) {
	c := cell{0, 0}
	// The disc's centre is outside both axes of the cell. A centre-cell or
	// axis-only intersection test would miss the diagonal corner contact.
	p := Point{.16, .18}
	r := math.Hypot(.06, .08)
	if !cloudDiskTouchesCell(p, r, c) {
		t.Fatal("grazing endpoint footprint lost")
	}
	if cloudDiskTouchesCell(p, r-1e-9, c) {
		t.Fatal("strictly separated disk intersects")
	}
}

func TestCloudReviewSharedUncertaintyDoesNotAverageAcrossRings(t *testing.T) {
	o, p := reviewCloud()
	for ring := 0; ring < 25; ring++ {
		reviewRing(&o, ring, 6)
	}
	_, low := reviewBuild(t, o, p)
	o.Scan.PositionUncertaintyM = .12
	o.Scan.YawUncertaintyRad = .09
	_, high := reviewBuild(t, o, p)
	if high.OriginErrorM <= low.OriginErrorM || high.AttitudeErrorRad <= low.AttitudeErrorRad {
		t.Fatal("shared uncertainty was reduced")
	}
	for i, r := range high.Rays {
		if r.SupportM > low.Rays[i].SupportM+1e-12 || r.EndpointRadiusM < low.Rays[i].EndpointRadiusM {
			t.Fatalf("uncertainty increased support or shrank endpoint at ray %d", i)
		}
	}
	for i, x := range high.Cells {
		if x.WitnessRings & ^low.Cells[i].WitnessRings != 0 {
			t.Fatalf("averaged ring uncertainty created witness: %+v", x)
		}
	}
}

func TestCloudReviewLegacyBuildAndCopiesRemainIndependent(t *testing.T) {
	o, p := reviewCloud()
	reviewRing(&o, 12, 6)
	before, err := Build(o.Scan, area(), p)
	if err != nil {
		t.Fatal(err)
	}
	g, e := reviewBuild(t, o, p)
	after, err := Build(o.Scan, area(), p)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("cloud build changed legacy grid: %v", err)
	}
	if g.sequence != before.sequence || g.epoch != before.epoch || g.captured != before.captured {
		t.Fatal("cloud build refreshed original identity/time")
	}
	oldCells := make(map[cell]occupancy, len(g.cells))
	for c, v := range g.cells {
		oldCells[c] = v
	}
	o.Ranges[0], o.Scan.Ranges[0] = .1, .1
	o.Scan.TiltedPose.OdomPositionXYZ[0] = 99
	p.TiltedScan.BaseHeightMinM = 99
	for i := range e.Cells {
		e.Cells[i].State = "free"
		e.Cells[i].WitnessRings = ^uint32(0)
	}
	for i := range e.Rays {
		e.Rays[i].SupportM = 999
	}
	if !reflect.DeepEqual(oldCells, g.cells) {
		t.Fatal("external mutable evidence changed grid")
	}
	// Existing planar-only callers keep their original behavior and do not
	// accidentally opt into the cloud endpoint interpretation.
	s := scan()
	pp := profile()
	legacy, err := Build(s, area(), pp)
	if err != nil || legacy.cells[at(Point{1, 0})] != free {
		t.Fatalf("legacy profile baseline: %v", err)
	}
	if _, _, err := BuildCloud(CloudObservation{Scan: s}, area(), pp); !errors.Is(err, ErrInvalid) {
		t.Fatalf("implicit legacy opt-in: %v", err)
	}
}

func TestCloudReviewFullRingEndpointAndPrefixContainPerturbations(t *testing.T) {
	o, p := reviewCloud()
	o.Scan.Robot.Yaw = .9
	o.Scan.TiltedPose.QuaternionWXYZ = tiltQuaternion(-.11, .12, .9)
	for ring := 0; ring < 25; ring++ {
		reviewRing(&o, ring, 2)
	}
	_, e := reviewBuild(t, o, p)
	// Independent Euler composition, followed by an adversarial world-axis
	// Rodrigues rotation. The same rotation perturbs both mount and direction.
	euler := func(v [3]float64) [3]float64 {
		r, pp, y := -.11, .12, .9
		a := [3]float64{v[0], math.Cos(r)*v[1] - math.Sin(r)*v[2], math.Sin(r)*v[1] + math.Cos(r)*v[2]}
		b := [3]float64{math.Cos(pp)*a[0] + math.Sin(pp)*a[2], a[1], -math.Sin(pp)*a[0] + math.Cos(pp)*a[2]}
		return [3]float64{math.Cos(y)*b[0] - math.Sin(y)*b[1], math.Sin(y)*b[0] + math.Cos(y)*b[1], b[2]}
	}
	perturb := func(v, axis [3]float64, angle float64) [3]float64 {
		c, s := math.Cos(angle), math.Sin(angle)
		dot := axis[0]*v[0] + axis[1]*v[1] + axis[2]*v[2]
		cross := [3]float64{axis[1]*v[2] - axis[2]*v[1], axis[2]*v[0] - axis[0]*v[2], axis[0]*v[1] - axis[1]*v[0]}
		return [3]float64{v[0]*c + cross[0]*s + axis[0]*dot*(1-c), v[1]*c + cross[1]*s + axis[1]*dot*(1-c), v[2]*c + cross[2]*s + axis[2]*dot*(1-c)}
	}
	axes := [][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {1 / math.Sqrt(3), 1 / math.Sqrt(3), 1 / math.Sqrt(3)}}
	delta := 2 * math.Sin(e.AttitudeErrorRad/2)
	checked := 0
	for ring := 0; ring < 25; ring++ {
		el := (-30 + 2.5*float64(ring)) * math.Pi / 180
		for az := 0; az < 360; az += 45 {
			a := (-180 + float64(az)) * math.Pi / 180
			nominal := euler([3]float64{math.Cos(el) * math.Cos(a), math.Cos(el) * math.Sin(a), math.Sin(el)})
			ray := e.Rays[ring*360+az]
			cap := ray.SupportM / (math.Hypot(nominal[0], nominal[1]) - delta)
			for _, axis := range axes {
				for _, sign := range []float64{-1, 1} {
					u := perturb(nominal, axis, sign*e.AttitudeErrorRad)
					m := perturb(euler([3]float64{.28, 0, .08}), axis, sign*e.AttitudeErrorRad)
					for _, h := range []float64{o.Scan.TiltedPose.BaseHeightMinM, o.Scan.TiltedPose.BaseHeightMaxM} {
						for _, r := range []float64{2 - p.TiltedScan.RangeErrorM - CloudRangeQuantizationM, 2 + p.TiltedScan.RangeErrorM + CloudRangeQuantizationM} {
							// A positional error aimed away from the nominal endpoint
							// exercises the full shared-origin radius too.
							x, y := m[0]+r*u[0], m[1]+r*u[1]
							dx, dy := x-ray.EndpointXY.X, y-ray.EndpointXY.Y
							if d := math.Hypot(dx, dy); d > 0 {
								x += o.Scan.PositionUncertaintyM * dx / d
								y += o.Scan.PositionUncertaintyM * dy / d
							}
							z := h + m[2] + r*u[2]
							if math.Hypot(x-ray.EndpointXY.X, y-ray.EndpointXY.Y) > ray.EndpointRadiusM+1e-12 || z < ray.EndZMinM-1e-12 || z > ray.EndZMaxM+1e-12 {
								t.Fatalf("endpoint not enclosed ring=%d az=%d", ring, az)
							}
							checked++
						}
						if cap > 0 {
							for _, fraction := range []float64{0, .25, .5, .75, 1} {
								z := h + m[2] + fraction*cap*u[2]
								if z < p.TiltedScan.SlabMarginM-1e-12 || z > p.TiltedScan.ObstacleMinHeightM-p.TiltedScan.SlabMarginM+1e-12 {
									t.Fatalf("prefix leaves slab ring=%d az=%d", ring, az)
								}
							}
						}
					}
				}
			}
		}
	}
	if checked != 6400 {
		t.Fatalf("lost adversarial coverage %d", checked)
	}
}
