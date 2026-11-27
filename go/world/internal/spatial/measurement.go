package spatial

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/strictjson"
)

// Measurement is one sample from one sensor. Every concrete type embeds
// Header. None of them carries the sensor's pose: where a sensor is, and how
// its clock relates to site time, is what the spatial layer estimates.
type Measurement interface {
	// When is the sample instant on the reporting sensor's clock.
	When() time.Time
	From() (DeviceID, SensorID)
	Kind() SensorKind
	Validate() error
}

// Header identifies the reporting sensor and the sample instant. Time is on
// the sensor's own clock: site time = Time − Calibration.TimeOffset.
type Header struct {
	Device DeviceID  `json:"device"`
	Sensor SensorID  `json:"sensor"`
	Time   time.Time `json:"time"`
}

func (h Header) When() time.Time            { return h.Time }
func (h Header) From() (DeviceID, SensorID) { return h.Device, h.Sensor }

func (h Header) check() error {
	if !validID(string(h.Device)) || !validID(string(h.Sensor)) || h.Time.IsZero() {
		return invalid("measurement header")
	}
	return nil
}

// Sentinel range values. Any other value outside [RangeMin, RangeMax] is
// classified by Scan.Beam.
const (
	// RangeNoReturn: the beam fired and nothing came back within RangeMax, so
	// the space along the beam is free.
	RangeNoReturn = -1.0
	// RangeInvalid: dropout, saturation or a return closer than RangeMin. The
	// beam carries no information.
	RangeInvalid = -2.0
)

// MaxScanBeams bounds one Scan.
const MaxScanBeams = 4096

// BeamState classifies one beam of a Scan.
type BeamState int

const (
	BeamHit BeamState = iota
	BeamNoReturn
	BeamInvalid
)

// Scan is one sweep of a 2D lidar in the SENSOR frame. Beam i points along
// yaw AngleMin + i·AngleIncrement (from sensor +X toward sensor +Z) and its
// hit lies at Ranges[i]·(cos, sin) of that yaw.
type Scan struct {
	Header
	AngleMin       float64   `json:"angle_min"`
	AngleIncrement float64   `json:"angle_increment"`
	RangeMin       float64   `json:"range_min_m"`
	RangeMax       float64   `json:"range_max_m"`
	Ranges         []float64 `json:"ranges_m"`
	RangeSigma     float64   `json:"range_sigma_m"` // 1σ range noise the sensor declares
}

func (Scan) Kind() SensorKind { return KindLidar2D }

// Angle is the sensor-frame yaw of beam i.
func (s Scan) Angle(i int) float64 { return s.AngleMin + float64(i)*s.AngleIncrement }

// Beam returns the yaw, range and state of beam i. The range is meaningful
// only for BeamHit. A range beyond RangeMax counts as no return.
func (s Scan) Beam(i int) (angle, r float64, state BeamState) {
	angle, r = s.Angle(i), s.Ranges[i]
	switch {
	case r == RangeNoReturn || r > s.RangeMax:
		return angle, r, BeamNoReturn
	case r >= s.RangeMin && r > 0:
		return angle, r, BeamHit
	}
	return angle, r, BeamInvalid
}

// Points returns the hits in the sensor frame, in beam order.
func (s Scan) Points() []geom.Vec2 {
	pts := make([]geom.Vec2, 0, len(s.Ranges))
	for i := range s.Ranges {
		if a, r, state := s.Beam(i); state == BeamHit {
			pts = append(pts, geom.Heading(a).Scale(r))
		}
	}
	return pts
}

