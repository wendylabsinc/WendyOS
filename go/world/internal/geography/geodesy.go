// Package geography converts explicitly referenced site coordinates. It does
// not establish survey accuracy, permission to publish a frame, or navigation
// safety.
package geography

import (
	"errors"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const WGS84Ellipsoid = "WGS84-ellipsoid"

const (
	wgs84A           = 6378137.0
	wgs84F           = 1 / 298.257223563
	wgs84B           = wgs84A * (1 - wgs84F)
	wgs84E2          = wgs84F * (2 - wgs84F)
	degreesToRadians = math.Pi / 180
	// This implementation supports terrestrial sites and aircraft, not deep
	// Earth or orbital coordinates, where the inverse needs a wider contract.
	minAltitude = -10000.0
	maxAltitude = 100000.0
)

type Coordinate struct {
	LatitudeDegrees  float64 `json:"latitude_degrees"`
	LongitudeDegrees float64 `json:"longitude_degrees"`
	AltitudeMetres   float64 `json:"altitude_metres"`
	AltitudeDatum    string  `json:"altitude_datum"`
}

func geoFinite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func (c Coordinate) Validate() error {
	if !geoFinite(c.LatitudeDegrees, c.LongitudeDegrees, c.AltitudeMetres) || math.Abs(c.LatitudeDegrees) > 90 || math.Abs(c.LongitudeDegrees) > 180 || c.AltitudeMetres < minAltitude || c.AltitudeMetres > maxAltitude {
		return errors.New("geographic coordinate outside finite latitude, longitude or supported altitude bounds")
	}
	if c.AltitudeDatum != WGS84Ellipsoid {
		return errors.New("geographic conversion requires explicit WGS84-ellipsoid altitude")
	}
	return nil
}

// Tangent uses x=east, y=up, z=south in metres. Its private state is immutable,
// so one instance can be shared by concurrent readers. At a pole, the anchor's
// longitude determines the otherwise ambiguous east/south orientation.
type Tangent struct {
	origin [3]float64
	// Rows transform ECEF differences into local east, up, south.
	rotation [3][3]float64
	valid    bool
}

func NewTangent(anchor world.GeographicAnchor) (*Tangent, error) {
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	c := Coordinate{anchor.LatitudeDegrees, anchor.LongitudeDegrees, anchor.AltitudeMetres, anchor.AltitudeDatum}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	sinLat, cosLat := math.Sincos(c.LatitudeDegrees * degreesToRadians)
	sinLon, cosLon := math.Sincos(c.LongitudeDegrees * degreesToRadians)
	return &Tangent{origin: geographicECEF(c), rotation: [3][3]float64{
		{-sinLon, cosLon, 0},
		{cosLat * cosLon, cosLat * sinLon, sinLat},
		{sinLat * cosLon, sinLat * sinLon, -cosLat},
	}, valid: true}, nil
}

func geographicECEF(c Coordinate) [3]float64 {
	sinLat, cosLat := math.Sincos(c.LatitudeDegrees * degreesToRadians)
	sinLon, cosLon := math.Sincos(c.LongitudeDegrees * degreesToRadians)
	n := wgs84A / math.Sqrt(1-wgs84E2*sinLat*sinLat)
	return [3]float64{(n + c.AltitudeMetres) * cosLat * cosLon, (n + c.AltitudeMetres) * cosLat * sinLon, (n*(1-wgs84E2) + c.AltitudeMetres) * sinLat}
}

func (t *Tangent) ToLocal(c Coordinate) ([3]float64, error) {
	if t == nil || !t.valid {
		return [3]float64{}, errors.New("uninitialized geographic tangent")
	}
	if err := c.Validate(); err != nil {
		return [3]float64{}, err
	}
	ecef := geographicECEF(c)
	var out [3]float64
	for row := range out {
		for column := range ecef {
			out[row] += t.rotation[row][column] * (ecef[column] - t.origin[column])
		}
	}
	if !geoFinite(out[:]...) {
		return [3]float64{}, errors.New("nonfinite local coordinate")
	}
	return out, nil
}

func (t *Tangent) ToGeographic(local [3]float64) (Coordinate, error) {
	if t == nil || !t.valid {
		return Coordinate{}, errors.New("uninitialized geographic tangent")
	}
	if !geoFinite(local[:]...) {
		return Coordinate{}, errors.New("local coordinate must be finite")
	}
	ecef := t.origin
	for row := range ecef {
		for column := range local {
			ecef[row] += t.rotation[column][row] * local[column]
		}
	}
	return ecefGeographic(ecef)
}

func ecefGeographic(ecef [3]float64) (Coordinate, error) {
	if !geoFinite(ecef[:]...) {
		return Coordinate{}, errors.New("nonfinite geocentric coordinate")
	}
	p := math.Hypot(ecef[0], ecef[1])
	radius := math.Hypot(p, ecef[2])
	if radius < wgs84B+minAltitude-1e-6 || radius > wgs84A+maxAltitude+1e-6 {
		return Coordinate{}, errors.New("geocentric coordinate outside supported terrestrial shell")
	}
	// Fixed-point geodetic latitude from z = (N(1-e²)+h)sin(phi)
	// and p = (N+h)cos(phi). Near Earth's surface this converges rapidly;
	// a fixed iteration bound and residual check prevent silent bad inverses.
	latitude := math.Atan2(ecef[2], p*(1-wgs84E2))
	converged := false
	for iteration := 0; iteration < 16; iteration++ {
		sinLat := math.Sin(latitude)
		n := wgs84A / math.Sqrt(1-wgs84E2*sinLat*sinLat)
		next := math.Atan2(ecef[2]+wgs84E2*n*sinLat, p)
		if math.Abs(next-latitude) <= 1e-15 {
			latitude = next
			converged = true
			break
		}
		latitude = next
	}
	if !converged {
		return Coordinate{}, errors.New("geographic inverse did not converge")
	}
	sinLat, cosLat := math.Sincos(latitude)
	// Project along the ellipsoid normal. This avoids divisions by cos(phi)
	// at a pole or sin(phi) at the equator.
	altitude := p*cosLat + ecef[2]*sinLat - wgs84A*math.Sqrt(1-wgs84E2*sinLat*sinLat)
	// Permit only sub-micrometre arithmetic noise at the explicit altitude limits.
	if altitude < minAltitude && altitude >= minAltitude-1e-6 {
		altitude = minAltitude
	}
	if altitude > maxAltitude && altitude <= maxAltitude+1e-6 {
		altitude = maxAltitude
	}
	out := Coordinate{latitude / degreesToRadians, math.Atan2(ecef[1], ecef[0]) / degreesToRadians, altitude, WGS84Ellipsoid}
	if err := out.Validate(); err != nil {
		return Coordinate{}, err
	}
	reconstructed := geographicECEF(out)
	if math.Hypot(math.Hypot(reconstructed[0]-ecef[0], reconstructed[1]-ecef[1]), reconstructed[2]-ecef[2]) > 1e-6 {
		return Coordinate{}, errors.New("geographic inverse residual exceeds one micrometre")
	}
	return out, nil
}
