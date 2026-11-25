package go2nav

import (
	"errors"
	"math"
	"testing"
)

func tiltQuaternion(r, p, y float64) [4]float64 {
	cr, sr := math.Cos(r/2), math.Sin(r/2)
	cp, sp := math.Cos(p/2), math.Sin(p/2)
	cy, sy := math.Cos(y/2), math.Sin(y/2)
	return [4]float64{cr*cp*cy + sr*sp*sy, sr*cp*cy - cr*sp*sy, cr*sp*cy + sr*cp*sy, cr*cp*sy - sr*sp*cy}
}
func tiltFixture() (Scan, Profile) {
	s, p := scan(), profile()
	p.MaxAbsRollRad = .15
	p.MaxAbsPitchRad = .15
	p.TiltedScan = &TiltedScanProfile{Model: "solid-vertical-slab-v1", BaseHeightMinM: .15, BaseHeightMaxM: .6, ObstacleMinHeightM: .8, SlabMarginM: .02, BodyHalfHeightM: .35, MaxSupportRangeM: 8, RangeErrorM: .02, AttitudeErrorRad: .001, MaxTranslationMPerWallS: 1, MaxRotationRadPerWallS: 2, OdomFloorZM: 0, HeightCalibrationErrorM: .005}
	s.TiltedPose = &TiltedPose{QuaternionWXYZ: [4]float64{1, 0, 0, 0}, OdomPositionXYZ: [3]float64{0, 0, .33}, MountTranslationXYZ: [3]float64{.28, 0, .08}, MountQuaternionWXYZ: [4]float64{1, 0, 0, 0}, OdomCapturedUnixNS: s.CapturedUnixNS, BaseHeightMinM: .275, BaseHeightMaxM: .385}
	return s, p
}
func TestTiltedProfilesAndEvidenceFailClosed(t *testing.T) {
	tests := map[string]func(*Scan, *Profile){
		"no pose":       func(s *Scan, p *Profile) { s.TiltedPose = nil },
		"no model":      func(s *Scan, p *Profile) { p.TiltedScan = nil },
		"unknown model": func(s *Scan, p *Profile) { p.TiltedScan.Model = "leveled" },
		"unknown floor": func(s *Scan, p *Profile) { p.TiltedScan.OdomFloorZM = math.NaN() },
		"departed height": func(s *Scan, p *Profile) {
			s.TiltedPose.OdomPositionXYZ[2] = .7
			s.TiltedPose.BaseHeightMinM = .645
			s.TiltedPose.BaseHeightMaxM = .755
		},
		"fabricated height":       func(s *Scan, p *Profile) { s.TiltedPose.BaseHeightMinM = .28 },
		"unbounded quaternion":    func(s *Scan, p *Profile) { s.TiltedPose.QuaternionWXYZ[0] = 2 },
		"changed mount":           func(s *Scan, p *Profile) { s.TiltedPose.MountTranslationXYZ[2] = 0 },
		"wrong yaw":               func(s *Scan, p *Profile) { s.TiltedPose.QuaternionWXYZ = tiltQuaternion(0, 0, .1) },
		"tilt beyond model":       func(s *Scan, p *Profile) { s.TiltedPose.QuaternionWXYZ = tiltQuaternion(.151, 0, 0) },
		"missing temporal budget": func(s *Scan, p *Profile) { s.TiltedPose.OdomCapturedUnixNS -= 10000000 },
		"excessive skew":          func(s *Scan, p *Profile) { s.TiltedPose.OdomCapturedUnixNS -= 51000000 },
		"short obstacle class":    func(s *Scan, p *Profile) { p.TiltedScan.ObstacleMinHeightM = .4 },
	}
	for n, f := range tests {
		t.Run(n, func(t *testing.T) {
			s, p := tiltFixture()
			f(&s, &p)
			if _, e := Build(s, area(), p); !errors.Is(e, ErrInvalid) {
				t.Fatalf("got %v", e)
			}
		})
	}
}
func TestTiltedFullRotationMountAndProjectedAngles(t *testing.T) {
	for _, angles := range [][3]float64{{.12, 0, 0}, {-.12, 0, 0}, {0, .12, .4}, {0, -.12, -.4}, {.1, -.11, 1.2}} {
		s, p := tiltFixture()
		s.Robot.Yaw = angles[2]
		s.TiltedPose.QuaternionWXYZ = tiltQuaternion(angles[0], angles[1], angles[2])
		s.Robot.X = .2
		s.Robot.Y = -.1
		s.TiltedPose.OdomPositionXYZ[0] = .2
		s.TiltedPose.OdomPositionXYZ[1] = -.1
		out, e := projectTilted(s, p)
		if e != nil {
			t.Fatal(e)
		}
		// Independent Euler composition applies pitch/roll before yaw.
		rotate := func(v [3]float64) [3]float64 {
			r, pp, y := angles[0], angles[1], angles[2]
			a := [3]float64{v[0], math.Cos(r)*v[1] - math.Sin(r)*v[2], math.Sin(r)*v[1] + math.Cos(r)*v[2]}
			b := [3]float64{math.Cos(pp)*a[0] + math.Sin(pp)*a[2], a[1], -math.Sin(pp)*a[0] + math.Cos(pp)*a[2]}
			return [3]float64{math.Cos(y)*b[0] - math.Sin(y)*b[1], math.Sin(y)*b[0] + math.Cos(y)*b[1], b[2]}
		}
		m := rotate([3]float64{.28, 0, .08})
		if distance(out.origin, Point{.2 + m[0], -.1 + m[1]}) > 1e-12 {
			t.Fatal("full mount rotation lost")
		}
		for i := range s.Ranges {
			a := s.AngleMin + float64(i)*s.AngleIncrement
			u := rotate([3]float64{math.Cos(a), math.Sin(a), 0})
			end := Point{out.origin.X + 6*u[0], out.origin.Y + 6*u[1]}
			found := false
			for _, r := range out.rays {
				if distance(end, r.end) < 1e-11 {
					found = true
					break
				}
			}
			if !found {
				t.Fatal("actual ray transformed incorrectly")
			}
		}
		if angles[0] != 0 && angles[1] != 0 {
			d := out.rays[1].angle - out.rays[0].angle
			vary := false
			for i := 2; i < len(out.rays); i++ {
				if math.Abs(out.rays[i].angle-out.rays[i-1].angle-d) > 1e-6 {
					vary = true
					break
				}
			}
			if !vary {
				t.Fatal("expected nonuniform projected angles")
			}
		}
	}
}
func TestTiltedSlabTruncatesUpwardAndGroundRays(t *testing.T) {
	s, p := tiltFixture()
	s.TiltedPose.QuaternionWXYZ = tiltQuaternion(0, .14, 0)
	out, e := projectTilted(s, p)
	if e != nil {
		t.Fatal(e)
	}
	// Long distant returns must not assert clearance after the down-going ray
	// reaches the floor or the up-going ray passes above the obstacle slab.
	var forward, back projectedRay
	for _, r := range out.rays {
		if math.Abs(normalize(r.angle)) < 1e-9 {
			forward = r
		}
		if math.Abs(normalize(r.angle-math.Pi)) < 1e-9 {
			back = r
		}
	}
	if forward.support <= .5 || forward.support >= 3 || back.support <= .5 || back.support >= 3 {
		t.Fatalf("unsupported slab caps %g %g", forward.support, back.support)
	}
	g, e := Build(s, area(), p)
	if e != nil {
		t.Fatal(e)
	}
	if g.cells[at(Point{2.8, 0})] == free || g.cells[at(Point{-2.8, 0})] == free {
		t.Fatal("floor/above-short-obstacle ray cleared distant cell")
	}
	// A nearer obstacle return still truncates before the slab cap.
	s.Ranges[180] = .7
	g, e = Build(s, area(), p)
	if e != nil {
		t.Fatal(e)
	}
	if g.cells[at(Point{1.1, 0})] == free {
		t.Fatal("near obstacle cleared behind return")
	}
}
func TestTiltedUnknownRayAndCellBrackets(t *testing.T) {
	s, p := tiltFixture()
	g, e := Build(s, area(), p)
	if e != nil {
		t.Fatal(e)
	}
	c := at(Point{1, 0})
	if g.cells[c] != free {
		t.Fatal("baseline supported cell not clear")
	}
	s.Ranges[180] = math.Inf(1)
	g, e = Build(s, area(), p)
	if e != nil {
		t.Fatal(e)
	}
	if g.cells[c] == free {
		t.Fatal("missing ray fabricated free cell")
	}
	for i := range s.Ranges {
		s.Ranges[i] = math.Inf(1)
	}
	g, e = Build(s, area(), p)
	if e != nil {
		t.Fatal(e)
	}
	if len(g.cells) != 0 {
		t.Fatal("unknown returns created occupancy or clearance")
	}
}
func TestTiltedTemporalEnvelopeAndHeightRecompute(t *testing.T) {
	s, p := tiltFixture()
	initial, e := projectTilted(s, p)
	if e != nil {
		t.Fatal(e)
	}
	s.TiltedPose.OdomCapturedUnixNS -= 10000000
	s.TiltedPose.PairTranslationBoundM = .01
	s.TiltedPose.PairAttitudeBoundRad = .02
	s.TiltedPose.BaseHeightMinM -= .01
	s.TiltedPose.BaseHeightMaxM += .01
	delayed, e := projectTilted(s, p)
	if e != nil {
		t.Fatal(e)
	}
	if delayed.originError <= initial.originError || delayed.angleError <= initial.angleError {
		t.Fatal("pairing skew did not widen spatial uncertainty")
	}
	for i := range initial.rays {
		if delayed.rays[i].support > initial.rays[i].support+1e-12 {
			t.Fatal("pairing uncertainty extended ray support")
		}
	}
	s.TiltedPose.PairTranslationBoundM = 0
	if _, e = Build(s, area(), p); !errors.Is(e, ErrInvalid) {
		t.Fatal("omitted scan-time displacement admitted")
	}
}
func TestTiltedFootprintContainsRotatedPrismCorners(t *testing.T) {
	s, p := tiltFixture()
	g, e := Build(s, area(), p)
	if e != nil {
		t.Fatal(e)
	}
	for _, roll := range []float64{-.15, 0, .15} {
		for _, pitch := range []float64{-.15, 0, .15} {
			r, _ := quaternionRotation(tiltQuaternion(roll, pitch, 0))
			for _, x := range []float64{-p.HalfLengthM, p.HalfLengthM} {
				for _, y := range []float64{-p.HalfWidthM, p.HalfWidthM} {
					for _, z := range []float64{-p.TiltedScan.BodyHalfHeightM, p.TiltedScan.BodyHalfHeightM} {
						q := r.apply([3]float64{x, y, z})
						if math.Abs(q[0]) > g.halfLength || math.Abs(q[1]) > g.halfWidth {
							t.Fatal("tilted prism escapes horizontal footprint")
						}
					}
				}
			}
		}
	}
}

