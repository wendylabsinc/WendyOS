package adapters

import (
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Converted is what one sensing.Sample yields. Poses are in the sample's own
// site frame (Sample.Frame), which the caller must match against the frame it
// works in.
type Converted struct {
	// Measurement is ready to ingest: a spatial.Scan for a lidar sample, a
	// spatial.Scalar for a gas sample. It is nil for a camera sample.
	Measurement spatial.Measurement
	// Camera is the header of a camera sample's frame: identity, time, image
	// size and Config.PixelSigma, with no detections, because a sample
	// carries an image reference and not detector output. It must not be
	// ingested as it is: an empty detection list states that nothing was
	// seen. Fill Detections from a detector first.
	Camera *spatial.CameraFrame
	// Calibration is the prior a camera sample declares, in state
	// "estimating": pinhole intrinsics without distortion and the optical
	// frame's site pose. Nil when the sample declares no focal lengths or no
	// camera-to-site transform.
	Calibration *spatial.Calibration
	// SensorToSite places the measurement's frame in the site at the sample
	// instant. Lidar and gas samples declare a position and no orientation,
	// so the rotation is the identity: a lidar Scan is expressed in a frame
	// at the sensor position with SITE-ALIGNED axes, not in the physical
	// sensor frame, and the heading of the device that carries the sensor
	// cannot be recovered from it. For a camera with a Calibration it is the
	// optical frame's pose; without one only the translation is declared.
	SensorToSite geom.Pose3
	// PositionSigmaM is the position uncertainty the sample declares
	// (Sample.UncertaintyM); whether that is 1σ or a bound is the source's
	// convention.
	PositionSigmaM float64
}

// FromSample converts one sensing.Sample. device is the device that produced
// it, which the sample does not name (take it from the authenticated outer
// component's source); the sensor id is Sample.SensorID. The measurement time
// is anchor + Sample.SampleNS: anchor is the run anchor the sample clock
// counts from, and when the sample carries a SensorLink it must equal the
// link's anchor.
//
// Fields used, by kind:
//
//   - lidar: Lidar.PointsM (site frame), Lidar.MaxRangeM, PositionM. The cloud
//     is flattened into a planar scan around PositionM (see Config.Beams,
//     BandLow, BandHigh, RangeMin, RangeSigma and planarScan for what that
//     loses). Points outside the height band or beyond MaxRangeM are dropped.
//   - gas: Gas.Species (the Scalar quantity), Gas.ValuePPM, Gas.UncertaintyPPM
//     (taken as 1σ), PositionM.
//   - camera: Camera.Width, Height, FX, FY, CX, CY, CameraToSite, PositionM,
//     UncertaintyM, CalibrationRevision, SensorLink.ClockErrorUS. Not used:
//     Camera.CalibrationValidForNS (a calibration has no expiry field) and
//     the evidence image.
//   - audio: refused with ErrUnmapped. A mono clip has no direction of
//     arrival, so there is no Bearing to report, and no level to report as a
//     Scalar.
//
// The sample's simulation lineage (RunID, ScenarioVersion, Seed) is not read:
// provenance comes from the outer component (see InputFrom).
func FromSample(cfg Config, device spatial.DeviceID, anchor time.Time, s sensing.Sample) (Converted, error) {
	c, err := cfg.resolved()
	if err != nil {
		return Converted{}, err
	}
	if s.Kind == "audio" {
		return Converted{}, fmt.Errorf("%w: a mono audio sample carries no direction of arrival", ErrUnmapped)
	}
	if anchor.IsZero() {
		return Converted{}, fmt.Errorf("%w: no run anchor for sample %q", ErrNoTime, s.ID)
	}
	if s.SampleNS < 0 {
		return Converted{}, invalid("negative sample time")
	}
	if link := s.SensorLink; link != nil && !time.UnixMicro(link.AnchorUnixUS).Equal(anchor) {
		return Converted{}, invalid("SensorLink anchor differs from the run anchor")
	}
	if len(s.PositionM) != 3 {
		return Converted{}, fmt.Errorf("%w: sample %q has no position_m", ErrNoPose, s.ID)
	}
	if !finite(append([]float64{s.UncertaintyM}, s.PositionM...)...) || s.UncertaintyM < 0 {
		return Converted{}, invalid("sample position or uncertainty")
	}
	h := spatial.Header{Device: device, Sensor: spatial.SensorID(s.SensorID), Time: spatial.Stamp(anchor.UnixNano() + s.SampleNS)}
	origin := geom.Vec3{X: s.PositionM[0], Y: s.PositionM[1], Z: s.PositionM[2]}
	out := Converted{SensorToSite: geom.Pose3{R: geom.Identity3(), T: origin}, PositionSigmaM: s.UncertaintyM}
	switch s.Kind {
	case "lidar":
		cloud := s.Lidar
		if cloud == nil || cloud.PointsM == nil || !(cloud.MaxRangeM > c.RangeMin) || math.IsInf(cloud.MaxRangeM, 0) {
			return Converted{}, invalid("lidar cloud or maximum range")
		}
		points := make([]geom.Vec2, 0, len(cloud.PointsM))
		for _, p := range cloud.PointsM {
			if !finite(p[:]...) {
				return Converted{}, invalid("non-finite lidar point")
			}
			if d := geom.Vec3FromArray(p).Sub(origin); d.Y >= c.BandLow && d.Y <= c.BandHigh {
				points = append(points, d.XZ())
			}
		}
		scan, err := c.planarScan(h, cloud.MaxRangeM, points)
		if err != nil {
			return Converted{}, err
		}
		out.Measurement = scan
	case "gas":
		if s.Gas == nil {
			return Converted{}, invalid("gas sample without gas metadata")
		}
		scalar, err := checked(spatial.Scalar{Header: h, Quantity: s.Gas.Species, Value: s.Gas.ValuePPM, Unit: "ppm", Sigma: s.Gas.UncertaintyPPM})
		if err != nil {
			return Converted{}, err
		}
		out.Measurement = scalar
	case "camera":
		cam := s.Camera
		if cam == nil {
			return Converted{}, invalid("camera sample without camera metadata")
		}
		frame, err := checked(spatial.CameraFrame{Header: h, Width: cam.Width, Height: cam.Height, PixelSigma: c.PixelSigma})
		if err != nil {
			return Converted{}, err
		}
		out.Camera = &frame
		if len(cam.CameraToSite) == 0 || (cam.FX == 0 && cam.FY == 0) {
			break
		}
		pose, err := geom.Pose3FromMatrix4(cam.CameraToSite)
		if err != nil {
			return Converted{}, invalid("camera_to_site: %v", err)
		}
		if pose.T.Dist(origin) > 1e-6 {
			return Converted{}, invalid("camera_to_site disagrees with position_m")
		}
		at := spatial.At(pose.T, s.UncertaintyM)
		cal := spatial.Calibration{
			Device: device, Sensor: h.Sensor, Kind: spatial.KindCamera,
			Intrinsics:     &geom.Intrinsics{FX: cam.FX, FY: cam.FY, CX: cam.CX, CY: cam.CY, Width: cam.Width, Height: cam.Height},
			SensorToParent: pose, Parent: spatial.ParentSite, Location: &at,
			TranslationSigmaM: s.UncertaintyM, RotationSigmaRad: c.RotationSigma,
			ResidualUnit: "px", Method: "declared", Revision: s.CalibrationRevision, State: spatial.CalibrationEstimating,
		}
		if link := s.SensorLink; link != nil {
			// The link declares a bound; using it as 1σ is conservative.
			cal.TimeOffsetSigma = time.Duration(link.ClockErrorUS) * time.Microsecond
		}
		if err := cal.Validate(); err != nil {
			return Converted{}, invalid("declared camera calibration: %v", err)
		}
		out.Calibration, out.SensorToSite = &cal, pose
	default:
		return Converted{}, invalid("sample kind %q", s.Kind)
	}
	return out, nil
}
