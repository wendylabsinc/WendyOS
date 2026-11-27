package go2nav

import (
	"math"
	"sort"
)

const (
	CloudModelV1                  = "coherent-multiring-slab-v1"
	CloudSourceDigest             = "sha256:2869d018ae55e4c83318329bb20cc670e16f6f62838989bb5192829002af8d2b"
	CloudRings                    = 25
	CloudAzimuths                 = 360
	CloudRayCount                 = CloudRings * CloudAzimuths
	CloudRangeQuantizationM       = 1.0 / (1 << 20)
	CloudDirectionQuantizationRad = 4.0 / (1 << 24)
)

// CloudObservation is an offline, source-pinned observation, not authenticated
// authority. Scan contains the actual same-stamp horizontal companion and its
// paired original odometry. Scan.WorldEpoch is separately observed context:
// PointCloud2 does not encode an epoch. The caller verifies association and
// freshness; this function does not read a clock or establish either property.
// Ranges has exactly 25*360 recovered norms in original ring-major order.
// Missing returns are +Inf, never a fabricated maximum-range measurement.
type CloudObservation struct {
	Model            string    `json:"model"`
	SourceDigest     string    `json:"source_digest"`
	Frame            string    `json:"frame"`
	PublisherGID     string    `json:"publisher_gid"`
	SerializedSHA256 string    `json:"serialized_sha256"`
	CapturedUnixNS   int64     `json:"captured_unix_ns"`
	Scan             Scan      `json:"scan"`
	Ranges           []float64 `json:"ranges"`
}

type CloudRayEvidence struct {
	Index           int     `json:"index"`
	Ring            int     `json:"ring"`
	Azimuth         int     `json:"azimuth"`
	BearingRad      float64 `json:"bearing_rad"`
	SupportM        float64 `json:"support_m"`
	EndpointXY      Point   `json:"endpoint_xy"`
	EndpointRadiusM float64 `json:"endpoint_radius_m"`
	EndZMinM        float64 `json:"end_z_min_m"`
	EndZMaxM        float64 `json:"end_z_max_m"`
	// missing, below_floor, definite_interior, or ambiguous. A minimum
	// column height is not a maximum height: above-top returns stay ambiguous.
	Class string `json:"class"`
}

type CloudCellEvidence struct {
	X                      int    `json:"x"`
	Y                      int    `json:"y"`
	State                  string `json:"state"` // unknown, free, occupied
	WitnessRings           uint32 `json:"witness_rings"`
	AmbiguousEndpointRings uint32 `json:"ambiguous_endpoint_rings"`
	DefiniteEndpointRings  uint32 `json:"definite_endpoint_rings"`
	ResolvedAmbiguousRings uint32 `json:"resolved_ambiguous_rings"`
	Reason                 string `json:"reason"`
}

// CloudEvidence is bounded by 9000 ray entries and 81*81 cell entries. Masks
// use bit r for elevation ring r. Witnesses precede endpoint interpretation;
// a witness from the endpoint's own ring cannot resolve its ambiguity.
// These are finite-angular, solid-column model results, not a universal proof
// against thin objects between rays, short obstacles, holes, or overhangs.
type CloudEvidence struct {
	Model            string              `json:"model"`
	SourceDigest     string              `json:"source_digest"`
	PublisherGID     string              `json:"publisher_gid"`
	SerializedSHA256 string              `json:"serialized_sha256"`
	CapturedUnixNS   int64               `json:"captured_unix_ns"`
	Sequence         uint64              `json:"sequence"`
	ObservedEpoch    uint64              `json:"observed_epoch"`
	Origin           Point               `json:"origin"`
	OriginErrorM     float64             `json:"origin_error_m"`
	AttitudeErrorRad float64             `json:"attitude_error_rad"`
	Rays             []CloudRayEvidence  `json:"rays"`
	Cells            []CloudCellEvidence `json:"cells"`
}

func cloudHex(s string, n int, nonzero bool) bool {
	if len(s) != n {
		return false
	}
	any := false
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
		any = any || c != '0'
	}
	return !nonzero || any
}

