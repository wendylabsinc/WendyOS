package geography

import (
	"math"
	"math/rand"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func anchorAt(lat, lon, altitude float64) world.GeographicAnchor {
	return world.GeographicAnchor{LatitudeDegrees: lat, LongitudeDegrees: lon, AltitudeMetres: altitude, AltitudeDatum: WGS84Ellipsoid, UncertaintyMetres: 1}
}

// Independent fixtures generated with pyproj 3.6.1 / PROJ 9.3.0, not this
// implementation. Pipeline: +proj=pipeline +step +proj=cart +ellps=WGS84
// +step +proj=topocentric +ellps=WGS84 +lat_0=... +lon_0=... +h_0=...
// PROJ's east,north,up output is explicitly reordered to east,up,-north.
func TestPROJIndependentFixtures(t *testing.T) {
	cases := []struct {
		name               string
		anchor, coordinate [3]float64 // latitude, longitude, ellipsoid height
		local              [3]float64
	}{
		{"office", [3]float64{52, 5, 0}, [3]float64{52.000123, 5.000456, 7.8}, [3]float64{31.317127719031497, 7.799908583780026, -13.685999546415491}},
		{"dateline", [3]float64{10, 179.9999, 20}, [3]float64{10.0001, -179.9998, 22}, [3]float64{32.89191260572052, 1.9999055442149425, -11.060829893162266}},
		{"north-pole", [3]float64{90, 42, 0}, [3]float64{89.999, 43, 15}, [3]float64{1.9493332967936325, 14.999025284312673, 111.6772297784776}},
		{"south-pole", [3]float64{-90, -23, 20}, [3]float64{-89.998, -24, 100}, [3]float64{-3.8987183753069417, 79.99610108323397, -223.35742613007886}},
		{"equator", [3]float64{0, 0, 0}, [3]float64{.001, .002, 20}, [3]float64{222.63967963935173, 19.995149259455502, -110.57462488194369}},
		{"opposite", [3]float64{0, 0, 0}, [3]float64{0, 180, 0}, [3]float64{7.810965061573302e-10, -12756274, 0}},
		{"below-sea", [3]float64{-31.5, 115.9, -1000}, [3]float64{-31.5002, 115.8997, -900}, [3]float64{-28.496617859055824, 99.99989769113017, 22.172517277016013}},
		{"high", [3]float64{45, -120, 0}, [3]float64{45.1, -119.9, 100000}, [3]float64{7994.1500461941905, 99985.21657496359, -11292.735575690713}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projection, err := NewTangent(anchorAt(tc.anchor[0], tc.anchor[1], tc.anchor[2]))
			if err != nil {
				t.Fatal(err)
			}
			coordinate := Coordinate{tc.coordinate[0], tc.coordinate[1], tc.coordinate[2], WGS84Ellipsoid}
			got, err := projection.ToLocal(coordinate)
			if err != nil {
				t.Fatal(err)
			}
			for i := range got {
				if math.Abs(got[i]-tc.local[i]) > 1e-6 {
					t.Fatalf("axis %d: got %.12f want %.12f", i, got[i], tc.local[i])
				}
			}
			// Invert the external fixture, not the implementation's own output.
			back, err := projection.ToGeographic(tc.local)
			if err != nil {
				t.Fatal(err)
			}
			checkCoordinate(t, back, coordinate)
		})
	}
}

