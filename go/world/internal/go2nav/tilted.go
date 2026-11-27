package go2nav

import (
	"math"
	"sort"
)

// TiltedScanProfile commissions a finite-resolution, synthetic obstacle model.
// Every relevant obstacle must be solid from the calibrated floor to at least
// ObstacleMinHeightM. It excludes holes, drop-offs, overhangs and short objects.
// Wall-time rate ceilings and the 3D .05m integration allowance are assumptions,
// not bounds derived from covariance or a measured velocity sample.
type TiltedScanProfile struct {
	Model                   string  `json:"model"`
	BaseHeightMinM          float64 `json:"base_height_min_m"`
	BaseHeightMaxM          float64 `json:"base_height_max_m"`
	ObstacleMinHeightM      float64 `json:"obstacle_min_height_m"`
	SlabMarginM             float64 `json:"slab_margin_m"`
	BodyHalfHeightM         float64 `json:"body_half_height_m"`
	MaxSupportRangeM        float64 `json:"max_support_range_m"`
	RangeErrorM             float64 `json:"range_error_m"`
	AttitudeErrorRad        float64 `json:"attitude_error_rad"`
	MaxTranslationMPerWallS float64 `json:"max_translation_m_per_wall_s"`
	MaxRotationRadPerWallS  float64 `json:"max_rotation_rad_per_wall_s"`
	OdomFloorZM             float64 `json:"odom_floor_z_m"`
	HeightCalibrationErrorM float64 `json:"height_calibration_error_m"`
}

// TiltedPose retains original paired odometry, not a leveled or truth-corrected
// pose. Height and pair bounds are repeated for audit and independently checked.
type TiltedPose struct {
	QuaternionWXYZ        [4]float64 `json:"quaternion_wxyz"`
	OdomPositionXYZ       [3]float64 `json:"odom_position_xyz"`
	MountTranslationXYZ   [3]float64 `json:"mount_translation_xyz"`
	MountQuaternionWXYZ   [4]float64 `json:"mount_quaternion_wxyz"`
	OdomCapturedUnixNS    int64      `json:"odom_captured_unix_ns"`
	PairTranslationBoundM float64    `json:"pair_translation_bound_m"`
	PairAttitudeBoundRad  float64    `json:"pair_attitude_bound_rad"`
	OdometryEpochAgeS     float64    `json:"odometry_epoch_age_s"`
	BaseHeightMinM        float64    `json:"base_height_min_m"`
	BaseHeightMaxM        float64    `json:"base_height_max_m"`
}

func (p TiltedScanProfile) Validate() error {
	for _, v := range []float64{p.BaseHeightMinM, p.BaseHeightMaxM, p.ObstacleMinHeightM, p.SlabMarginM, p.BodyHalfHeightM, p.MaxSupportRangeM, p.RangeErrorM, p.AttitudeErrorRad, p.MaxTranslationMPerWallS, p.MaxRotationRadPerWallS, p.OdomFloorZM, p.HeightCalibrationErrorM} {
		if !finite(v) {
			return ErrInvalid
		}
	}
	if p.Model != "solid-vertical-slab-v1" || p.BaseHeightMinM < .15 || p.BaseHeightMaxM > .6 || p.BaseHeightMinM >= p.BaseHeightMaxM || p.ObstacleMinHeightM < .5 || p.ObstacleMinHeightM > 3 || p.ObstacleMinHeightM <= p.BaseHeightMaxM+.08+p.SlabMarginM || p.SlabMarginM < .02 || p.SlabMarginM > .1 || p.BodyHalfHeightM < .35 || p.BodyHalfHeightM > .6 || p.MaxSupportRangeM < .5 || p.MaxSupportRangeM > 8 || p.RangeErrorM < .02 || p.RangeErrorM > .1 || p.AttitudeErrorRad < .001 || p.AttitudeErrorRad > .05 || p.MaxTranslationMPerWallS < .1 || p.MaxTranslationMPerWallS > 2 || p.MaxRotationRadPerWallS < .1 || p.MaxRotationRadPerWallS > 4 || math.Abs(p.OdomFloorZM) > 10 || p.HeightCalibrationErrorM < .001 || p.HeightCalibrationErrorM > .1 {
		return ErrInvalid
	}
	return nil
}

type rotation [3][3]float64