// ScanFromROS builds a Scan from the fields of a ROS sensor_msgs/LaserScan,
// whose angles run counter-clockwise seen from above while this package's run
// from +X toward +Z (clockwise): AngleMin and AngleIncrement change sign, so
// beam i keeps its index. +Inf becomes RangeNoReturn; NaN, −Inf and
// non-positive values become RangeInvalid. The result still needs Validate.
func ScanFromROS(h Header, angleMin, angleIncrement, rangeMin, rangeMax, rangeSigma float64, ranges []float64) Scan {
	s := Scan{Header: h, AngleMin: -angleMin, AngleIncrement: -angleIncrement, RangeMin: rangeMin, RangeMax: rangeMax, RangeSigma: rangeSigma, Ranges: make([]float64, len(ranges))}
	for i, r := range ranges {
		switch {
		case math.IsInf(r, 1):
			s.Ranges[i] = RangeNoReturn
		case !(r > 0) || math.IsInf(r, 0):
			s.Ranges[i] = RangeInvalid
		default:
			s.Ranges[i] = r
		}
	}
	return s
}

func (s Scan) Validate() error {
	if err := s.check(); err != nil {
		return err
	}
	if len(s.Ranges) == 0 || len(s.Ranges) > MaxScanBeams || !finite(s.AngleMin, s.AngleIncrement, s.RangeMin, s.RangeMax, s.RangeSigma) ||
		s.AngleIncrement == 0 || s.RangeMin < 0 || s.RangeMax <= s.RangeMin || s.RangeSigma < 0 || !finite(s.Ranges...) {
		return invalid("scan")
	}
	return nil
}

// OdometryNoise describes how odometry error grows with motion. Each
// coefficient k acts twice, with equal weight:
//
//   - as the 1σ of a bias fixed per device (wheel scale, heading drift per
//     metre, turn scale), whose error grows linearly: k·d after d of motion;
//   - as white noise whose variance grows linearly, so its 1σ grows with the
//     square root: k·√d.
//
// Over a short step the white part dominates (k·√d ≫ k·d for d ≪ 1), so
// reading k as "error per metre" alone is over-confident by 1/√d. Covariance
// combines both parts.
type OdometryNoise struct {
	TransPerMetre float64 `json:"trans_per_m"` // position: m per m of bias, m per √m of white noise
	YawPerMetre   float64 `json:"yaw_per_m"`   // heading from travel: rad per m, rad per √m
	YawPerRadian  float64 `json:"yaw_per_rad"` // heading from turning: rad per rad, rad per √rad
}

// Covariance is the covariance over (x, z, yaw) of one odometry increment
// delta = previous.Between(current), in the previous pose's frame: the motion
// model of a filter prediction, or the covariance of a pose-graph odometry
// edge (invert it for the information).
//
// With d = |delta position| and θ = |delta yaw|:
//
//	position   TransPerMetre²·d on both axes (white)
//	           + TransPerMetre²·d² along the direction of travel (scale bias)
//	yaw        YawPerMetre²·(d + d²) + YawPerRadian²·(θ + θ²)
//
// delta should span one short, monotone motion such as a keyframe step: d and
// θ are net motion, so back-and-forth travel inside the step is not counted,
// and the heading error accumulated inside the step is not propagated into
// position. The bias is the same on every increment of one device, which
// independent per-edge covariances cannot express; the sum over a long chain
// therefore under-states the drift. A device that did not move has a zero
// covariance, which has no inverse: add the caller's own floor first.
func (n OdometryNoise) Covariance(delta geom.Pose2) geom.Mat3 {
	d, turn := delta.Pos().Norm(), math.Abs(geom.WrapAngle(delta.Yaw))
	t2, ym2, yr2 := n.TransPerMetre*n.TransPerMetre, n.YawPerMetre*n.YawPerMetre, n.YawPerRadian*n.YawPerRadian
	return geom.Mat3{
		t2 * (d + delta.X*delta.X), t2 * delta.X * delta.Z, 0,
		t2 * delta.X * delta.Z, t2 * (d + delta.Z*delta.Z), 0,
		0, 0, ym2*(d+d*d) + yr2*(turn+turn*turn),
	}
}

// Odometry is the pose of the device base in the device's OWN odometry
// frame, which starts at the identity when the device powers on and drifts.
// Only differences between two samples are meaningful.
type Odometry struct {
	Header
	Pose           geom.Pose2    `json:"pose"`
	ElevationKnown bool          `json:"elevation_known"`
	Elevation      float64       `json:"elevation_m"` // relative to the odometry origin; valid when ElevationKnown
	Noise          OdometryNoise `json:"noise"`
}