func checkCoordinate(t *testing.T, got, want Coordinate) {
	t.Helper()
	dlon := math.Remainder(got.LongitudeDegrees-want.LongitudeDegrees, 360)
	if math.Abs(got.LatitudeDegrees-want.LatitudeDegrees) > 1e-10 || (math.Abs(want.LatitudeDegrees) < 89.999999 && math.Abs(dlon) > 1e-10) || math.Abs(got.AltitudeMetres-want.AltitudeMetres) > 1e-6 || got.AltitudeDatum != want.AltitudeDatum {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestGlobalRoundTripsAndAltitudeBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(20261109))
	for i := 0; i < 2000; i++ {
		a := anchorAt(rng.Float64()*180-90, rng.Float64()*360-180, rng.Float64()*110000-10000)
		projection, err := NewTangent(a)
		if err != nil {
			t.Fatal(err)
		}
		c := Coordinate{rng.Float64()*180 - 90, rng.Float64()*360 - 180, rng.Float64()*110000 - 10000, WGS84Ellipsoid}
		if i%4 == 0 {
			c.AltitudeMetres = minAltitude
		}
		if i%4 == 1 {
			c.AltitudeMetres = maxAltitude
		}
		if i%7 == 0 {
			c.LatitudeDegrees = 90
		}
		if i%11 == 0 {
			c.LatitudeDegrees = -90
		}
		point, err := projection.ToLocal(c)
		if err != nil {
			t.Fatal(err)
		}
		back, err := projection.ToGeographic(point)
		if err != nil {
			t.Fatalf("sample %d, anchor %+v, coordinate %+v: %v", i, a, c, err)
		}
		checkCoordinate(t, back, c)
	}
}

func TestLocalAxesAreEastUpSouth(t *testing.T) {
	projection, err := NewTangent(anchorAt(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		point [3]float64
		check func(Coordinate) bool
	}{
		{[3]float64{1, 0, 0}, func(c Coordinate) bool { return c.LongitudeDegrees > 0 && c.LatitudeDegrees == 0 }},
		{[3]float64{0, 1, 0}, func(c Coordinate) bool {
			return math.Abs(c.AltitudeMetres-1) < 1e-8 && c.LongitudeDegrees == 0 && c.LatitudeDegrees == 0
		}},
		{[3]float64{0, 0, 1}, func(c Coordinate) bool { return c.LatitudeDegrees < 0 && c.LongitudeDegrees == 0 }},
	} {
		c, err := projection.ToGeographic(tc.point)
		if err != nil || !tc.check(c) {
			t.Fatalf("point %+v: %+v %v", tc.point, c, err)
		}
	}
}

func TestInvalidGeographicCoordinatesAndTangent(t *testing.T) {
	good := Coordinate{52, 5, 0, WGS84Ellipsoid}
	for _, change := range []func(*Coordinate){
		func(c *Coordinate) { c.LatitudeDegrees = 91 },
		func(c *Coordinate) { c.LongitudeDegrees = -181 },
		func(c *Coordinate) { c.AltitudeMetres = minAltitude - 1 },
		func(c *Coordinate) { c.AltitudeMetres = maxAltitude + 1 },
		func(c *Coordinate) { c.AltitudeDatum = "MSL" },
		func(c *Coordinate) { c.AltitudeDatum = "" },
		func(c *Coordinate) { c.LatitudeDegrees = math.NaN() },
		func(c *Coordinate) { c.LongitudeDegrees = math.Inf(1) },
		func(c *Coordinate) { c.AltitudeMetres = math.Inf(-1) },
	} {
		bad := good
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
		if _, err := NewTangent(world.GeographicAnchor{LatitudeDegrees: bad.LatitudeDegrees, LongitudeDegrees: bad.LongitudeDegrees, AltitudeMetres: bad.AltitudeMetres, AltitudeDatum: bad.AltitudeDatum}); err == nil {
			t.Fatalf("accepted anchor %+v", bad)
		}
	}
	badAnchor := anchorAt(0, 0, 0)
	badAnchor.UncertaintyMetres = -1
	if _, err := NewTangent(badAnchor); err == nil {
		t.Fatal("accepted negative anchor uncertainty")
	}
	projection, _ := NewTangent(anchorAt(0, 0, 0))
	for _, point := range [][3]float64{{math.NaN(), 0, 0}, {0, math.Inf(1), 0}, {1e308, 1e308, 1e308}, {0, -wgs84A, 0}, {0, maxAltitude + 1, 0}} {
		if _, err := projection.ToGeographic(point); err == nil {
			t.Fatalf("accepted %+v", point)
		}
	}
	for _, projection := range []*Tangent{nil, {}} {
		if _, err := projection.ToLocal(good); err == nil {
			t.Fatal("accepted invalid tangent")
		}
		if _, err := projection.ToGeographic([3]float64{}); err == nil {
			t.Fatal("accepted invalid tangent")
		}
	}
}