func quaternionRotation(q [4]float64) (rotation, error) {
	n := 0.0
	for _, v := range q {
		if !finite(v) {
			return rotation{}, ErrInvalid
		}
		n += v * v
	}
	if math.Abs(n-1) > 1e-9 {
		return rotation{}, ErrInvalid
	}
	// Divide by the norm to prevent the permitted floating-point roundoff from
	// turning an orthogonal transform into a range extension.
	inv := 1 / math.Sqrt(n)
	w, x, y, z := q[0]*inv, q[1]*inv, q[2]*inv, q[3]*inv
	return rotation{{1 - 2*(y*y+z*z), 2 * (x*y - z*w), 2 * (x*z + y*w)}, {2 * (x*y + z*w), 1 - 2*(x*x+z*z), 2 * (y*z - x*w)}, {2 * (x*z - y*w), 2 * (y*z + x*w), 1 - 2*(x*x+y*y)}}, nil
}
func (r rotation) apply(v [3]float64) [3]float64 {
	return [3]float64{r[0][0]*v[0] + r[0][1]*v[1] + r[0][2]*v[2], r[1][0]*v[0] + r[1][1]*v[1] + r[1][2]*v[2], r[2][0]*v[0] + r[2][1]*v[1] + r[2][2]*v[2]}
}
func QuaternionRPY(q [4]float64) (roll, pitch, yaw float64, err error) {
	r, e := quaternionRotation(q)
	if e != nil {
		return 0, 0, 0, e
	}
	return math.Atan2(r[2][1], r[2][2]), math.Asin(math.Max(-1, math.Min(1, -r[2][0]))), math.Atan2(r[1][0], r[0][0]), nil
}
func near(a, b float64) bool { return finite(a) && finite(b) && math.Abs(a-b) <= 1e-10 }

// ValidateTiltedPose rechecks the redundant timing/height evidence. Caller still
// checks authenticated run/source identity, current freshness and epoch age cap.
func ValidateTiltedPose(s Scan, p Profile) error {
	t, v := s.TiltedPose, p.TiltedScan
	if t == nil || v == nil || v.Validate() != nil || !finite(p.MaxAbsRollRad) || !finite(p.MaxAbsPitchRad) || p.MaxAbsRollRad <= 0 || p.MaxAbsRollRad > .25 || p.MaxAbsPitchRad <= 0 || p.MaxAbsPitchRad > .25 {
		return ErrInvalid
	}
	if t.MountTranslationXYZ != ([3]float64{.28, 0, .08}) || t.MountQuaternionWXYZ != ([4]float64{1, 0, 0, 0}) || s.SensorMount != (Pose{X: .28}) || t.OdomCapturedUnixNS <= 0 || s.CapturedUnixNS <= 0 || !finite(t.OdometryEpochAgeS) || t.OdometryEpochAgeS < 0 || t.OdometryEpochAgeS > 120 {
		return ErrInvalid
	}
	for _, n := range t.OdomPositionXYZ {
		if !finite(n) || math.Abs(n) > 100 {
			return ErrInvalid
		}
	}
	if !near(t.OdomPositionXYZ[0], s.Robot.X) || !near(t.OdomPositionXYZ[1], s.Robot.Y) {
		return ErrInvalid
	}
	r, pitch, yaw, e := QuaternionRPY(t.QuaternionWXYZ)
	if e != nil || math.Abs(r) > p.MaxAbsRollRad || math.Abs(pitch) > p.MaxAbsPitchRad || math.Abs(normalize(yaw-s.Robot.Yaw)) > 1e-10 {
		return ErrInvalid
	}
	// Both stamps are positive, so subtraction cannot overflow int64.
	skew := math.Abs(float64(s.CapturedUnixNS-t.OdomCapturedUnixNS)) / 1e9
	if skew > .05 || !near(t.PairTranslationBoundM, v.MaxTranslationMPerWallS*skew) || !near(t.PairAttitudeBoundRad, v.MaxRotationRadPerWallS*skew) {
		return ErrInvalid
	}
	if !finite(s.YawUncertaintyRad) || s.YawUncertaintyRad < 0 || !finite(s.PositionUncertaintyM) || s.PositionUncertaintyM < 0 || s.YawUncertaintyRad+v.AttitudeErrorRad+t.PairAttitudeBoundRad > .5 || s.PositionUncertaintyM+t.PairTranslationBoundM > p.MaxPositionUncertaintyM {
		return ErrInvalid
	}
	h := t.OdomPositionXYZ[2] - v.OdomFloorZM
	he := v.HeightCalibrationErrorM + .05 + math.Hypot(.001, -.0005)*t.OdometryEpochAgeS + t.PairTranslationBoundM
	if !near(t.BaseHeightMinM, h-he) || !near(t.BaseHeightMaxM, h+he) || t.BaseHeightMinM < v.BaseHeightMinM || t.BaseHeightMaxM > v.BaseHeightMaxM {
		return ErrInvalid
	}
	return nil
}

type projectedRay struct {
	angle, support float64
	end            Point
	finiteReturn   bool
}
type tiltedProjection struct {
	origin                            Point
	rays                              []projectedRay
	originError, angleError, attitude float64
}

