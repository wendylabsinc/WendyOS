package spatial

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Component types of the spatial layer. Types lists them all.
const (
	TypeCalibration        = "wendy.spatial.calibration.v1"
	TypeDevicePose         = "wendy.spatial.device_pose.v1"
	TypeSensorRegistration = "wendy.spatial.sensor_registration.v1"
	TypeAnchor             = "wendy.spatial.anchor.v1"
	TypeTrack              = "wendy.spatial.track.v1"
	TypeMapTile            = "wendy.spatial.map_tile.v1"
	TypeRegion             = "wendy.spatial.region.v1"
	TypeChangeEvent        = "wendy.spatial.change_event.v1"
	TypeFloorInfo          = "wendy.spatial.floor.v1"
	TypePortal             = "wendy.spatial.portal.v1"
	TypeGeoReference       = "wendy.spatial.geo_reference.v1"
	TypeKeyframe           = "wendy.spatial.keyframe.v1"
	TypeLandmark           = "wendy.spatial.landmark.v1"
	TypeObject             = "wendy.spatial.object.v1"
	TypeFrameAlignment     = "wendy.spatial.frame_alignment.v1"
	TypeLayerTile          = "wendy.spatial.layer_tile.v1"
)

// Payload is the body of one published spatial component.
type Payload interface {
	// Type is the component type, one of the Type* constants.
	Type() string
	// EntityKey is the component key's entity: stable for the thing the
	// payload describes, so successive versions replace each other.
	EntityKey() string
	Validate() error
}

// Location is the site-frame placement that selection.Region.Classify reads
// from a payload: "position_m" is [x, y, z] in metres and "uncertainty_m" is
// a radius around it. For a point estimate the radius is the position
// uncertainty. For something with extent (a region, a map tile, a portal) it
// is at least the bounding radius, so an area query never misses an overlap.
//
// Payloads that always have a place embed Location. Payloads that may not
// (a robot-mounted sensor's calibration) embed *Location, which is nil and
// absent from the JSON when there is none; check it before reading the fields.
type Location struct {
	PositionM    [3]float64 `json:"position_m"`
	UncertaintyM float64    `json:"uncertainty_m"`
}

// At builds a Location from a site-frame point.
func At(p geom.Vec3, uncertaintyM float64) Location {
	return Location{PositionM: p.Array(), UncertaintyM: uncertaintyM}
}

// Enclose returns the Location of a planar footprint at the given elevation:
// the centre of the points' bounding box, with the distance to the farthest
// point as the radius. With no points it is the origin with zero radius.
func Enclose(elevation float64, pts ...geom.Vec2) Location {
	if len(pts) == 0 {
		return Location{PositionM: [3]float64{0, elevation, 0}}
	}
	lo, hi := pts[0], pts[0]
	for _, p := range pts[1:] {
		lo = geom.Vec2{X: math.Min(lo.X, p.X), Z: math.Min(lo.Z, p.Z)}
		hi = geom.Vec2{X: math.Max(hi.X, p.X), Z: math.Max(hi.Z, p.Z)}
	}
	c := lo.Lerp(hi, 0.5)
	r := 0.0
	for _, p := range pts {
		r = math.Max(r, c.Dist(p))
	}
	return Location{PositionM: [3]float64{c.X, elevation, c.Z}, UncertaintyM: r}
}

// Point is the position as a vector.
func (l Location) Point() geom.Vec3 { return geom.Vec3FromArray(l.PositionM) }

func (l Location) check() error {
	if !coordinates(l.PositionM[:]...) || !coordinates(l.UncertaintyM) || l.UncertaintyM < 0 {
		return invalid("position_m or uncertainty_m")
	}
	return nil
}

// covers reports whether the uncertainty radius reaches every point.
func (l Location) covers(pts ...geom.Vec2) bool {
	c := l.Point().XZ()
	for _, p := range pts {
		if c.Dist(p) > l.UncertaintyM+1e-6 {
			return false
		}
	}
	return true
}

// covariance reports whether c is a finite, symmetric, positive semi-definite
// n x n matrix: no eigenvalue below −1e-9 of the largest. Zero is allowed.
func covariance(c []float64, n int) bool {
	if !finite(c...) {
		return false
	}
	sym := geom.NewMat(n, n)
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			if math.Abs(c[i*n+j]-c[j*n+i]) > 1e-9*(1+math.Abs(c[i*n+j])) {
				return false
			}
			v := (c[i*n+j] + c[j*n+i]) / 2
			sym.Set(i, j, v)
			sym.Set(j, i, v)
		}
	}
	values, _, err := geom.SymEigen(sym)
	return err == nil && values[n-1] >= -1e-9*math.Max(values[0], 0)
}

func planarPose(p geom.Pose2) bool { return coordinates(p.X, p.Z) && finite(p.Yaw) }

// interval reports whether first is set and last does not precede it.
func interval(first, last time.Time) bool { return !first.IsZero() && !last.Before(first) }

// CalibrationState is the lifecycle of a calibration revision.
type CalibrationState string

const (
	CalibrationEstimating  CalibrationState = "estimating"  // not yet trustworthy
	CalibrationValid       CalibrationState = "valid"       // in use
	CalibrationStale       CalibrationState = "stale"       // superseded or aged out
	CalibrationQuarantined CalibrationState = "quarantined" // the sensor moved; its output must not be used
)

// ParentKind names the frame a sensor's extrinsic is expressed in.
type ParentKind string

const (
	ParentSite       ParentKind = "site"        // a fixed sensor, placed in the site frame
	ParentDeviceBase ParentKind = "device_base" // a mounted sensor, placed in its carrier's base frame
)