func (Odometry) Kind() SensorKind { return KindOdometry }

func (o Odometry) Validate() error {
	if err := o.check(); err != nil {
		return err
	}
	n := o.Noise
	if !finite(o.Pose.X, o.Pose.Z, o.Pose.Yaw, o.Elevation, n.TransPerMetre, n.YawPerMetre, n.YawPerRadian) || n.TransPerMetre < 0 || n.YawPerMetre < 0 || n.YawPerRadian < 0 {
		return invalid("odometry")
	}
	return nil
}

// Imu is a planar inertial sample in the sensor frame. YawRate is positive
// from +X toward +Z. Accel is specific force, so a level sensor at rest reads
// about (0, +9.81, 0).
type Imu struct {
	Header
	YawRate    float64   `json:"yaw_rate_rad_s"`
	Accel      geom.Vec3 `json:"accel_m_s2"`
	GyroSigma  float64   `json:"gyro_sigma_rad_s"`
	AccelSigma float64   `json:"accel_sigma_m_s2"`
}

func (Imu) Kind() SensorKind { return KindImu }

func (m Imu) Validate() error {
	if err := m.check(); err != nil {
		return err
	}
	if !finite(m.YawRate, m.Accel.X, m.Accel.Y, m.Accel.Z, m.GyroSigma, m.AccelSigma) || m.GyroSigma < 0 || m.AccelSigma < 0 {
		return invalid("imu sample")
	}
	return nil
}

// Baro is a barometer sample. Altitude is derived from the pressure relative
// to the sensor's own reference; its zero is arbitrary and drifts slowly, so
// only short-term differences are meaningful.
type Baro struct {
	Header
	PressurePa    float64 `json:"pressure_pa"`
	Altitude      float64 `json:"altitude_m"`
	AltitudeSigma float64 `json:"altitude_sigma_m"`
}

func (Baro) Kind() SensorKind { return KindBaro }

func (b Baro) Validate() error {
	if err := b.check(); err != nil {
		return err
	}
	if !finite(b.PressurePa, b.Altitude, b.AltitudeSigma) || b.PressurePa <= 0 || b.AltitudeSigma < 0 {
		return invalid("barometer sample")
	}
	return nil
}

// GnssFix is a satellite position fix of the receiver antenna in WGS84.
// AltitudeMetres is the height above the WGS84 ellipsoid, the datum
// geography.Coordinate requires; a receiver that reports height above mean
// sea level must have the geoid separation added by its adapter.
type GnssFix struct {
	Header
	LatitudeDegrees    float64 `json:"latitude_degrees"`
	LongitudeDegrees   float64 `json:"longitude_degrees"`
	AltitudeMetres     float64 `json:"altitude_metres"`       // above the WGS84 ellipsoid
	HorizontalAccuracy float64 `json:"horizontal_accuracy_m"` // 1σ
	VerticalAccuracy   float64 `json:"vertical_accuracy_m"`   // 1σ; 0 when not reported
}

func (GnssFix) Kind() SensorKind { return KindGnss }

func (g GnssFix) Validate() error {
	if err := g.check(); err != nil {
		return err
	}
	if !finite(g.LatitudeDegrees, g.LongitudeDegrees, g.AltitudeMetres, g.HorizontalAccuracy, g.VerticalAccuracy) ||
		math.Abs(g.LatitudeDegrees) > 90 || math.Abs(g.LongitudeDegrees) > 180 || g.HorizontalAccuracy <= 0 || g.VerticalAccuracy < 0 {
		return invalid("GNSS fix")
	}
	return nil
}

// DetectionKind says what a camera detection refers to.
type DetectionKind string

const (
	// DetectFiducial is one point of a FiducialGeometry: a robot marker point
	// or a static wall landmark. ID is the fiducial id, Index the point index.
	DetectFiducial DetectionKind = "fiducial"
	// DetectTargetCorner is one corner of a TargetGeometry. ID is the target
	// id, Index the corner index.
	DetectTargetCorner DetectionKind = "target_corner"
	// DetectObject is a class-labelled object. Pixel is its foot or reference
	// point, ID an optional detector-local instance id.
	DetectObject DetectionKind = "object"
)

