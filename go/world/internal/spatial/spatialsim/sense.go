package spatialsim

import (
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// sensor resolves a device's sensor of one kind at a site instant. It
// returns nils when the device is unknown, lacks the sensor, or is not yet on.
func (s *Site) sensor(dev spatial.DeviceID, kind spatial.SensorKind, at time.Time) (*device, *sensor, time.Duration) {
	d := s.byID[dev]
	if d == nil {
		return nil, nil, 0
	}
	t := at.Sub(epoch)
	if t < d.start {
		return nil, nil, 0
	}
	if sn := d.sensorOf(kind); sn != nil {
		return d, sn, t
	}
	return nil, nil, 0
}

func (s *Site) header(d *device, sn *sensor, t time.Duration) spatial.Header {
	return spatial.Header{Device: d.id, Sensor: sn.id, Time: s.At(t + sn.clock)}
}

// caster accumulates the nearest hit per lidar beam. Each obstacle is tested
// only against the beams inside the arc it subtends from the sensor.
type caster struct {
	origin geom.Vec2
	yaw0   float64 // site yaw of beam 0
	inc    float64
	max    float64
	dirs   []geom.Vec2
	ranges []float64
}

// arc returns the first beam and the beam count that cover the site-frame
// angular interval [lo, lo+span], with one beam of margin on each side.
func (c *caster) arc(lo, span float64) (first, count int) {
	n := len(c.dirs)
	count = int(span/c.inc) + 3
	if count >= n {
		return 0, n
	}
	first = int(math.Mod(math.Floor((lo-c.yaw0)/c.inc)-1, float64(n)))
	if first < 0 {
		first += n
	}
	return first, count
}

func (c *caster) segment(sg geom.Segment) {
	dist := sg.DistanceTo(c.origin)
	if dist > c.max {
		return
	}
	first, count := 0, len(c.dirs)
	if dist > 1e-9 {
		a := sg.A.Sub(c.origin).Angle()
		span := geom.AngleDiff(sg.B.Sub(c.origin).Angle(), a)
		if span < 0 {
			a, span = a+span, -span
		}
		first, count = c.arc(a, span)
	}
	for k := 0; k < count; k++ {
		i := (first + k) % len(c.dirs)
		if t, ok := geom.RaySegment(c.origin, c.dirs[i], sg); ok && t < c.ranges[i] {
			c.ranges[i] = t
		}
	}
}

func (c *caster) circle(centre geom.Vec2, radius float64) {
	v := centre.Sub(c.origin)
	dist := v.Norm()
	if dist-radius > c.max {
		return
	}
	first, count := 0, len(c.dirs)
	if dist > radius {
		half := math.Asin(radius / dist)
		first, count = c.arc(v.Angle()-half, 2*half)
	}
	for k := 0; k < count; k++ {
		i := (first + k) % len(c.dirs)
		if t, ok := geom.RayCircle(c.origin, c.dirs[i], centre, radius); ok && t < c.ranges[i] {
			c.ranges[i] = t
		}
	}
}

// Scan is the lidar sweep of a robot at the instant: walls, closed doors,
// furniture, actors and other devices are raycast from the true sensor pose.
// Beam 0 points backwards (AngleMin = -π) and the sweep covers 360°.
func (s *Site) Scan(dev spatial.DeviceID, at time.Time) (spatial.Scan, bool) {
	d, sn, t := s.sensor(dev, spatial.KindLidar2D, at)
	if d == nil {
		return spatial.Scan{}, false
	}
	c := &s.cfg
	base, _, lv := s.place(d, t)
	pose := base.Compose(d.lidarAt(t))
	n := c.LidarBeams
	cs := caster{origin: pose.Pos(), yaw0: pose.Yaw - math.Pi, inc: 2 * math.Pi / float64(n), max: c.LidarRangeMax, dirs: make([]geom.Vec2, n), ranges: make([]float64, n)}
	for i := range cs.dirs {
		cs.dirs[i] = geom.Heading(cs.yaw0 + float64(i)*cs.inc)
		cs.ranges[i] = math.Inf(1)
	}
	sc := s.scene(lv, t)
	for _, w := range lv.walls {
		cs.segment(w)
	}
	for _, w := range sc.doors {
		cs.segment(w)
	}
	for i := range sc.solids {
		so := &sc.solids[i]
		switch {
		case so.id == string(d.id):
		case so.shape.Radius > 0:
			cs.circle(so.shape.Centre, so.shape.Radius)
		default:
			for k := 0; k < 4; k++ {
				cs.segment(geom.Segment{A: so.corners[k], B: so.corners[(k+1)%4]})
			}
		}
	}
	r := s.rng(d.id, "lidar", int64(t))
	for i, v := range cs.ranges {
		v += c.LidarSigma * r.NormFloat64()
		switch drop := r.Float64() < c.LidarDropout; {
		case drop, v < c.LidarRangeMin:
			v = spatial.RangeInvalid
		case v > c.LidarRangeMax:
			v = spatial.RangeNoReturn
		}
		cs.ranges[i] = v
	}
	return spatial.Scan{Header: s.header(d, sn, t), AngleMin: -math.Pi, AngleIncrement: cs.inc, RangeMin: c.LidarRangeMin, RangeMax: c.LidarRangeMax, Ranges: cs.ranges, RangeSigma: c.LidarSigma}, true
}

// odometry integrates the noisy motion of a device from its start to t. The
// pose at each odomStep is a pure function of the seed and the route, so the
// lazily grown cache does not make the result depend on call order.
func (s *Site) odometry(d *device, t time.Duration) geom.Pose2 {
	rel := t - d.start
	k := int(rel / odomStep)
	c := &s.cfg
	d.odomMu.Lock()
	for len(d.odom) <= k+1 {
		i := len(d.odom)
		if i == 0 {
			d.odom = append(d.odom, geom.Pose2{})
			continue
		}
		p0, _, _, _ := d.route.at((time.Duration(i-1) * odomStep).Seconds())
		p1, _, _, _ := d.route.at((time.Duration(i) * odomStep).Seconds())
		step := p0.Between(p1)
		dist, turn := step.Pos().Norm(), math.Abs(step.Yaw)
		r := s.rng(d.id, "odom", int64(i))
		white := c.OdomTransPerMetre * math.Sqrt(dist)
		step.Yaw += d.odomBias[1]*dist + d.odomBias[2]*step.Yaw +
			c.OdomYawPerMetre*math.Sqrt(dist)*r.NormFloat64() + c.OdomYawPerRadian*math.Sqrt(turn)*r.NormFloat64()
		step.X = step.X*(1+d.odomBias[0]) + white*r.NormFloat64()
		step.Z = step.Z*(1+d.odomBias[0]) + white*r.NormFloat64()
		d.odom = append(d.odom, d.odom[i-1].Compose(step))
	}
	a, b := d.odom[k], d.odom[k+1]
	d.odomMu.Unlock()
	return a.Interpolate(b, float64(rel-time.Duration(k)*odomStep)/float64(odomStep))
}

// Odometry is the device base in its own odometry frame, which is the
// identity at the instant the device starts. Elevation is not reported.
func (s *Site) Odometry(dev spatial.DeviceID, at time.Time) (spatial.Odometry, bool) {
	d, sn, t := s.sensor(dev, spatial.KindOdometry, at)
	if d == nil {
		return spatial.Odometry{}, false
	}
	c := &s.cfg
	return spatial.Odometry{
		Header: s.header(d, sn, t), Pose: s.odometry(d, t),
		Noise: spatial.OdometryNoise{TransPerMetre: c.OdomTransPerMetre, YawPerMetre: c.OdomYawPerMetre, YawPerRadian: c.OdomYawPerRadian},
	}, true
}

// joltSeconds is how long a bumped camera's IMU rings.
const joltSeconds = 0.5

// Imu is an inertial sample. A mobile device reads its true yaw rate; a fixed
// camera reads rest, and a decaying oscillation for joltSeconds after a bump.
func (s *Site) Imu(dev spatial.DeviceID, at time.Time) (spatial.Imu, bool) {
	d, sn, t := s.sensor(dev, spatial.KindImu, at)
	if d == nil {
		return spatial.Imu{}, false
	}
	c := &s.cfg
	r := s.rng(d.id, "imu", int64(t))
	m := spatial.Imu{
		Header:    s.header(d, sn, t),
		YawRate:   d.gyroBias + c.GyroSigma*r.NormFloat64(),
		Accel:     geom.Vec3{X: c.AccelSigma * r.NormFloat64(), Y: gravity + c.AccelSigma*r.NormFloat64(), Z: c.AccelSigma * r.NormFloat64()},
		GyroSigma: c.GyroSigma, AccelSigma: c.AccelSigma,
	}
	if d.route != nil {
		_, _, _, w := d.route.at((t - d.start).Seconds())
		m.YawRate += w
	}
	if tau := (t - d.camBump).Seconds(); d.camBump > 0 && tau >= 0 && tau < joltSeconds {
		env := math.Exp(-tau / 0.1)
		m.YawRate += c.JoltYawRate * env * math.Cos(2*math.Pi*10*tau)
		m.Accel = m.Accel.Add(geom.Vec3{X: math.Cos(2 * math.Pi * 12 * tau), Y: 0.5 * math.Sin(2*math.Pi*9*tau), Z: math.Sin(2 * math.Pi * 12 * tau)}.Scale(c.JoltAccel * env))
	}
	return m, true
}

// pressureAt is the standard-atmosphere pressure in pascals at an altitude.
func pressureAt(altitude float64) float64 {
	return 101325 * math.Pow(1-2.25577e-5*altitude, 5.25588)
}

// Baro is a barometer sample. Altitude follows the true elevation plus a
// slow sinusoidal drift and an arbitrary per-device zero.
func (s *Site) Baro(dev spatial.DeviceID, at time.Time) (spatial.Baro, bool) {
	d, sn, t := s.sensor(dev, spatial.KindBaro, at)
	if d == nil {
		return spatial.Baro{}, false
	}
	c := &s.cfg
	_, elevation, _ := s.place(d, t)
	h := elevation + c.BaroSigma*s.rng(d.id, "baro", int64(t)).NormFloat64()
	if c.BaroDriftPeriod > 0 {
		h += c.BaroDrift * math.Sin(2*math.Pi*t.Seconds()/c.BaroDriftPeriod.Seconds()+d.phase[0])
	}
	return spatial.Baro{Header: s.header(d, sn, t), PressurePa: pressureAt(s.geo.Origin.AltitudeMetres + h), Altitude: d.baroRef + h, AltitudeSigma: c.BaroSigma}, true
}

// Gnss is a satellite fix, available only while the device is on an outdoor
// floor. The error is white noise plus a bias that wanders over minutes.
func (s *Site) Gnss(dev spatial.DeviceID, at time.Time) (spatial.GnssFix, bool) {
	d, sn, t := s.sensor(dev, spatial.KindGnss, at)
	if d == nil {
		return spatial.GnssFix{}, false
	}
	c := &s.cfg
	base, elevation, lv := s.place(d, t)
	if !lv.floorAt(base.Pos()).outdoor {
		return spatial.GnssFix{}, false
	}
	r := s.rng(d.id, "gnss", int64(t))
	sec := t.Seconds()
	p := base.Pos().Add(geom.Vec2{
		X: c.GnssBias*math.Sin(2*math.Pi*sec/97+d.phase[1]) + c.GnssSigma*r.NormFloat64(),
		Z: c.GnssBias*math.Sin(2*math.Pi*sec/131+d.phase[2]) + c.GnssSigma*r.NormFloat64(),
	})
	local := s.geo.SiteToTangent().Apply(p)
	fix, err := s.tangent.ToGeographic([3]float64{local.X, elevation + 1.5*c.GnssSigma*r.NormFloat64(), local.Z})
	if err != nil {
		return spatial.GnssFix{}, false
	}
	acc := math.Max(math.Hypot(c.GnssSigma, c.GnssBias), 1e-3)
	return spatial.GnssFix{
		Header: s.header(d, sn, t), LatitudeDegrees: fix.LatitudeDegrees, LongitudeDegrees: fix.LongitudeDegrees, AltitudeMetres: fix.AltitudeMetres,
		HorizontalAccuracy: acc, VerticalAccuracy: 1.5 * acc,
	}, true
}

// Ranges are the UWB distances from a device's antenna to every beacon and
// every other antenna on its level within UwbRange, beacons first, each by id.
// The distance is three-dimensional, between antennas: Survey declares where
// each one is. A path blocked by a wall or a closed door gains a one-sided
// positive bias, is reported with the larger sigma and has LineOfSight false,
// as a radio's own channel diagnosis would.
func (s *Site) Ranges(dev spatial.DeviceID, at time.Time) []spatial.Range {
	d, sn, t := s.sensor(dev, spatial.KindUwb, at)
	if d == nil {
		return nil
	}
	c := &s.cfg
	tag, lv := s.tagAt(d, t)
	sc := s.barriers(lv, t)
	r := s.rng(d.id, "uwb", int64(t))
	var out []spatial.Range
	add := func(peer spatial.DeviceID, p geom.Vec3) {
		dist := tag.Dist(p)
		if dist > c.UwbRange {
			return
		}
		sigma := c.UwbSigma
		dist += sigma * r.NormFloat64()
		clear := !sc.walled(tag.XZ(), p.XZ())
		if !clear {
			sigma = math.Hypot(c.UwbSigma, c.UwbNlosSigma)
			dist += math.Abs(c.UwbNlosSigma * r.NormFloat64())
		}
		out = append(out, spatial.Range{Header: s.header(d, sn, t), Peer: peer, PeerSensor: SensorUwb, Distance: math.Max(dist, 0), Sigma: math.Max(sigma, 1e-3), LineOfSight: &clear})
	}
	for _, b := range s.beacons {
		if s.floor(b.Floor).level == lv {
			add(b.ID, b.Position)
		}
	}
	for _, o := range s.devices {
		if o == d || t < o.start || !o.has(spatial.KindUwb) {
			continue
		}
		if p, l := s.tagAt(o, t); l == lv {
			add(o.id, p)
		}
	}
	return out
}

// Bearings are the directions of arrival a microphone array reports: one per
// sound source that is active, on its level, within MicRange and not behind a
// wall or a closed door. Source carries the emitter's id.
func (s *Site) Bearings(dev spatial.DeviceID, at time.Time) []spatial.Bearing {
	d, sn, t := s.sensor(dev, spatial.KindMic, at)
	if d == nil {
		return nil
	}
	c := &s.cfg
	pose, _, lv := s.place(d, t)
	sc := s.barriers(lv, t)
	r := s.rng(d.id, "mic", int64(t))
	var out []spatial.Bearing
	for _, src := range s.sounds {
		v := src.Position.XZ().Sub(pose.Pos())
		if s.floor(src.Floor).level != lv || !src.ActiveAt(t) || v.Norm() > c.MicRange || sc.walled(pose.Pos(), src.Position.XZ()) {
			continue
		}
		az := geom.WrapAngle(v.Angle() - pose.Yaw + c.BearingSigma*r.NormFloat64())
		out = append(out, spatial.Bearing{Header: s.header(d, sn, t), Azimuth: az, Sigma: math.Max(c.BearingSigma, 1e-4), Source: src.ID})
	}
	return out
}

// concentration is the analytic gas field on a level, in ppm.
func (s *Site) concentration(lv *level, p geom.Vec2) float64 {
	v := GasBackgroundPPM
	for _, g := range s.gas {
		if s.floor(g.Floor).level == lv {
			v += g.PeakPPM * math.Exp(-p.Sub(g.Position.XZ()).Norm2()/(2*g.SpreadM*g.SpreadM))
		}
	}
	return v
}

// Gas is a concentration reading at the device's position.
func (s *Site) Gas(dev spatial.DeviceID, at time.Time) (spatial.Scalar, bool) {
	d, sn, t := s.sensor(dev, spatial.KindGas, at)
	if d == nil {
		return spatial.Scalar{}, false
	}
	base, _, lv := s.place(d, t)
	v := s.concentration(lv, base.Pos()) + s.cfg.GasSigma*s.rng(d.id, "gas", int64(t)).NormFloat64()
	return spatial.Scalar{Header: s.header(d, sn, t), Quantity: "co2", Value: v, Unit: "ppm", Sigma: s.cfg.GasSigma}, true
}