func TestTiltedSupportContainsAdversarialPosePerturbations(t *testing.T) {
	s, p := tiltFixture()
	s.TiltedPose.QuaternionWXYZ = tiltQuaternion(.11, -.12, 0)
	s.TiltedPose.OdomCapturedUnixNS -= 10000000
	s.TiltedPose.PairTranslationBoundM = .01
	s.TiltedPose.PairAttitudeBoundRad = .02
	s.TiltedPose.BaseHeightMinM -= .01
	s.TiltedPose.BaseHeightMaxM += .01
	out, e := projectTilted(s, p)
	if e != nil {
		t.Fatal(e)
	}
	rot, _ := quaternionRotation(s.TiltedPose.QuaternionWXYZ)
	delta := 2 * math.Sin(out.attitude/2)
	// Rodrigues is independent of the production quaternion rotation. Apply
	// arbitrary bounded world-frame orientation error to both ray and mount.
	perturb := func(v, axis [3]float64, a float64) [3]float64 {
		n := math.Sqrt(axis[0]*axis[0] + axis[1]*axis[1] + axis[2]*axis[2])
		for i := range axis {
			axis[i] /= n
		}
		dot := v[0]*axis[0] + v[1]*axis[1] + v[2]*axis[2]
		cross := [3]float64{axis[1]*v[2] - axis[2]*v[1], axis[2]*v[0] - axis[0]*v[2], axis[0]*v[1] - axis[1]*v[0]}
		var q [3]float64
		for i := range q {
			q[i] = v[i]*math.Cos(a) + cross[i]*math.Sin(a) + axis[i]*dot*(1-math.Cos(a))
		}
		return q
	}
	checks := 0
	for i := 0; i < len(s.Ranges); i += 10 {
		angle := s.AngleMin + float64(i)*s.AngleIncrement
		u := rot.apply([3]float64{math.Cos(angle), math.Sin(angle), 0})
		m := rot.apply([3]float64{.28, 0, .08})
		rho := math.Hypot(u[0], u[1])
		bearing := math.Atan2(u[1], u[0])
		if bearing < 0 {
			bearing += 2 * math.Pi
		}
		var support float64
		for _, r := range out.rays {
			if math.Abs(normalize(r.angle-bearing)) < 1e-12 {
				support = r.support
			}
		}
		cap := support / (rho - delta)
		for _, axis := range [][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {1, -1, 1}} {
			for _, sign := range []float64{-1, 1} {
				pu, pm := perturb(u, axis, sign*out.attitude), perturb(m, axis, sign*out.attitude)
				if math.Abs(normalize(math.Atan2(pu[1], pu[0])-bearing)) > out.angleError+1e-12 {
					t.Fatal("direction escapes angular support budget")
				}
				for _, height := range []float64{s.TiltedPose.BaseHeightMinM, s.TiltedPose.BaseHeightMaxM} {
					for _, f := range []float64{0, .25, .5, 1} {
						z := height + pm[2] + cap*f*pu[2]
						if z < p.TiltedScan.SlabMarginM-1e-10 || z > p.TiltedScan.ObstacleMinHeightM-p.TiltedScan.SlabMarginM+1e-10 {
							t.Fatalf("supported prefix escapes slab: %g", z)
						}
						checks++
					}
				}
				if cap*math.Hypot(pu[0], pu[1])+1e-10 < support {
					t.Fatal("projected radial support exceeds perturbed ray")
				}
			}
		}
	}
	if checks != 2304 {
		t.Fatalf("unexpected adversarial sample count %d", checks)
	}
}