// BBox is an axis-aligned box in raw pixels.
type BBox struct {
	Min geom.Pixel `json:"min"`
	Max geom.Pixel `json:"max"`
}

// Detection is one detector output in a camera frame. Pixel coordinates are
// raw, that is distorted, pixels.
type Detection struct {
	Kind       DetectionKind `json:"kind"`
	ID         string        `json:"id,omitempty"`
	Index      int           `json:"index,omitempty"`
	Pixel      geom.Pixel    `json:"pixel"`
	Class      string        `json:"class,omitempty"`      // objects only
	Confidence float64       `json:"confidence,omitempty"` // objects only, [0, 1]
	BBox       *BBox         `json:"bbox,omitempty"`       // objects only
}

func (d Detection) check() error {
	if !finite(d.Pixel.U, d.Pixel.V) {
		return invalid("detection pixel")
	}
	switch d.Kind {
	case DetectFiducial, DetectTargetCorner:
		if !validID(d.ID) || d.Index < 0 || d.Class != "" || d.BBox != nil || d.Confidence != 0 {
			return invalid("fiducial or target detection")
		}
	case DetectObject:
		if (d.ID != "" && !validID(d.ID)) || d.Index != 0 || !validID(d.Class) || !unit(d.Confidence) || d.BBox == nil ||
			!finite(d.BBox.Min.U, d.BBox.Min.V, d.BBox.Max.U, d.BBox.Max.V) || d.BBox.Max.U <= d.BBox.Min.U || d.BBox.Max.V <= d.BBox.Min.V {
			return invalid("object detection")
		}
	default:
		return invalid("detection kind")
	}
	return nil
}

// MaxDetections bounds one CameraFrame.
const MaxDetections = 4096

// CameraFrame is the detector output for one image. The image itself is not
// carried. An empty Detections slice is a frame in which nothing was seen.
type CameraFrame struct {
	Header
	Width      int         `json:"width"`
	Height     int         `json:"height"`
	PixelSigma float64     `json:"pixel_sigma"` // 1σ localisation noise of fiducial and corner pixels
	Detections []Detection `json:"detections"`
}

func (CameraFrame) Kind() SensorKind { return KindCamera }

func (f CameraFrame) Validate() error {
	if err := f.check(); err != nil {
		return err
	}
	if f.Width <= 0 || f.Height <= 0 || !finite(f.PixelSigma) || f.PixelSigma < 0 || len(f.Detections) > MaxDetections {
		return invalid("camera frame")
	}
	for _, d := range f.Detections {
		if err := d.check(); err != nil {
			return err
		}
	}
	return nil
}

// Range is a UWB distance between the reporting sensor and a peer endpoint,
// which is another device or a fixed beacon. One ranging round yields one
// Range per peer, all with the same Header.
type Range struct {
	Header
	Peer       DeviceID `json:"peer"`
	PeerSensor SensorID `json:"peer_sensor,omitempty"`
	Distance   float64  `json:"distance_m"`
	Sigma      float64  `json:"sigma_m"`
	// LineOfSight is the radio's own verdict on the direct path, nil when it
	// does not report one. A blocked path reads long; Sigma should already
	// reflect that, so this is a hint for gating, not a correction.
	LineOfSight *bool `json:"line_of_sight,omitempty"`
}

func (Range) Kind() SensorKind { return KindUwb }

func (r Range) Validate() error {
	if err := r.check(); err != nil {
		return err
	}
	if !validID(string(r.Peer)) || (r.PeerSensor != "" && !validID(string(r.PeerSensor))) || !finite(r.Distance, r.Sigma) || r.Distance < 0 || r.Sigma <= 0 {
		return invalid("range")
	}
	return nil
}

