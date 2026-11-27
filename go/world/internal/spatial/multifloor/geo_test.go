package multifloor

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// A hand-built site: the site origin is at 48°N 11°E and site +X points 20°
// south of east.
var handGeo = spatial.GeoReference{
	Origin:  world.GeographicAnchor{LatitudeDegrees: 48, LongitudeDegrees: 11, AltitudeMetres: 500, AltitudeDatum: geography.WGS84Ellipsoid},
	SiteYaw: 20 * math.Pi / 180, Method: "truth",
}

// fixAt is the GNSS fix a receiver at a site position reports, with the
// rotation written out so that the test does not lean on Pose2 or
// GeoReference.SiteToTangent for the handedness it checks.
func fixAt(t *testing.T, dev spatial.DeviceID, when time.Time, site geom.Vec3, errEast, errSouth float64) spatial.GnssFix {
	t.Helper()
	tangent, err := geography.NewTangent(handGeo.Origin)
	if err != nil {
		t.Fatal(err)
	}
	sin, cos := math.Sincos(handGeo.SiteYaw)
	c, err := tangent.ToGeographic([3]float64{site.X*cos - site.Z*sin + errEast, site.Y, site.X*sin + site.Z*cos + errSouth})
	if err != nil {
		t.Fatal(err)
	}
	return spatial.GnssFix{
		Header:          spatial.Header{Device: dev, Sensor: "gnss", Time: when},
		LatitudeDegrees: c.LatitudeDegrees, LongitudeDegrees: c.LongitudeDegrees, AltitudeMetres: c.AltitudeMetres,
		HorizontalAccuracy: 1.5, VerticalAccuracy: 2.5,
	}
}

// yardWalk is n positions along a 10 x 14 m rectangle 40 m from the origin.
func yardWalk(n int) []geom.Vec3 {
	corners := []geom.Vec2{{X: 32, Z: 4}, {X: 42, Z: 4}, {X: 42, Z: 18}, {X: 32, Z: 18}}
	out := make([]geom.Vec3, n)
	for i := range out {
		u := float64(i) / float64(n) * 4
		k := int(u)
		out[i] = corners[k%4].Lerp(corners[(k+1)%4], u-float64(k)).Lift(0)
	}
	return out
}

func noisySamples(t *testing.T, seed uint64, n int, sigma float64, outlierEvery int) []GeoSample {
	rng := rand.New(rand.NewPCG(seed, 20))
	var out []GeoSample
	for i, p := range yardWalk(n) {
		e, s := sigma*rng.NormFloat64(), sigma*rng.NormFloat64()
		if outlierEvery > 0 && i%outlierEvery == 0 {
			e, s = e+40, s-25 // multipath off the building
		}
		out = append(out, GeoSample{Site: p, Fix: fixAt(t, "rover", at(float64(i)), p, e, s)})
	}
	return out
}

// geoError is the distance in metres between where two geo references place
// the same site point on the Earth.
func geoError(t *testing.T, est, truth spatial.GeoReference, p geom.Vec2) float64 {
	t.Helper()
	te, err := geography.NewTangent(est.Origin)
	if err != nil {
		t.Fatal(err)
	}
	tt, err := geography.NewTangent(truth.Origin)
	if err != nil {
		t.Fatal(err)
	}
	local := est.SiteToTangent().Apply(p)
	c, err := te.ToGeographic([3]float64{local.X, 0, local.Z})
	if err != nil {
		t.Fatal(err)
	}
	c.AltitudeMetres = truth.Origin.AltitudeMetres
	got, err := tt.ToLocal(c)
	if err != nil {
		t.Fatal(err)
	}
	return geom.Vec2{X: got[0], Z: got[2]}.Dist(truth.SiteToTangent().Apply(p))
}