type cloudRing struct {
	rays       []projectedRay
	angleError float64
}

// BuildCloud constructs a separate offline grid. It never invokes an adapter,
// changes a legacy Build result, caches permission, or extends capture age.
// Definite endpoint footprints veto free witnesses. Ambiguous footprints need
// a complete different-ring witness for every originating ring touching a cell.
func BuildCloud(o CloudObservation, area Bounds, p Profile) (*Grid, CloudEvidence, error) {
	var evidence CloudEvidence
	if o.Model != CloudModelV1 || o.SourceDigest != CloudSourceDigest || o.Frame != "utlidar_lidar" ||
		!cloudHex(o.PublisherGID, 48, true) || !cloudHex(o.SerializedSHA256, 64, false) ||
		o.CapturedUnixNS <= 0 || o.CapturedUnixNS != o.Scan.CapturedUnixNS ||
		len(o.Ranges) != CloudRayCount || len(o.Scan.Ranges) != CloudAzimuths ||
		p.TiltedScan == nil || o.Scan.TiltedPose == nil ||
		math.Abs(o.Scan.AngleMin+math.Pi) > angularQuantizationTolerance ||
		math.Abs(o.Scan.AngleIncrement-2*math.Pi/CloudAzimuths) > 1e-9 {
		return nil, evidence, ErrInvalid
	}
	// No input arrays or pointers are retained in the returned grid/evidence.
	o.Ranges = append([]float64(nil), o.Ranges...)
	o.Scan.Ranges = append([]float64(nil), o.Scan.Ranges...)
	pose, profile := *o.Scan.TiltedPose, *p.TiltedScan
	o.Scan.TiltedPose, p.TiltedScan = &pose, &profile
	for i, r := range o.Ranges {
		if !math.IsInf(r, 1) && (!finite(r) || r < .1-CloudRangeQuantizationM || r > 12+CloudRangeQuantizationM) {
			return nil, evidence, ErrInvalid
		}
		if i/CloudAzimuths == 12 {
			h := o.Scan.Ranges[i%CloudAzimuths]
			if math.IsInf(r, 1) != math.IsInf(h, 1) ||
				!math.IsInf(r, 1) && (!finite(h) || math.Abs(r-h) > CloudRangeQuantizationM+12.0/(1<<24)) {
				return nil, evidence, ErrInvalid
			}
		}
	}
	// Reuse unchanged validation and footprint constraints. Its temporary
	// horizontal-only occupancy is discarded, never used as cloud evidence.
	g, err := Build(o.Scan, area, p)
	if err != nil {
		return nil, evidence, err
	}
	g.cells = make(map[cell]occupancy)
	s, v, t := o.Scan, p.TiltedScan, o.Scan.TiltedPose
	rot, _ := quaternionRotation(t.QuaternionWXYZ)
	mount := rot.apply(t.MountTranslationXYZ)
	// Direction quantization is included conservatively in the shared angular
	// bound; including it in mount/body padding can only enlarge the footprint.
	eps := s.YawUncertaintyRad + v.AttitudeErrorRad + t.PairAttitudeBoundRad + CloudDirectionQuantizationRad
	if eps > .5 {
		return nil, CloudEvidence{}, ErrInvalid
	}
	delta := 2 * math.Sin(eps/2)
	mountError := math.Hypot(.28, .08) * delta
	origin := Point{s.Robot.X + mount[0], s.Robot.Y + mount[1]}
	originError := s.PositionUncertaintyM + t.PairTranslationBoundM + mountError
	height := (t.BaseHeightMinM+t.BaseHeightMaxM)/2 + mount[2]
	heightError := (t.BaseHeightMaxM-t.BaseHeightMinM)/2 + mountError
	rangeError := v.RangeErrorM + CloudRangeQuantizationM
	bodyRotation := (math.Hypot(p.HalfLengthM, p.HalfWidthM) + v.BodyHalfHeightM) * 2 * math.Sin((p.MaxAbsRollRad+p.MaxAbsPitchRad+eps)/2)
	g.halfLength = p.HalfLengthM + p.ClearanceM + originError + bodyRotation
	g.halfWidth = p.HalfWidthM + p.ClearanceM + originError + bodyRotation
	g.initialYawUncertainty = eps
	evidence = CloudEvidence{Model: o.Model, SourceDigest: o.SourceDigest, PublisherGID: o.PublisherGID,
		SerializedSHA256: o.SerializedSHA256, CapturedUnixNS: o.CapturedUnixNS, Sequence: s.Sequence,
		ObservedEpoch: s.WorldEpoch, Origin: origin, OriginErrorM: originError, AttitudeErrorRad: eps,
		Rays: make([]CloudRayEvidence, 0, CloudRayCount)}
	var rings [CloudRings]cloudRing
	for ring := 0; ring < CloudRings; ring++ {
		elevation := (-30 + 2.5*float64(ring)) * math.Pi / 180
		for azimuth := 0; azimuth < CloudAzimuths; azimuth++ {
			index := ring*CloudAzimuths + azimuth
			angle := -math.Pi + float64(azimuth)*2*math.Pi/CloudAzimuths
			u := rot.apply([3]float64{math.Cos(elevation) * math.Cos(angle), math.Cos(elevation) * math.Sin(angle), math.Sin(elevation)})
			rho := math.Hypot(u[0], u[1])
			if rho <= delta {
				return nil, CloudEvidence{}, ErrInvalid
			}
			bearing := math.Atan2(u[1], u[0])
			if bearing < 0 {
				bearing += 2 * math.Pi
			}
			rings[ring].angleError = math.Max(rings[ring].angleError, math.Asin(delta/rho))
			ray := projectedRay{angle: bearing}
			record := CloudRayEvidence{Index: index, Ring: ring, Azimuth: azimuth, BearingRad: bearing, Class: "missing"}
			r := o.Ranges[index]
			if finite(r) {
				cap := math.Min(r-rangeError, v.MaxSupportRangeM)
				lower, upper := height-heightError-v.SlabMarginM, v.ObstacleMinHeightM-v.SlabMarginM-height-heightError
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
				record.SupportM = ray.support
				record.EndpointXY = Point{origin.X + r*u[0], origin.Y + r*u[1]}
				record.EndpointRadiusM = originError + r*delta + rangeError
				z, ze := height+r*u[2], heightError+r*delta+rangeError
				record.EndZMinM, record.EndZMaxM = z-ze, z+ze
				record.Class = "ambiguous"
				if record.EndZMaxM < 0 {
					record.Class = "below_floor"
				} else if record.EndZMinM > v.SlabMarginM && record.EndZMaxM < v.ObstacleMinHeightM-v.SlabMarginM {
					record.Class = "definite_interior"
				}
			}
			rings[ring].rays = append(rings[ring].rays, ray)
			evidence.Rays = append(evidence.Rays, record)
		}
		sort.Slice(rings[ring].rays, func(i, j int) bool { return rings[ring].rays[i].angle < rings[ring].rays[j].angle })
		base := rings[ring].rays
		rings[ring].rays = make([]projectedRay, 0, 3*CloudAzimuths)
		for k := -1; k <= 1; k++ {
			for _, r := range base {
				r.angle += float64(k) * 2 * math.Pi
				rings[ring].rays = append(rings[ring].rays, r)
			}
		}
	}
	lo, hi := at(area.Min), at(area.Max)
	count := (hi.X - lo.X + 1) * (hi.Y - lo.Y + 1)
	if count > 81*81 {
		return nil, CloudEvidence{}, ErrInvalid
	}
	evidence.Cells = make([]CloudCellEvidence, 0, count)
	indices := make(map[cell]int, count)
	radius := CellSize/math.Sqrt2 + originError
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			c := cell{x, y}
			q := center(c)
			ce := CloudCellEvidence{X: x, Y: y, State: "unknown", Reason: "no_complete_ring_witness"}
			if !area.contains(Point{q.X - CellSize/2, q.Y - CellSize/2}) || !area.contains(Point{q.X + CellSize/2, q.Y + CellSize/2}) {
				ce.Reason = "bounds"
			} else {
				for ring := range rings {
					if cloudCellWitness(q, origin, radius, rings[ring]) {
						ce.WitnessRings |= 1 << ring
					}
				}
			}
			indices[c] = len(evidence.Cells)
			evidence.Cells = append(evidence.Cells, ce)
		}
	}
	// Endpoint uncertainty footprints are intersected even when the nominal
	// endpoint lies outside the commissioned area. Grazing cells are included.
	for _, r := range evidence.Rays {
		if r.Class == "missing" || r.Class == "below_floor" {
			continue
		}
		first, last := at(Point{r.EndpointXY.X - r.EndpointRadiusM, r.EndpointXY.Y - r.EndpointRadiusM}), at(Point{r.EndpointXY.X + r.EndpointRadiusM, r.EndpointXY.Y + r.EndpointRadiusM})
		first.X = max(first.X-1, lo.X)
		first.Y = max(first.Y-1, lo.Y)
		last.X = min(last.X, hi.X)
		last.Y = min(last.Y, hi.Y)
		for y := first.Y; y <= last.Y; y++ {
			for x := first.X; x <= last.X; x++ {
				c := cell{x, y}
				if !cloudDiskTouchesCell(r.EndpointXY, r.EndpointRadiusM, c) {
					continue
				}
				ce := &evidence.Cells[indices[c]]
				if r.Class == "definite_interior" {
					ce.DefiniteEndpointRings |= 1 << r.Ring
				} else {
					ce.AmbiguousEndpointRings |= 1 << r.Ring
				}
			}
		}
	}
	for i := range evidence.Cells {
		ce := &evidence.Cells[i]
		if ce.Reason == "bounds" {
			continue
		}
		if ce.DefiniteEndpointRings != 0 {
			ce.State, ce.Reason = "occupied", "definite_endpoint"
			if ce.WitnessRings != 0 {
				ce.Reason = "definite_endpoint_conflict"
			}
			g.cells[cell{ce.X, ce.Y}] = occupied
			continue
		}
		for ring := 0; ring < CloudRings; ring++ {
			bit := uint32(1) << ring
			if ce.AmbiguousEndpointRings&bit != 0 && ce.WitnessRings & ^bit != 0 {
				ce.ResolvedAmbiguousRings |= bit
			}
		}
		if ce.AmbiguousEndpointRings != ce.ResolvedAmbiguousRings {
			ce.Reason = "ambiguous_endpoint"
			continue
		}
		if ce.WitnessRings != 0 {
			ce.State, ce.Reason = "free", "complete_ring_witness"
			g.cells[cell{ce.X, ce.Y}] = free
		}
	}
	return g, evidence, nil
}

func cloudCellWitness(q, origin Point, radius float64, ring cloudRing) bool {
	d := distance(q, origin)
	half := math.Pi
	if d > radius {
		half = math.Min(math.Pi, math.Asin(radius/d)+ring.angleError)
	}
	angle := math.Atan2(q.Y-origin.Y, q.X-origin.X)
	if angle < 0 {
		angle += 2 * math.Pi
	}
	first := sort.Search(len(ring.rays), func(i int) bool { return ring.rays[i].angle >= angle-half }) - 1
	last := sort.Search(len(ring.rays), func(i int) bool { return ring.rays[i].angle >= angle+half })
	if first < 0 || last >= len(ring.rays) {
		return false
	}
	for i := first; i <= last; i++ {
		if ring.rays[i].support <= d+radius+.02 {
			return false
		}
	}
	return true
}

func cloudDiskTouchesCell(p Point, radius float64, c cell) bool {
	q := center(c)
	dx := math.Max(0, math.Abs(p.X-q.X)-CellSize/2)
	dy := math.Max(0, math.Abs(p.Y-q.Y)-CellSize/2)
	return math.Hypot(dx, dy) <= radius
}