// Bearing is a direction of arrival in the sensor frame: Azimuth is the yaw
// from sensor +X toward sensor +Z at which the source lies. Source names the
// emitter when the sensor can tell sources apart and is empty otherwise.
type Bearing struct {
	Header
	Azimuth float64 `json:"azimuth_rad"`
	Sigma   float64 `json:"sigma_rad"`
	Source  string  `json:"source,omitempty"`
}

func (Bearing) Kind() SensorKind { return KindMic }

func (b Bearing) Validate() error {
	if err := b.check(); err != nil {
		return err
	}
	if !finite(b.Azimuth, b.Sigma) || b.Sigma <= 0 || (b.Source != "" && !validID(b.Source)) {
		return invalid("bearing")
	}
	return nil
}

// Scalar is a point reading of a field, such as a gas concentration in ppm.
type Scalar struct {
	Header
	Quantity string  `json:"quantity"` // what is measured, for example "co2"
	Value    float64 `json:"value"`
	Unit     string  `json:"unit"`
	Sigma    float64 `json:"sigma"` // 1σ in Unit
}

func (Scalar) Kind() SensorKind { return KindGas }

func (s Scalar) Validate() error {
	if err := s.check(); err != nil {
		return err
	}
	if !validID(s.Quantity) || !validID(s.Unit) || !finite(s.Value, s.Sigma) || s.Sigma < 0 {
		return invalid("scalar")
	}
	return nil
}

// AttachmentMode is a sensor's own hint about how it is mounted.
type AttachmentMode string

const (
	AttachUnknown AttachmentMode = "unknown"
	AttachFixed   AttachmentMode = "fixed"   // bolted to the building
	AttachMounted AttachmentMode = "mounted" // rigidly carried by Attachment.Carrier
)

// Attachment is a hint, not a pose: it says what the sensor is rigidly
// attached to, never where.
type Attachment struct {
	Mode    AttachmentMode `json:"mode"`
	Carrier DeviceID       `json:"carrier,omitempty"` // required for AttachMounted
}

// SensorManifest is what a sensor announces when it appears. It carries no
// pose. Declared intrinsics are a starting point, not a calibration.
type SensorManifest struct {
	Header                      // Time is the announcement instant
	Modality   SensorKind       `json:"kind"`
	Model      string           `json:"model"`
	Intrinsics *geom.Intrinsics `json:"intrinsics,omitempty"`
	Attachment Attachment       `json:"attachment"`
}

func (m SensorManifest) Kind() SensorKind { return m.Modality }

func (m SensorManifest) Validate() error {
	if err := m.check(); err != nil {
		return err
	}
	if !m.Modality.Valid() || m.Model == "" || !validText(m.Model) {
		return invalid("sensor manifest")
	}
	if m.Intrinsics != nil {
		if m.Modality != KindCamera {
			return invalid("sensor manifest: only cameras declare intrinsics")
		}
		if err := m.Intrinsics.Validate(); err != nil {
			return err
		}
	}
	switch m.Attachment.Mode {
	case AttachUnknown, AttachFixed:
		if m.Attachment.Carrier != "" {
			return invalid("sensor manifest: only a mounted sensor names a carrier")
		}
	case AttachMounted:
		if !validID(string(m.Attachment.Carrier)) {
			return invalid("sensor manifest: mounted sensor needs a carrier")
		}
	default:
		return invalid("sensor manifest attachment")
	}
	return nil
}

// MaxGeometryPoints bounds one FiducialGeometry or TargetGeometry.
const MaxGeometryPoints = 1024

// FiducialGeometry is the known shape of a fiducial: the positions of its
// detectable points, indexed as Detection.Index. With a Carrier the points
// are in that device's base frame (x forward, y up, z right), which is how a
// robot marker constrains the robot's pose. Without one the fiducial is a
// static landmark and the points are in its own local frame; its place in
// the site is estimated, not declared here.
type FiducialGeometry struct {
	ID      string      `json:"id"`
	Carrier DeviceID    `json:"carrier,omitempty"`
	Points  []geom.Vec3 `json:"points_m"`
}

