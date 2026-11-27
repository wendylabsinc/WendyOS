// Package spatial is the device-agnostic data model of the spatial layer:
// measurement types shared by the simulator and the real-data adapters, the
// Entity representation used by query and history, the wendy.spatial.*.v1
// payloads published as World components, and the conversion helpers between
// them. It holds types, validation and encoding only; estimation lives in the
// feature packages.
//
// Conventions follow package geom: World axes are right-handed and Y up, the
// planar map uses World X and Z with yaw from +X toward +Z, transforms map
// child to parent, lengths are metres and angles radians. A planar sensor or
// body frame has +X forward and +Z to the right, so an angle that a ROS-style
// driver reports counter-clockwise seen from above is negated on the way in:
// ScanFromROS and geom.Pose2FromROS do that conversion.
//
// Timestamps are time.Time values in UTC without a monotonic reading (see
// Stamp). A measurement carries the reporting sensor's clock, which may be
// offset from site time by Calibration.TimeOffset.
package spatial

import (
	"errors"
	"math"
	"time"
)

// DeviceID names a physical device: a robot, a fixed camera unit, a beacon.
type DeviceID string

// SensorID names one sensor on a device. It is unique within its device.
type SensorID string

// FloorID names one storey or outdoor level of the site.
type FloorID string

// SensorKind is the modality of a sensor.
type SensorKind string

const (
	KindLidar2D  SensorKind = "lidar2d"
	KindCamera   SensorKind = "camera"
	KindImu      SensorKind = "imu"
	KindBaro     SensorKind = "baro"
	KindGnss     SensorKind = "gnss"
	KindUwb      SensorKind = "uwb"
	KindMic      SensorKind = "mic"
	KindGas      SensorKind = "gas"
	KindOdometry SensorKind = "odometry"
)

// SensorKinds lists every modality in a fixed order.
func SensorKinds() []SensorKind {
	return []SensorKind{KindLidar2D, KindCamera, KindImu, KindBaro, KindGnss, KindUwb, KindMic, KindGas, KindOdometry}
}

func (k SensorKind) Valid() bool {
	for _, v := range SensorKinds() {
		if k == v {
			return true
		}
	}
	return false
}

// Stamp converts nanoseconds since the Unix epoch to the canonical timestamp
// form: UTC, no monotonic reading, so values compare with == and survive JSON.
func Stamp(unixNanos int64) time.Time { return time.Unix(0, unixNanos).UTC() }

// MaxIDLength bounds every identifier in this package, in bytes.
const MaxIDLength = 128

// MaxCoordinateM bounds site coordinates, matching selection.Region.
const MaxCoordinateM = 1e6

// validID accepts 1..MaxIDLength printable ASCII bytes without spaces or '/',
// which keeps identifiers safe to join into component entity keys.
func validID(s string) bool {
	if len(s) == 0 || len(s) > MaxIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c > '~' || c == '/' {
			return false
		}
	}
	return true
}

// validText accepts an optional human-readable string of bounded length.
func validText(s string) bool { return len(s) <= 256 }

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// coordinates reports whether every value is a finite site coordinate.
func coordinates(values ...float64) bool {
	for _, v := range values {
		if !finite(v) || math.Abs(v) > MaxCoordinateM {
			return false
		}
	}
	return true
}

func unit(v float64) bool { return v >= 0 && v <= 1 }

func invalid(what string) error { return errors.New("spatial: invalid " + what) }