// ResidualStats summarises the fit residuals of an estimate.
type ResidualStats struct {
	Count  int     `json:"count"`
	RMSE   float64 `json:"rmse"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

func (r ResidualStats) check() error {
	if r.Count < 0 || !finite(r.RMSE, r.Median, r.Max) || r.RMSE < 0 || r.Median < 0 || r.Max < 0 {
		return invalid("residual statistics")
	}
	return nil
}

// Calibration is one revision of a sensor's intrinsic and extrinsic
// calibration. The component entity is the sensor, so a new revision replaces
// the previous one; Revision names it for records that depend on it.
type Calibration struct {
	Device     DeviceID         `json:"device"`
	Sensor     SensorID         `json:"sensor"`
	Kind       SensorKind       `json:"kind"`
	Intrinsics *geom.Intrinsics `json:"intrinsics,omitempty"` // cameras only
	// SensorToParent maps sensor coordinates to the Parent frame. For a camera
	// the sensor frame is the optical frame.
	SensorToParent geom.Pose3 `json:"sensor_to_parent"`
	Parent         ParentKind `json:"parent"`
	// ParentDevice is the carrier whose base frame is the parent; required for
	// ParentDeviceBase and empty for ParentSite.
	ParentDevice      DeviceID         `json:"parent_device,omitempty"`
	TranslationSigmaM float64          `json:"translation_sigma_m"`
	RotationSigmaRad  float64          `json:"rotation_sigma_rad"`
	ReprojectionRMSE  float64          `json:"reprojection_rmse_px"` // cameras; 0 otherwise
	Residuals         ResidualStats    `json:"residuals"`
	ResidualUnit      string           `json:"residual_unit"` // unit of Residuals, "px" or "m"
	Method            string           `json:"method"`
	Revision          string           `json:"revision"`
	State             CalibrationState `json:"state"`
	// TimeOffset is the sensor clock minus site time: subtract it from a
	// measurement's timestamp to get site time.
	TimeOffset      time.Duration `json:"time_offset_ns"`
	TimeOffsetSigma time.Duration `json:"time_offset_sigma_ns"`
	// Location is the sensor's site position; present exactly when Parent is
	// ParentSite, and then equal to SensorToParent.T.
	*Location
}

func (Calibration) Type() string { return TypeCalibration }
func (c Calibration) EntityKey() string {
	return "sensor/" + string(c.Device) + "/" + string(c.Sensor)
}

func (c Calibration) Validate() error {
	if !validID(string(c.Device)) || !validID(string(c.Sensor)) || !c.Kind.Valid() {
		return invalid("calibration identity")
	}
	if c.Intrinsics != nil {
		if c.Kind != KindCamera {
			return invalid("calibration: only cameras carry intrinsics")
		}
		if err := c.Intrinsics.Validate(); err != nil {
			return err
		}
	} else if c.Kind == KindCamera && c.State == CalibrationValid {
		return invalid("calibration: a valid camera calibration needs intrinsics")
	}
	t := c.SensorToParent.T
	if !c.SensorToParent.R.IsRotation(1e-6) || !coordinates(t.X, t.Y, t.Z) {
		return invalid("calibration extrinsic")
	}
	switch c.Parent {
	case ParentSite:
		if c.ParentDevice != "" || c.Location == nil {
			return invalid("calibration: a site-fixed sensor needs a location and no parent device")
		}
		if err := c.Location.check(); err != nil {
			return err
		}
		if c.Location.Point().Dist(t) > 1e-6 {
			return invalid("calibration: position_m disagrees with the extrinsic")
		}
	case ParentDeviceBase:
		if !validID(string(c.ParentDevice)) || c.Location != nil {
			return invalid("calibration: a mounted sensor needs a parent device and no site location")
		}
	default:
		return invalid("calibration parent")
	}
	if !finite(c.TranslationSigmaM, c.RotationSigmaRad, c.ReprojectionRMSE) || c.TranslationSigmaM < 0 || c.RotationSigmaRad < 0 || c.ReprojectionRMSE < 0 || c.TimeOffsetSigma < 0 {
		return invalid("calibration uncertainty")
	}
	if err := c.Residuals.check(); err != nil {
		return err
	}
	switch c.State {
	case CalibrationEstimating, CalibrationValid, CalibrationStale, CalibrationQuarantined:
	default:
		return invalid("calibration state")
	}
	if c.Method == "" || !validText(c.Method) || !validText(c.ResidualUnit) || !validID(c.Revision) {
		return invalid("calibration method or revision")
	}
	return nil
}

// Entities projects a site-fixed sensor; a mounted sensor has no site place
// of its own.
func (c Calibration) Entities() []Entity {
	if c.Location == nil {
		return nil
	}
	e := Entity{
		Kind: EntitySensor, ID: string(c.Device) + "/" + string(c.Sensor), Position: c.Location.Point(),
		Class: string(c.Kind), UncertaintyM: c.UncertaintyM, Source: c.Device,
		Attributes: map[string]string{"state": string(c.State), "revision": c.Revision, "method": c.Method},
	}
	if c.State == CalibrationValid {
		e.Confidence = 1
	}
	// The heading is where the sensor points: the optical axis (+Z) for a
	// camera, +X for everything else. A sensor aimed straight up or down has none.
	axis := geom.Vec3{X: 1}
	if c.Kind == KindCamera {
		axis = geom.Vec3{Z: 1}
	}
	if d := c.SensorToParent.Rotate(axis).XZ(); d.Norm() > 1e-9 {
		e.HasYaw, e.Yaw = true, d.Angle()
	}
	return []Entity{e}
}

// DevicePose is a device base pose estimate. PositionM is (x, elevation, z)
// in the frame named FrameID, which must be the ID of the component's frame:
// the site frame for a localized device, or a device-local map frame before
// alignment (such a component is not located by site-frame region queries;
// FrameAlignment places it). The entity key does not include the frame, so
// one source holds one pose per device: a publisher of both a local-frame and
// a site-frame pose uses two Source.Stream values.
type DevicePose struct {
	Device  DeviceID `json:"device"`
	Floor   FloorID  `json:"floor"`
	FrameID string   `json:"frame_id"`
	Location
	Yaw        float64   `json:"yaw"`
	Covariance geom.Mat3 `json:"covariance"` // over (x, z, yaw), row-major
	Method     string    `json:"method"`
	Score      float64   `json:"score"` // match quality in [0, 1]
}

func (DevicePose) Type() string { return TypeDevicePose }
func (p DevicePose) EntityKey() string {
	return "device/" + string(p.Device)
}

// Pose2 is the planar pose; Elevation is PositionM[1].
func (p DevicePose) Pose2() geom.Pose2 {
	return geom.Pose2{X: p.PositionM[0], Z: p.PositionM[2], Yaw: p.Yaw}
}

func (p DevicePose) componentFrame() string { return p.FrameID }

func (p DevicePose) Validate() error {
	if !validID(string(p.Device)) || !validID(string(p.Floor)) || p.FrameID == "" || !validText(p.FrameID) || p.Method == "" || !validText(p.Method) {
		return invalid("device pose identity")
	}
	if err := p.Location.check(); err != nil {
		return err
	}
	if !finite(p.Yaw) || !covariance(p.Covariance[:], 3) || !unit(p.Score) {
		return invalid("device pose estimate")
	}
	return nil
}

func (p DevicePose) Entities() []Entity {
	return []Entity{{
		Kind: EntityDevice, ID: string(p.Device), Floor: p.Floor, Position: p.Point(), HasYaw: true, Yaw: p.Yaw,
		Confidence: p.Score, UncertaintyM: p.UncertaintyM, Source: p.Device,
		Attributes: map[string]string{"frame_id": p.FrameID, "method": p.Method},
	}}
}

// RegistrationState is the automatic sensor registration state machine.
type RegistrationState string

const (
	RegistrationDiscovered  RegistrationState = "discovered"  // manifest seen, nothing known
	RegistrationCollecting  RegistrationState = "collecting"  // gathering constraining observations
	RegistrationEstimating  RegistrationState = "estimating"  // solving extrinsics and clock offset
	RegistrationRegistered  RegistrationState = "registered"  // a calibration revision is in use
	RegistrationQuarantined RegistrationState = "quarantined" // output withheld pending recalibration
	RegistrationFailed      RegistrationState = "failed"      // the data could not constrain the sensor
)

// SensorRegistration is the registration status of one announced sensor. It
// projects no entity: a placed sensor is the entity of its Calibration.
type SensorRegistration struct {
	Manifest SensorManifest    `json:"manifest"`
	State    RegistrationState `json:"state"`
	// CalibrationRevision is the Calibration.Revision in use; required once
	// registered or quarantined.
	CalibrationRevision string `json:"calibration_revision,omitempty"`
	Detail              string `json:"detail,omitempty"`
	// Location is the sensor's site position once a fixed sensor is registered.
	*Location
}

func (SensorRegistration) Type() string { return TypeSensorRegistration }
func (r SensorRegistration) EntityKey() string {
	return "sensor/" + string(r.Manifest.Device) + "/" + string(r.Manifest.Sensor)
}

func (r SensorRegistration) Validate() error {
	if err := r.Manifest.Validate(); err != nil {
		return err
	}
	switch r.State {
	case RegistrationRegistered, RegistrationQuarantined:
		if !validID(r.CalibrationRevision) {
			return invalid("sensor registration: a registered sensor names its calibration revision")
		}
	case RegistrationDiscovered, RegistrationCollecting, RegistrationEstimating, RegistrationFailed:
		if r.CalibrationRevision != "" && !validID(r.CalibrationRevision) {
			return invalid("sensor registration revision")
		}
	default:
		return invalid("sensor registration state")
	}
	if !validText(r.Detail) {
		return invalid("sensor registration detail")
	}
	if r.Location != nil {
		return r.Location.check()
	}
	return nil
}

// KeyframeRef binds an anchor to a map keyframe. Offset is the anchor's pose
// in the keyframe's base frame, so the anchor follows the keyframe when the
// pose graph is corrected.
type KeyframeRef struct {
	Device   DeviceID   `json:"device"`
	Sequence uint64     `json:"sequence"`
	Offset   geom.Pose2 `json:"offset"`
}

const (
	MaxSignatureBytes = 4096
	MaxAnchorBindings = 32
)

// Anchor is a persistent named place.
type Anchor struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Floor FloorID `json:"floor"`
	Location
	Yaw float64 `json:"yaw"`
	// Signature is the anchors package's encoding of the local geometry around
	// the anchor, used to re-resolve it. Opaque here; base64 in JSON.
	Signature []byte        `json:"signature,omitempty"`
	Keyframes []KeyframeRef `json:"keyframes,omitempty"`
	Revision  string        `json:"revision"`
}

func (Anchor) Type() string        { return TypeAnchor }
func (a Anchor) EntityKey() string { return "anchor/" + a.ID }

func (a Anchor) Pose2() geom.Pose2 {
	return geom.Pose2{X: a.PositionM[0], Z: a.PositionM[2], Yaw: a.Yaw}
}

func (a Anchor) Validate() error {
	if !validID(a.ID) || !validText(a.Name) || !validID(string(a.Floor)) || !validID(a.Revision) || !finite(a.Yaw) ||
		len(a.Signature) > MaxSignatureBytes || len(a.Keyframes) > MaxAnchorBindings {
		return invalid("anchor")
	}
	for _, k := range a.Keyframes {
		if !validID(string(k.Device)) || !planarPose(k.Offset) {
			return invalid("anchor keyframe binding")
		}
	}
	return a.Location.check()
}

func (a Anchor) Entities() []Entity {
	return []Entity{{
		Kind: EntityAnchor, ID: a.ID, Floor: a.Floor, Position: a.Point(), HasYaw: true, Yaw: a.Yaw,
		Label: a.Name, Confidence: 1, UncertaintyM: a.UncertaintyM,
		Attributes: map[string]string{"revision": a.Revision},
	}}
}

// TrackState is the lifecycle of a tracked object.
type TrackState string

const (
	TrackTentative TrackState = "tentative" // not yet confirmed by enough observations
	TrackConfirmed TrackState = "confirmed"
	TrackCoasting  TrackState = "coasting" // predicted without a current observation
	TrackLost      TrackState = "lost"     // ended; published once so consumers can close it
)

// SensorRef names one sensor.
type SensorRef struct {
	Device DeviceID `json:"device"`
	Sensor SensorID `json:"sensor"`
}

// MaxTrackSensors bounds Track.Sensors.
const MaxTrackSensors = 32

// Track is a moving object estimate. IDs are anonymous and local to the
// tracker; they are not identities of people.
type Track struct {
	ID              string  `json:"id"`
	Class           string  `json:"class,omitempty"`
	ClassConfidence float64 `json:"class_confidence"`
	Floor           FloorID `json:"floor"`
	Location
	VelocityMPS geom.Vec2   `json:"velocity_mps"`
	Covariance  [16]float64 `json:"covariance"` // over (x, z, vx, vz), row-major
	State       TrackState  `json:"state"`
	FirstSeen   time.Time   `json:"first_seen"`
	LastSeen    time.Time   `json:"last_seen"` // last supporting observation, not the prediction time
	Sensors     []SensorRef `json:"sensors,omitempty"`
	RadiusM     float64     `json:"radius_m,omitempty"`
}

func (Track) Type() string        { return TypeTrack }
func (t Track) EntityKey() string { return "track/" + t.ID }

// PositionCov is the (x, z) block of Covariance.
func (t Track) PositionCov() geom.Cov2 {
	return geom.Cov2{t.Covariance[0], t.Covariance[1], t.Covariance[4], t.Covariance[5]}
}

func (t Track) Validate() error {
	if !validID(t.ID) || (t.Class != "" && !validID(t.Class)) || !unit(t.ClassConfidence) || !validID(string(t.Floor)) {
		return invalid("track identity")
	}
	if err := t.Location.check(); err != nil {
		return err
	}
	if !finite(t.VelocityMPS.X, t.VelocityMPS.Z, t.RadiusM) || t.RadiusM < 0 || !covariance(t.Covariance[:], 4) || !interval(t.FirstSeen, t.LastSeen) || len(t.Sensors) > MaxTrackSensors {
		return invalid("track estimate")
	}
	switch t.State {
	case TrackTentative, TrackConfirmed, TrackCoasting, TrackLost:
	default:
		return invalid("track state")
	}
	for _, s := range t.Sensors {
		if !validID(string(s.Device)) || !validID(string(s.Sensor)) {
			return invalid("track sensor")
		}
	}
	return nil
}

func (t Track) Entities() []Entity {
	return []Entity{{
		Kind: EntityTrack, ID: t.ID, Floor: t.Floor, Position: t.Point(), Extent: Extent{RadiusM: t.RadiusM},
		Class: t.Class, Confidence: t.ClassConfidence, UncertaintyM: t.UncertaintyM, ObservedAt: t.LastSeen,
		Attributes: map[string]string{
			"state": string(t.State),
			"vx":    strconv.FormatFloat(t.VelocityMPS.X, 'g', -1, 64),
			"vz":    strconv.FormatFloat(t.VelocityMPS.Z, 'g', -1, 64),
		},
	}}
}

const (
	// MaxTileSide bounds MapTile.Side. A full tile is 16 KiB of cells, about
	// 22 KiB as base64, well inside MaxPayloadBytes.
	MaxTileSide = 128
	// MaxTileSources bounds MapTile.Versions.
	MaxTileSources = 64
)

// TileOrigin is the minimum corner, in site (x, z) metres, of the tile with
// index (ix, iz): tiles of side cells of cellSizeM metres partition the plane
// from the site origin, so cell (cx, cz) of a floor's map lies in tile
// (floor(cx/side), floor(cz/side)). Every tile payload must carry exactly
// this origin; publishers that compute it here produce equal digests.
func TileOrigin(ix, iz int32, side int, cellSizeM float64) [2]float64 {
	span := float64(side) * cellSizeM
	return [2]float64{float64(ix) * span, float64(iz) * span}
}

// tileGeometry checks the shape of a tile and that it sits on the tiling.
func tileGeometry(ix, iz int32, side int, cellSizeM float64, origin [2]float64, cells int) bool {
	if side < 1 || side > MaxTileSide || cells != side*side || !finite(cellSizeM) || cellSizeM <= 0 || !coordinates(origin[:]...) {
		return false
	}
	want := TileOrigin(ix, iz, side, cellSizeM)
	return math.Abs(origin[0]-want[0]) <= 1e-6*cellSizeM && math.Abs(origin[1]-want[1]) <= 1e-6*cellSizeM
}

func tileCorners(origin [2]float64, side int, cellSizeM float64) []geom.Vec2 {
	span := float64(side) * cellSizeM
	o := geom.Vec2{X: origin[0], Z: origin[1]}
	return []geom.Vec2{o, {X: o.X + span, Z: o.Z}, {X: o.X + span, Z: o.Z + span}, {X: o.X, Z: o.Z + span}}
}

// MapTile is one square tile of a floor's occupancy map.
//
// The tile with index (IX, IZ) covers Side x Side cells of CellSizeM metres
// starting at OriginXZ, its minimum corner in the site frame, which must be
// TileOrigin(IX, IZ, Side, CellSizeM). Cells holds one
// signed byte per cell, row-major with index iz·Side+ix; int8(Cells[i]) times
// LogOddsScale is the cell's log-odds of being occupied, 0 meaning unknown.
// Versions is the per-source version vector of the evidence merged into the
// tile. Digest is ContentDigest; Seal fills it together with Location.
type MapTile struct {
	Floor        FloorID           `json:"floor"`
	IX           int32             `json:"ix"`
	IZ           int32             `json:"iz"`
	Side         int               `json:"side"`
	CellSizeM    float64           `json:"cell_size_m"`
	OriginXZ     [2]float64        `json:"origin_xz"`
	LogOddsScale float64           `json:"log_odds_scale"`
	Cells        []byte            `json:"cells"` // base64 in JSON
	Versions     map[string]uint64 `json:"versions"`
	Digest       string            `json:"digest"`
	Location
}

func (MapTile) Type() string { return TypeMapTile }
func (t MapTile) EntityKey() string {
	return fmt.Sprintf("tile/%s/%d/%d", t.Floor, t.IX, t.IZ)
}

// ContentDigest is the hex SHA-256 of every field except Digest and Location,
// in canonical JSON. Two tiles with equal content have equal digests whatever
// order their evidence arrived in.
func (t MapTile) ContentDigest() string {
	t.Digest, t.Location = "", Location{}
	raw, err := json.Marshal(t)
	if err != nil { // only non-finite floats, which Validate rejects
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (t MapTile) corners() []geom.Vec2 { return tileCorners(t.OriginXZ, t.Side, t.CellSizeM) }

// Seal sets Location to the tile centre at the floor elevation with the
// half-diagonal as radius, and Digest to ContentDigest. Call it last.
func (t *MapTile) Seal(elevation float64) {
	t.Location = Enclose(elevation, t.corners()...)
	t.Digest = t.ContentDigest()
}

func (t MapTile) Validate() error {
	if !validID(string(t.Floor)) || !tileGeometry(t.IX, t.IZ, t.Side, t.CellSizeM, t.OriginXZ, len(t.Cells)) || !finite(t.LogOddsScale) || t.LogOddsScale <= 0 {
		return invalid("map tile geometry")
	}
	if len(t.Versions) == 0 || len(t.Versions) > MaxTileSources {
		return invalid("map tile version vector")
	}
	for source, v := range t.Versions {
		if !validID(source) || v == 0 {
			return invalid("map tile source version")
		}
	}
	if err := t.Location.check(); err != nil {
		return err
	}
	if !t.Location.covers(t.corners()...) {
		return invalid("map tile: uncertainty_m does not cover the tile")
	}
	if t.Digest != t.ContentDigest() {
		return invalid("map tile digest")
	}
	return nil
}

const (
	MaxRegionDoors   = 64
	MaxRegionClasses = 16
)

// Region is one semantic region of a floor, such as a room or a corridor.
// Two regions that list the same door id are adjacent through that door.
type Region struct {
	ID              string         `json:"id"`
	Label           string         `json:"label,omitempty"` // "kitchen", "corridor", ...; empty when unknown
	LabelConfidence float64        `json:"label_confidence"`
	Floor           FloorID        `json:"floor"`
	Polygon         []geom.Vec2    `json:"polygon"` // site (x, z) boundary
	AreaM2          float64        `json:"area_m2"`
	CellCounts      map[string]int `json:"cell_counts,omitempty"` // map cells per cell class
	Doors           []string       `json:"doors,omitempty"`       // Portal ids on the boundary
	Location                       // must cover the polygon; see Enclose
}

func (Region) Type() string        { return TypeRegion }
func (r Region) EntityKey() string { return "region/" + r.ID }

func (r Region) Validate() error {
	if !validID(r.ID) || !validText(r.Label) || !unit(r.LabelConfidence) || !validID(string(r.Floor)) || !finite(r.AreaM2) || r.AreaM2 < 0 ||
		len(r.CellCounts) > MaxRegionClasses || len(r.Doors) > MaxRegionDoors {
		return invalid("region")
	}
	if err := checkPolygon(r.Polygon, false); err != nil {
		return err
	}
	for class, n := range r.CellCounts {
		if !validID(class) || n < 0 {
			return invalid("region cell class")
		}
	}
	for _, d := range r.Doors {
		if !validID(d) {
			return invalid("region door")
		}
	}
	if err := r.Location.check(); err != nil {
		return err
	}
	if !r.Location.covers(r.Polygon...) {
		return invalid("region: uncertainty_m does not cover the polygon")
	}
	return nil
}

func (r Region) Entities() []Entity {
	return []Entity{{
		Kind: EntityRegion, ID: r.ID, Floor: r.Floor, Position: r.Point(), Extent: Extent{Polygon: r.Polygon},
		Class: "region", Label: r.Label, Confidence: r.LabelConfidence, UncertaintyM: r.UncertaintyM,
	}}
}

// ChangeKind classifies an environmental change.
type ChangeKind string

const (
	ChangeAppeared    ChangeKind = "appeared"
	ChangeDisappeared ChangeKind = "disappeared"
	ChangeMoved       ChangeKind = "moved"
	ChangeDoorOpened  ChangeKind = "door_opened"
	ChangeDoorClosed  ChangeKind = "door_closed"
)

// ChangeEvent is one detected change of the static environment. PositionM is
// where the change is now: the new place for ChangeMoved, the vacated place
// for ChangeDisappeared.
type ChangeEvent struct {
	ID    string     `json:"id"`
	Kind  ChangeKind `json:"kind"`
	Floor FloorID    `json:"floor"`
	Location
	Extent Extent `json:"extent"`
	// Subject is what changed when known: a Portal id for door events, an
	// object class or id otherwise.
	Subject string `json:"subject,omitempty"`
	// FromM and ToM are the old and new site positions; both are required for
	// ChangeMoved and absent otherwise.
	FromM         *[3]float64 `json:"from_m,omitempty"`
	ToM           *[3]float64 `json:"to_m,omitempty"`
	Confidence    float64     `json:"confidence"`
	FirstObserved time.Time   `json:"first_observed"`
	LastObserved  time.Time   `json:"last_observed"`
}

func (ChangeEvent) Type() string        { return TypeChangeEvent }
func (c ChangeEvent) EntityKey() string { return "change/" + c.ID }

func (c ChangeEvent) Validate() error {
	if !validID(c.ID) || !validID(string(c.Floor)) || !validText(c.Subject) || !unit(c.Confidence) || !interval(c.FirstObserved, c.LastObserved) {
		return invalid("change event")
	}
	switch c.Kind {
	case ChangeMoved:
		if c.FromM == nil || c.ToM == nil || !coordinates(c.FromM[:]...) || !coordinates(c.ToM[:]...) {
			return invalid("change event: a move needs from_m and to_m")
		}
	case ChangeAppeared, ChangeDisappeared, ChangeDoorOpened, ChangeDoorClosed:
		if c.FromM != nil || c.ToM != nil {
			return invalid("change event: only a move carries from_m and to_m")
		}
	default:
		return invalid("change kind")
	}
	if err := c.Extent.check(); err != nil {
		return err
	}
	return c.Location.check()
}

func (c ChangeEvent) Entities() []Entity {
	return []Entity{{
		Kind: EntityChange, ID: c.ID, Floor: c.Floor, Position: c.Point(), Extent: c.Extent,
		Class: string(c.Kind), Label: c.Subject, Confidence: c.Confidence, UncertaintyM: c.UncertaintyM, ObservedAt: c.LastObserved,
	}}
}

// PortalKind is the kind of passage a portal is.
type PortalKind string

const (
	PortalDoor    PortalKind = "door"
	PortalOpening PortalKind = "opening" // a doorless gap between regions
	PortalStairs  PortalKind = "stairs"
	PortalLift    PortalKind = "lift"
	PortalRamp    PortalKind = "ramp"
)

// PortalState is whether a portal can currently be passed.
type PortalState string

const (
	PortalOpen         PortalState = "open"
	PortalClosed       PortalState = "closed"
	PortalStateUnknown PortalState = "unknown"
)

// PortalEnd is one side of a portal.
type PortalEnd struct {
	Floor     FloorID    `json:"floor"`
	Region    string     `json:"region,omitempty"` // Region id on this side, when known
	PositionM [3]float64 `json:"position_m"`
}

// Portal joins two places: two regions of one floor (a door, where both ends
// share a position) or two floors (stairs, a lift). Location must cover both
// ends; Enclose over the two end positions does.
type Portal struct {
	ID         string      `json:"id"`
	Kind       PortalKind  `json:"kind"`
	State      PortalState `json:"state"`
	A          PortalEnd   `json:"a"`
	B          PortalEnd   `json:"b"`
	WidthM     float64     `json:"width_m,omitempty"`
	Confidence float64     `json:"confidence"`
	Location
}

func (Portal) Type() string        { return TypePortal }
func (p Portal) EntityKey() string { return "portal/" + p.ID }

func (p Portal) Validate() error {
	if !validID(p.ID) || !finite(p.WidthM) || p.WidthM < 0 || !unit(p.Confidence) {
		return invalid("portal")
	}
	switch p.Kind {
	case PortalDoor, PortalOpening, PortalStairs, PortalLift, PortalRamp:
	default:
		return invalid("portal kind")
	}
	switch p.State {
	case PortalOpen, PortalClosed, PortalStateUnknown:
	default:
		return invalid("portal state")
	}
	for _, end := range []PortalEnd{p.A, p.B} {
		if !validID(string(end.Floor)) || (end.Region != "" && !validID(end.Region)) || !coordinates(end.PositionM[:]...) {
			return invalid("portal end")
		}
	}
	if err := p.Location.check(); err != nil {
		return err
	}
	if !p.Location.covers(geom.Vec3FromArray(p.A.PositionM).XZ(), geom.Vec3FromArray(p.B.PositionM).XZ()) {
		return invalid("portal: uncertainty_m does not cover both ends")
	}
	return nil
}

func (p Portal) Entities() []Entity {
	attrs := map[string]string{"state": string(p.State), "floor_a": string(p.A.Floor), "floor_b": string(p.B.Floor)}
	if p.A.Region != "" {
		attrs["region_a"] = p.A.Region
	}
	if p.B.Region != "" {
		attrs["region_b"] = p.B.Region
	}
	return []Entity{{
		Kind: EntityPortal, ID: p.ID, Floor: p.A.Floor, Position: p.Point(), Extent: Extent{RadiusM: p.WidthM / 2},
		Class: string(p.Kind), Confidence: p.Confidence, UncertaintyM: p.UncertaintyM, Attributes: attrs,
	}}
}

// MaxFloorPortals bounds FloorInfo.Portals.
const MaxFloorPortals = 64

// FloorInfo describes one floor: its elevation and the portals that touch
// it. The portals are repeated here for routing; as entities they are
// projected by their own Portal components, so FloorInfo projects none.
type FloorInfo struct {
	Floor      FloorID  `json:"floor"`
	Name       string   `json:"name,omitempty"`
	ElevationM float64  `json:"elevation_m"`        // World Y of the floor surface
	HeightM    float64  `json:"height_m,omitempty"` // floor to ceiling, 0 when unknown
	Outdoor    bool     `json:"outdoor,omitempty"`  // open sky: GNSS may be available
	Portals    []Portal `json:"portals,omitempty"`
	// Location is the mapped extent of the floor, when known.
	*Location
}

func (FloorInfo) Type() string        { return TypeFloorInfo }
func (f FloorInfo) EntityKey() string { return "floor/" + string(f.Floor) }

func (f FloorInfo) Validate() error {
	if !validID(string(f.Floor)) || !validText(f.Name) || !coordinates(f.ElevationM) || !finite(f.HeightM) || f.HeightM < 0 || len(f.Portals) > MaxFloorPortals {
		return invalid("floor")
	}
	for _, p := range f.Portals {
		if err := p.Validate(); err != nil {
			return err
		}
		if p.A.Floor != f.Floor && p.B.Floor != f.Floor {
			return invalid("floor: portal does not touch this floor")
		}
	}
	if f.Location != nil {
		return f.Location.check()
	}
	return nil
}

// GeoReference ties the site frame to the Earth. Origin is the geographic
// coordinate of the site-frame point PositionM (normally the site origin).
// SiteYaw is the yaw of the site frame within the local tangent frame of
// geography.Tangent at Origin (x east, y up, z south): site +X points along
// (cos SiteYaw, sin SiteYaw) in (east, south), so its compass bearing is
// π/2 + SiteYaw and site +Z lies a further quarter turn clockwise. A site
// point s maps to tangent coordinates SiteToTangent().Apply(s.XZ()).
// UncertaintyM is the horizontal uncertainty of the tie.
type GeoReference struct {
	Origin      world.GeographicAnchor `json:"origin"`
	SiteYaw     float64                `json:"site_yaw_rad"`
	YawSigmaRad float64                `json:"yaw_sigma_rad"`
	Method      string                 `json:"method"`
	Fixes       int                    `json:"fixes"` // GNSS fixes used
	Location
}

func (GeoReference) Type() string      { return TypeGeoReference }
func (GeoReference) EntityKey() string { return "georeference" }

// SiteToTangent is the planar transform from site (x, z) to tangent
// (east, south) metres about Origin.
func (g GeoReference) SiteToTangent() geom.Pose2 {
	t := geom.Vec2{X: -g.PositionM[0], Z: -g.PositionM[2]}.Rotate(g.SiteYaw)
	return geom.Pose2{X: t.X, Z: t.Z, Yaw: g.SiteYaw}
}

func (g GeoReference) Validate() error {
	if err := g.Origin.Validate(); err != nil {
		return err
	}
	if !finite(g.SiteYaw, g.YawSigmaRad) || g.YawSigmaRad < 0 || g.Method == "" || !validText(g.Method) || g.Fixes < 0 {
		return invalid("geo reference")
	}
	return g.Location.check()
}

// MaxKeyframeBeams bounds CompactScan: any valid Scan can be compacted. A
// full keyframe is about 25 KiB of JSON, inside MaxPayloadBytes.
const MaxKeyframeBeams = MaxScanBeams

// Range codes of CompactScan.RangesMM; every other value is millimetres.
const (
	CompactInvalid  uint16 = 0
	CompactNoReturn uint16 = 65535
)

// compactMaxM is the longest range a CompactScan can hold as a hit.
const compactMaxM = 65.534

// CompactScan is a Scan quantised to millimetres for exchange between
// devices. It is in the sensor frame, like Scan.
type CompactScan struct {
	AngleMin       float64  `json:"angle_min"`
	AngleIncrement float64  `json:"angle_increment"`
	RangeMaxM      float64  `json:"range_max_m"`
	RangesMM       []uint16 `json:"ranges_mm"`
}

// Compact quantises a scan. Hits beyond 65.534 m become no-return. A hit
// stays a hit: one that would round up past RangeMaxM is encoded as the last
// whole millimetre inside it. It fails when the scan is invalid.
func Compact(s Scan) (CompactScan, error) {
	if err := s.Validate(); err != nil {
		return CompactScan{}, err
	}
	c := CompactScan{AngleMin: s.AngleMin, AngleIncrement: s.AngleIncrement, RangeMaxM: math.Min(s.RangeMax, compactMaxM), RangesMM: make([]uint16, len(s.Ranges))}
	lastMM := math.Floor(c.RangeMaxM * 1000)
	for i := range s.Ranges {
		switch _, r, state := s.Beam(i); {
		case state == BeamInvalid:
			c.RangesMM[i] = CompactInvalid
		case state == BeamNoReturn || r > compactMaxM:
			c.RangesMM[i] = CompactNoReturn
		default:
			c.RangesMM[i] = uint16(math.Max(1, math.Min(math.Round(r*1000), lastMM)))
		}
	}
	return c, nil
}

// Expand rebuilds a Scan under the given header. RangeSigma is the caller's
// to set: the quantisation itself adds at most 1 mm. RangeMin is not carried
// and comes back as 0; returns closer than it were already invalid.
func (c CompactScan) Expand(h Header, rangeSigma float64) Scan {
	s := Scan{Header: h, AngleMin: c.AngleMin, AngleIncrement: c.AngleIncrement, RangeMax: c.RangeMaxM, RangeSigma: rangeSigma, Ranges: make([]float64, len(c.RangesMM))}
	for i, mm := range c.RangesMM {
		switch mm {
		case CompactInvalid:
			s.Ranges[i] = RangeInvalid
		case CompactNoReturn:
			s.Ranges[i] = RangeNoReturn
		default:
			s.Ranges[i] = float64(mm) / 1000
		}
	}
	return s
}

func (c CompactScan) check() error {
	if len(c.RangesMM) == 0 || len(c.RangesMM) > MaxKeyframeBeams || !finite(c.AngleMin, c.AngleIncrement, c.RangeMaxM) || c.AngleIncrement == 0 || c.RangeMaxM <= 0 || c.RangeMaxM > compactMaxM {
		return invalid("compact scan")
	}
	return nil
}

// Keyframe is the unit of map exchange for collaborative SLAM: one device's
// scan with the pose it was taken from, in that device's own map frame.
type Keyframe struct {
	Device   DeviceID  `json:"device"`
	Sequence uint64    `json:"sequence"`
	Floor    FloorID   `json:"floor,omitempty"`
	Time     time.Time `json:"time"` // site time of the scan
	// Local is the device base pose in the device's own map frame, whose
	// relation to the site frame is what collaborative SLAM estimates.
	Local      geom.Pose2 `json:"local"`
	Covariance geom.Mat3  `json:"covariance"` // of Local over (x, z, yaw)
	// SensorToBase places the scanner in the base frame, so a receiver can use
	// the scan without the sender's calibration record.
	SensorToBase geom.Pose2  `json:"sensor_to_base"`
	Scan         CompactScan `json:"scan"`
}

func (Keyframe) Type() string { return TypeKeyframe }
func (k Keyframe) EntityKey() string {
	return "keyframe/" + string(k.Device) + "/" + strconv.FormatUint(k.Sequence, 10)
}

func (k Keyframe) Validate() error {
	if !validID(string(k.Device)) || (k.Floor != "" && !validID(string(k.Floor))) || k.Time.IsZero() ||
		!planarPose(k.Local) || !planarPose(k.SensorToBase) || !covariance(k.Covariance[:], 3) {
		return invalid("keyframe")
	}
	return k.Scan.check()
}

// Landmark is a static fiducial with an estimated site pose, usable to
// localize a camera that sees it, from a single tag when the tag has several
// points. TagToSite maps the fiducial's own frame, the one its
// FiducialGeometry.Points are in, to the site frame. By convention a planar
// tag's +Z is its outward normal, which is the heading of the projected
// entity. PositionM equals TagToSite.T and UncertaintyM is its 1σ.
type Landmark struct {
	ID        string     `json:"id"` // the FiducialGeometry id
	Floor     FloorID    `json:"floor"`
	TagToSite geom.Pose3 `json:"tag_to_site"`
	Location
	RotationSigmaRad float64 `json:"rotation_sigma_rad"`
	Observations     int     `json:"observations"`
	Revision         string  `json:"revision"`
}

func (Landmark) Type() string        { return TypeLandmark }
func (l Landmark) EntityKey() string { return "landmark/" + l.ID }

func (l Landmark) Validate() error {
	if !validID(l.ID) || !validID(string(l.Floor)) || l.Observations < 0 || !validID(l.Revision) || !finite(l.RotationSigmaRad) || l.RotationSigmaRad < 0 {
		return invalid("landmark")
	}
	if err := l.Location.check(); err != nil {
		return err
	}
	if !l.TagToSite.R.IsRotation(1e-6) || l.Point().Dist(l.TagToSite.T) > 1e-6 {
		return invalid("landmark: tag_to_site is not a rigid pose at position_m")
	}
	return nil
}

func (l Landmark) Entities() []Entity {
	e := Entity{
		Kind: EntityLandmark, ID: l.ID, Floor: l.Floor, Position: l.Point(), Class: "fiducial",
		Confidence: 1, UncertaintyM: l.UncertaintyM, Attributes: map[string]string{"revision": l.Revision},
	}
	e.Yaw, e.HasYaw = l.TagToSite.ViewYaw()
	return []Entity{e}
}

// Object is a static class-labelled thing, such as a piece of furniture.
// Moving things are Tracks.
type Object struct {
	ID         string  `json:"id"`
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
	Floor      FloorID `json:"floor"`
	Location
	Extent    Extent    `json:"extent"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

func (Object) Type() string        { return TypeObject }
func (o Object) EntityKey() string { return "object/" + o.ID }

func (o Object) Validate() error {
	if !validID(o.ID) || !validID(o.Class) || !unit(o.Confidence) || !validID(string(o.Floor)) || !interval(o.FirstSeen, o.LastSeen) {
		return invalid("object")
	}
	if err := o.Extent.check(); err != nil {
		return err
	}
	return o.Location.check()
}

func (o Object) Entities() []Entity {
	return []Entity{{
		Kind: EntityObject, ID: o.ID, Floor: o.Floor, Position: o.Point(), Extent: o.Extent,
		Class: o.Class, Confidence: o.Confidence, UncertaintyM: o.UncertaintyM, ObservedAt: o.LastSeen,
	}}
}

// FrameAlignment places one planar frame in another: a device's own map
// frame (Keyframe.Local, a DevicePose published before alignment) in the site
// frame, as collaborative SLAM or localization estimates it. A pose p of the
// child frame is ChildToParent.Compose(p) in the parent frame, and a child
// elevation e is ElevationM + e. The component's frame is the parent frame.
type FrameAlignment struct {
	ChildFrameID  string     `json:"child_frame_id"`
	ParentFrameID string     `json:"parent_frame_id"`
	ChildToParent geom.Pose2 `json:"child_to_parent"`
	ElevationM    float64    `json:"elevation_m"`
	Covariance    geom.Mat3  `json:"covariance"` // of ChildToParent over (x, z, yaw)
	Inliers       int        `json:"inliers"`    // constraints between the two frames that the estimate rests on
	RMSE          float64    `json:"rmse_m"`     // of those constraints after alignment
	Revision      string     `json:"revision"`
}

func (FrameAlignment) Type() string             { return TypeFrameAlignment }
func (a FrameAlignment) EntityKey() string      { return "frame/" + a.ChildFrameID }
func (a FrameAlignment) componentFrame() string { return a.ParentFrameID }

// validFrameID accepts what validID does plus '/', as in "map/rover-1".
func validFrameID(s string) bool { return validID(strings.ReplaceAll(s, "/", "-")) }

func (a FrameAlignment) Validate() error {
	if !validFrameID(a.ChildFrameID) || !validFrameID(a.ParentFrameID) || a.ChildFrameID == a.ParentFrameID || !validID(a.Revision) {
		return invalid("frame alignment identity")
	}
	if !planarPose(a.ChildToParent) || !coordinates(a.ElevationM) || !covariance(a.Covariance[:], 3) || a.Inliers < 0 || !finite(a.RMSE) || a.RMSE < 0 {
		return invalid("frame alignment estimate")
	}
	return nil
}

// MaxLayerClasses bounds LayerTile.ClassNames: code 0 means no data.
const MaxLayerClasses = 255

// LayerTile is one square tile of a per-cell layer over a floor's map: a
// scalar field such as a gas concentration, or a class per cell such as the
// semantic cell classes. Layers tile a floor exactly as MapTile does, so a
// layer cell and the occupancy cell with the same index are the same place.
//
// Cells holds one byte per cell, row-major with index iz·Side+ix; 0 means no
// data. A class layer lists its classes in ClassNames, and code c is
// ClassNames[c−1]. A scalar layer leaves ClassNames empty, and code c is the
// value ValueOffset + (c−1)·ValueStep in Unit: a tile resolves 255 levels, so
// choose ValueStep near the field's noise and no finer than (range)/254.
// ValueSigma is the 1σ of a cell value in Unit, quantisation included.
type LayerTile struct {
	Floor       FloorID    `json:"floor"`
	Layer       string     `json:"layer"` // "gas.co2", "semantic", ...
	IX          int32      `json:"ix"`
	IZ          int32      `json:"iz"`
	Side        int        `json:"side"`
	CellSizeM   float64    `json:"cell_size_m"`
	OriginXZ    [2]float64 `json:"origin_xz"` // TileOrigin(IX, IZ, Side, CellSizeM)
	Cells       []byte     `json:"cells"`     // base64 in JSON
	ClassNames  []string   `json:"class_names,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	ValueOffset float64    `json:"value_offset,omitempty"`
	ValueStep   float64    `json:"value_step,omitempty"`
	ValueSigma  float64    `json:"value_sigma,omitempty"`
	Location
}

func (LayerTile) Type() string { return TypeLayerTile }
func (t LayerTile) EntityKey() string {
	return fmt.Sprintf("layer/%s/%s/%d/%d", t.Layer, t.Floor, t.IX, t.IZ)
}

// Seal sets Location to the tile centre at the floor elevation with the
// half-diagonal as radius.
func (t *LayerTile) Seal(elevation float64) {
	t.Location = Enclose(elevation, tileCorners(t.OriginXZ, t.Side, t.CellSizeM)...)
}

func (t LayerTile) code(ix, iz int) byte {
	if ix < 0 || iz < 0 || ix >= t.Side || iz >= t.Side || len(t.Cells) != t.Side*t.Side {
		return 0
	}
	return t.Cells[iz*t.Side+ix]
}

// Value is the scalar of cell (ix, iz) of the tile; ok is false where there
// is no data, outside the tile and on a class layer.
func (t LayerTile) Value(ix, iz int) (value float64, ok bool) {
	c := t.code(ix, iz)
	if c == 0 || len(t.ClassNames) > 0 {
		return 0, false
	}
	return t.ValueOffset + float64(c-1)*t.ValueStep, true
}

// ValueCode is the cell code of a scalar value: the nearest level, saturating
// at the ends of the tile's range. It is never 0, the no-data code.
func (t LayerTile) ValueCode(value float64) byte {
	level := math.Round((value - t.ValueOffset) / t.ValueStep)
	if !(level > 0) { // also NaN
		level = 0
	}
	return byte(1 + math.Min(level, 254))
}

// Class is the class of cell (ix, iz) of the tile; ok is false where there is
// no data, outside the tile and on a scalar layer.
func (t LayerTile) Class(ix, iz int) (class string, ok bool) {
	c := int(t.code(ix, iz))
	if c == 0 || c > len(t.ClassNames) {
		return "", false
	}
	return t.ClassNames[c-1], true
}

func (t LayerTile) Validate() error {
	if !validID(string(t.Floor)) || !validID(t.Layer) || !tileGeometry(t.IX, t.IZ, t.Side, t.CellSizeM, t.OriginXZ, len(t.Cells)) {
		return invalid("layer tile geometry")
	}
	if len(t.ClassNames) > 0 {
		if len(t.ClassNames) > MaxLayerClasses || t.Unit != "" || t.ValueOffset != 0 || t.ValueStep != 0 || t.ValueSigma != 0 {
			return invalid("layer tile: a class layer carries class names only")
		}
		for _, name := range t.ClassNames {
			if !validID(name) {
				return invalid("layer tile class name")
			}
		}
		for _, c := range t.Cells {
			if int(c) > len(t.ClassNames) {
				return invalid("layer tile: cell class without a name")
			}
		}
	} else if !validID(t.Unit) || !finite(t.ValueOffset, t.ValueStep, t.ValueSigma) || t.ValueStep <= 0 || t.ValueSigma < 0 {
		return invalid("layer tile: a scalar layer needs a unit and a positive value step")
	}
	if err := t.Location.check(); err != nil {
		return err
	}
	if !t.Location.covers(tileCorners(t.OriginXZ, t.Side, t.CellSizeM)...) {
		return invalid("layer tile: uncertainty_m does not cover the tile")
	}
	return nil
}

// Types lists every spatial component type in ascending order.
func Types() []string {
	types := []string{
		TypeCalibration, TypeDevicePose, TypeSensorRegistration, TypeAnchor, TypeTrack, TypeMapTile, TypeRegion,
		TypeChangeEvent, TypeFloorInfo, TypePortal, TypeGeoReference, TypeKeyframe, TypeLandmark, TypeObject,
		TypeFrameAlignment, TypeLayerTile,
	}
	sort.Strings(types)
	return types
}