func (g FiducialGeometry) Validate() error {
	if !validID(g.ID) || (g.Carrier != "" && !validID(string(g.Carrier))) || len(g.Points) == 0 || len(g.Points) > MaxGeometryPoints {
		return invalid("fiducial geometry")
	}
	for _, p := range g.Points {
		if !coordinates(p.X, p.Y, p.Z) {
			return invalid("fiducial geometry point")
		}
	}
	return nil
}

// TargetGeometry is a planar calibration target: a Rows x Cols grid of
// corners with Spacing metres between neighbours. Corner index row·Cols+col
// lies at (col·Spacing, row·Spacing, 0) in the target frame.
type TargetGeometry struct {
	ID      string  `json:"id"`
	Rows    int     `json:"rows"`
	Cols    int     `json:"cols"`
	Spacing float64 `json:"spacing_m"`
}

func (g TargetGeometry) Validate() error {
	if !validID(g.ID) || g.Rows < 2 || g.Cols < 2 || g.Rows*g.Cols > MaxGeometryPoints || !finite(g.Spacing) || g.Spacing <= 0 {
		return invalid("target geometry")
	}
	return nil
}

// Count is the number of corners.
func (g TargetGeometry) Count() int { return g.Rows * g.Cols }

// Corner returns corner i in the target frame; ok is false out of range.
func (g TargetGeometry) Corner(i int) (geom.Vec3, bool) {
	if i < 0 || g.Cols <= 0 || i >= g.Count() {
		return geom.Vec3{}, false
	}
	return geom.Vec3{X: float64(i%g.Cols) * g.Spacing, Y: float64(i/g.Cols) * g.Spacing}, true
}

// MaxMeasurementBytes bounds one encoded measurement record.
const MaxMeasurementBytes = 1 << 20

type measurementRecord struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func recordType(m Measurement) string {
	switch m.(type) {
	case Scan:
		return "scan"
	case Odometry:
		return "odometry"
	case Imu:
		return "imu"
	case Baro:
		return "baro"
	case GnssFix:
		return "gnss"
	case CameraFrame:
		return "camera_frame"
	case Range:
		return "range"
	case Bearing:
		return "bearing"
	case Scalar:
		return "scalar"
	case SensorManifest:
		return "manifest"
	}
	return ""
}

// EncodeMeasurement validates m and returns one JSON object without a
// trailing newline: the line format of recorded measurement logs (NDJSON).
// m must be one of this package's measurement types, by value.
func EncodeMeasurement(m Measurement) ([]byte, error) {
	if m == nil {
		return nil, errors.New("spatial: nil measurement")
	}
	name := recordType(m)
	if name == "" {
		return nil, fmt.Errorf("spatial: unsupported measurement type %T", m)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(measurementRecord{name, data})
}

func decodeAs[T Measurement](data []byte) (Measurement, error) {
	var v T
	if err := strictjson.DecodeLimit(data, &v, MaxMeasurementBytes); err != nil {
		return nil, err
	}
	return v, v.Validate()
}

// DecodeMeasurement reads one record written by EncodeMeasurement. Unknown or
// duplicate fields, an unknown record type and invalid values are errors.
func DecodeMeasurement(line []byte) (Measurement, error) {
	var rec measurementRecord
	if err := strictjson.DecodeLimit(line, &rec, MaxMeasurementBytes); err != nil {
		return nil, err
	}
	switch rec.Type {
	case "scan":
		return decodeAs[Scan](rec.Data)
	case "odometry":
		return decodeAs[Odometry](rec.Data)
	case "imu":
		return decodeAs[Imu](rec.Data)
	case "baro":
		return decodeAs[Baro](rec.Data)
	case "gnss":
		return decodeAs[GnssFix](rec.Data)
	case "camera_frame":
		return decodeAs[CameraFrame](rec.Data)
	case "range":
		return decodeAs[Range](rec.Data)
	case "bearing":
		return decodeAs[Bearing](rec.Data)
	case "scalar":
		return decodeAs[Scalar](rec.Data)
	case "manifest":
		return decodeAs[SensorManifest](rec.Data)
	}
	return nil, fmt.Errorf("spatial: unknown measurement record type %q", rec.Type)
}