func projectTilted(s Scan, p Profile) (tiltedProjection, error) {
	var out tiltedProjection
	if err := ValidateTiltedPose(s, p); err != nil {
		return out, err
	}
	v, t := p.TiltedScan, s.TiltedPose
	rot, _ := quaternionRotation(t.QuaternionWXYZ)
	mount := rot.apply(t.MountTranslationXYZ)
	eps := s.YawUncertaintyRad + v.AttitudeErrorRad + t.PairAttitudeBoundRad
	delta := 2 * math.Sin(eps/2)
	mountError := math.Hypot(.28, .08) * delta
	out.origin = Point{s.Robot.X + mount[0], s.Robot.Y + mount[1]}
	out.originError = s.PositionUncertaintyM + t.PairTranslationBoundM + mountError
	out.attitude = eps
	height := (t.BaseHeightMinM+t.BaseHeightMaxM)/2 + mount[2]
	heightError := (t.BaseHeightMaxM-t.BaseHeightMinM)/2 + mountError
	for i, r := range s.Ranges {
		a := s.AngleMin + float64(i)*s.AngleIncrement
		u := rot.apply([3]float64{math.Cos(a), math.Sin(a), 0})
		rho := math.Hypot(u[0], u[1])
		if rho <= delta {
			return tiltedProjection{}, ErrInvalid
		}
		out.angleError = math.Max(out.angleError, math.Asin(math.Min(1, delta/rho)))
		ray := projectedRay{angle: math.Atan2(u[1], u[0]), finiteReturn: finite(r)}
		if ray.angle < 0 {
			ray.angle += 2 * math.Pi
		}
		if finite(r) {
			ray.end = Point{out.origin.X + r*u[0], out.origin.Y + r*u[1]}
			cap := math.Min(r-v.RangeErrorM, v.MaxSupportRangeM)
			// The full uncertain prefix, not merely its endpoint, must stay inside
			// the solid obstacle slab. Ground and above-obstacle support are unknown.
			lower := height - heightError - v.SlabMarginM
			upper := v.ObstacleMinHeightM - v.SlabMarginM - height - heightError
			if lower <= 0 || upper <= 0 {
				cap = 0
			} else {
				if u[2]-delta < 0 {
					cap = math.Min(cap, lower/(delta-u[2]))
				}
				if u[2]+delta > 0 {
					cap = math.Min(cap, upper/(u[2]+delta))
				}
			}
			ray.support = math.Max(0, cap) * (rho - delta)
		}
		out.rays = append(out.rays, ray)
	}
	sort.Slice(out.rays, func(i, j int) bool { return out.rays[i].angle < out.rays[j].angle })
	return out, nil
}

func buildTilted(s Scan, area Bounds, p Profile) (*Grid, error) {
	proj, e := projectTilted(s, p)
	if e != nil {
		return nil, e
	}
	// A tilted rectangular prism projects no farther than its yaw-only rectangle
	// plus the vertical half-height contribution and full horizontal-corner
	// rotation chord. Using the commissioned maximum tilt also covers subsequent
	// supported gait tilt, not just the current orientation.
	tilt := p.MaxAbsRollRad + p.MaxAbsPitchRad
	bodyRotation := (math.Hypot(p.HalfLengthM, p.HalfWidthM) + p.TiltedScan.BodyHalfHeightM) * 2 * math.Sin((tilt+proj.attitude)/2)
	padding := p.ClearanceM + proj.originError + bodyRotation
	g := &Grid{motionEnvelope: p.MotionEnvelope, initialYawUncertainty: s.YawUncertaintyRad + p.TiltedScan.AttitudeErrorRad + s.TiltedPose.PairAttitudeBoundRad, area: area, cells: make(map[cell]occupancy), halfLength: p.HalfLengthM + padding, halfWidth: p.HalfWidthM + padding, sequence: s.Sequence, epoch: s.WorldEpoch, captured: s.CapturedUnixNS}
	// Duplicate one cycle either side to keep nonuniform angular brackets exact.
	n := len(proj.rays)
	rays := make([]projectedRay, 0, 3*n)
	for k := -1; k <= 1; k++ {
		for _, r := range proj.rays {
			r.angle += float64(k) * 2 * math.Pi
			rays = append(rays, r)
		}
	}
	lo, hi := at(area.Min), at(area.Max)
	if (hi.X-lo.X+1)*(hi.Y-lo.Y+1) > 81*81 {
		return nil, ErrInvalid
	}
	radius := CellSize/math.Sqrt2 + proj.originError
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			c := cell{x, y}
			q := center(c)
			if !area.contains(Point{q.X - CellSize/2, q.Y - CellSize/2}) || !area.contains(Point{q.X + CellSize/2, q.Y + CellSize/2}) {
				continue
			}
			d := distance(q, proj.origin)
			half := math.Pi
			if d > radius {
				half = math.Min(math.Pi, math.Asin(radius/d)+proj.angleError)
			}
			angle := math.Atan2(q.Y-proj.origin.Y, q.X-proj.origin.X)
			if angle < 0 {
				angle += 2 * math.Pi
			}
			first := sort.Search(len(rays), func(i int) bool { return rays[i].angle >= angle-half }) - 1
			last := sort.Search(len(rays), func(i int) bool { return rays[i].angle >= angle+half })
			if first < 0 || last >= len(rays) {
				continue
			}
			clear := true
			for i := first; i <= last; i++ {
				if rays[i].support <= d+radius+.02 {
					clear = false
					break
				}
			}
			if clear {
				g.cells[c] = free
			}
		}
	}
	for _, r := range proj.rays {
		if r.finiteReturn && area.contains(r.end) {
			g.cells[at(r.end)] = occupied
		}
	}
	return g, nil
}