func TestGeoreferenceExactAndHandedness(t *testing.T) {
	fit, err := Georeference(noisySamples(t, 1, 60, 0, 0), GeoConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ref := fit.Reference
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	// The heading is quoted at Origin, 38 m east of the site origin where
	// handGeo quotes it: the meridians converge by 5e-6 rad in between.
	if d := geom.AngleDiff(ref.SiteYaw, handGeo.SiteYaw); math.Abs(d) > 1e-5 || fit.RMSE > 1e-5 || fit.Inliers != 60 || ref.Fixes != 60 {
		t.Fatalf("yaw error %g rad, rmse %g, inliers %d", d, fit.RMSE, fit.Inliers)
	}
	for _, p := range []geom.Vec2{{}, {X: 37, Z: 11}, {X: -50, Z: 80}} {
		if e := geoError(t, ref, handGeo, p); e > 1e-4 {
			t.Errorf("site %v placed %g m off", p, e)
		}
	}
	// Convention, checked against arithmetic done by hand: 100 m along site
	// +X is 93.97 m east and 34.20 m south, so the latitude falls and the
	// longitude grows. At 48°N a degree is 111190.3 m north and 74625.4 m east.
	tangent, err := geography.NewTangent(ref.Origin)
	if err != nil {
		t.Fatal(err)
	}
	coord := func(p geom.Vec2) geography.Coordinate {
		l := ref.SiteToTangent().Apply(p)
		c, err := tangent.ToGeographic([3]float64{l.X, 0, l.Z})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := coord(geom.Vec2{}), coord(geom.Vec2{X: 100})
	north, east := (b.LatitudeDegrees-a.LatitudeDegrees)*111190.3, (b.LongitudeDegrees-a.LongitudeDegrees)*74625.4
	if math.Abs(north+34.20) > 0.1 || math.Abs(east-93.97) > 0.1 {
		t.Fatalf("100 m along site +X is %.2f m north, %.2f m east; want -34.20, 93.97", north, east)
	}
	if math.Abs(a.LatitudeDegrees-48) > 1e-8 || math.Abs(a.LongitudeDegrees-11) > 1e-8 || math.Abs(ref.Origin.AltitudeMetres-500) > 1e-3 {
		t.Fatalf("site origin at %v", a)
	}
	if got := fit.Inputs; len(got) != 2 || got[0] != spatial.InputID(fixAt(t, "rover", at(0), geom.Vec3{}, 0, 0)) {
		t.Fatalf("inputs = %v", got)
	}
}

func TestGeoreferenceNoiseAndOutliers(t *testing.T) {
	centre := geom.Vec2{X: 37, Z: 11}
	var ratio2 float64
	const seeds = 30
	for seed := uint64(1); seed <= seeds; seed++ {
		samples := noisySamples(t, seed, 300, 1.5, 10)
		fit, err := Georeference(samples, GeoConfig{})
		if err != nil {
			t.Fatal(err)
		}
		ref := fit.Reference
		yawErr := geom.AngleDiff(ref.SiteYaw, handGeo.SiteYaw)
		if e := geoError(t, ref, handGeo, centre); e > 3*ref.UncertaintyM || e > 1 {
			t.Errorf("seed %d: yard centre %0.2f m off (reported %.2f)", seed, e, ref.UncertaintyM)
		}
		if math.Abs(yawErr) > 4*ref.YawSigmaRad {
			t.Errorf("seed %d: yaw error %.4f rad, reported sigma %.4f", seed, yawErr, ref.YawSigmaRad)
		}
		if fit.Inliers > 275 || fit.Inliers < 200 {
			t.Errorf("seed %d: %d inliers of 300 with 30 outliers", seed, fit.Inliers)
		}
		ratio2 += yawErr * yawErr / (ref.YawSigmaRad * ref.YawSigmaRad) / seeds

		// The same fixes without the robust weights are pulled metres away.
		src, dst := make([]geom.Vec2, len(samples)), make([]geom.Vec2, len(samples))
		tangent, _ := geography.NewTangent(handGeo.Origin)
		for i, s := range samples {
			l, _ := tangent.ToLocal(coordinate(s.Fix))
			src[i], dst[i] = s.Site.XZ(), geom.Vec2{X: l[0], Z: l[2]}
		}
		plain, _, _ := geom.AlignPoints2(src, dst, nil)
		if naive := plain.Apply(centre).Dist(handGeo.SiteToTangent().Apply(centre)); naive < 3 {
			t.Fatalf("seed %d: the outliers moved a plain fit by only %.2f m; the case proves nothing", seed, naive)
		}
	}
	// The reported heading sigma is honest: the errors are about one sigma.
	if r := math.Sqrt(ratio2); r < 0.5 || r > 1.6 {
		t.Errorf("yaw error is %.2f reported sigmas RMS over %d seeds", r, seeds)
	}
}

func TestGeoreferenceRefuses(t *testing.T) {
	few := noisySamples(t, 1, 300, 1.5, 0)[:6]
	if _, err := Georeference(few, GeoConfig{}); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("6 fixes: %v", err)
	}
	// A receiver that stands still, and one that walks two metres.
	rng := rand.New(rand.NewPCG(7, 20))
	var still, short []GeoSample
	for i := 0; i < 200; i++ {
		p := geom.Vec3{X: 35, Z: 10}
		still = append(still, GeoSample{Site: p, Fix: fixAt(t, "rover", at(float64(i)), p, 1.5*rng.NormFloat64(), 1.5*rng.NormFloat64())})
		q := geom.Vec3{X: 35 + 0.01*float64(i), Z: 10}
		short = append(short, GeoSample{Site: q, Fix: fixAt(t, "rover", at(float64(i)), q, 1.5*rng.NormFloat64(), 1.5*rng.NormFloat64())})
	}
	for name, s := range map[string][]GeoSample{"still": still, "two metres": short} {
		if _, err := Georeference(s, GeoConfig{}); !errors.Is(err, ErrBaseline) {
			t.Errorf("%s: %v, want ErrBaseline", name, err)
		}
	}
	// Mostly outliers: too few fixes agree.
	if _, err := Georeference(noisySamples(t, 2, 12, 0.1, 2), GeoConfig{}); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("half outliers of 12: %v", err)
	}
	// A long walk with few, exact fixes is still held to the reported
	// accuracy: agreement by luck is not precision.
	if _, err := Georeference(noisySamples(t, 3, 12, 0, 0), GeoConfig{}); !errors.Is(err, ErrBaseline) {
		t.Errorf("12 exact fixes over 7 m: %v, want ErrBaseline", err)
	}
	fit, err := Georeference(noisySamples(t, 3, 24, 0, 0), GeoConfig{})
	if err != nil || fit.Reference.YawSigmaRad < 0.02 {
		t.Errorf("24 exact fixes: yaw sigma %.4f, err %v", fit.Reference.YawSigmaRad, err)
	}
}
