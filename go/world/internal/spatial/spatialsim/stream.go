package spatialsim

import (
	"iter"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Filter selects what a Stream carries. An empty list selects everything.
type Filter struct {
	Devices []spatial.DeviceID
	Kinds   []spatial.SensorKind
}

// Stream yields every selected measurement sampled in [from, to), merged
// across devices and ordered by the true instant of sampling; ties keep
// device-id then sensor order. The timestamp a measurement carries is on its
// sensor's clock, so across sensors it may run a few tens of milliseconds
// out of this order. A device's manifests are yielded at the instant it
// starts, before its first sample, when that instant is in the interval.
//
// Generation is lazy: nothing is computed for a measurement until it is
// reached, and nothing at all for devices or kinds the filter excludes.
// Unfiltered, the Office site costs about 0.2 s per simulated minute and
// eight times that under the race detector, so filter and shorten there.
func (s *Site) Stream(from, to time.Time, f Filter) iter.Seq[spatial.Measurement] {
	return func(yield func(spatial.Measurement) bool) {
		type channel struct {
			d    *device
			sn   *sensor // nil for the manifest announcement
			next time.Duration
		}
		lo, hi := from.Sub(epoch), to.Sub(epoch)
		kind := func(k spatial.SensorKind) bool { return len(f.Kinds) == 0 || slices.Contains(f.Kinds, k) }
		var chans []channel
		for _, d := range s.devices {
			if len(f.Devices) > 0 && !slices.Contains(f.Devices, d.id) {
				continue
			}
			if d.start >= lo {
				chans = append(chans, channel{d, nil, d.start})
			}
			for i := range d.sensors {
				if sn := &d.sensors[i]; kind(sn.kind) {
					first := max(lo, d.start)
					chans = append(chans, channel{d, sn, (first + sn.period - 1) / sn.period * sn.period})
				}
			}
		}
		for {
			// ponytail: linear scan over at most a few hundred channels; a heap if device counts grow.
			best := -1
			for i := range chans {
				if chans[i].next < hi && (best < 0 || chans[i].next < chans[best].next) {
					best = i
				}
			}
			if best < 0 {
				return
			}
			ch := &chans[best]
			t := ch.next
			if ch.sn == nil {
				ch.next = never
				for i := range ch.d.sensors {
					if sn := &ch.d.sensors[i]; kind(sn.kind) && !yield(s.manifest(ch.d, sn)) {
						return
					}
				}
				continue
			}
			ch.next += ch.sn.period
			if !s.emit(ch.d.id, ch.sn.kind, s.At(t), yield) {
				return
			}
		}
	}
}

// emit yields what one sensor produces at one instant; false stops the stream.
func (s *Site) emit(dev spatial.DeviceID, kind spatial.SensorKind, at time.Time, yield func(spatial.Measurement) bool) bool {
	var m spatial.Measurement
	ok := false
	switch kind {
	case spatial.KindLidar2D:
		m, ok = s.Scan(dev, at)
	case spatial.KindOdometry:
		m, ok = s.Odometry(dev, at)
	case spatial.KindImu:
		m, ok = s.Imu(dev, at)
	case spatial.KindBaro:
		m, ok = s.Baro(dev, at)
	case spatial.KindGnss:
		m, ok = s.Gnss(dev, at)
	case spatial.KindCamera:
		m, ok = s.CameraFrame(dev, at)
	case spatial.KindGas:
		m, ok = s.Gas(dev, at)
	case spatial.KindUwb:
		for _, r := range s.Ranges(dev, at) {
			if !yield(r) {
				return false
			}
		}
	case spatial.KindMic:
		for _, b := range s.Bearings(dev, at) {
			if !yield(b) {
				return false
			}
		}
	}
	return !ok || yield(m)
}

func (s *Site) manifest(d *device, sn *sensor) spatial.SensorManifest {
	m := spatial.SensorManifest{Header: s.header(d, sn, d.start), Modality: sn.kind, Model: sn.model, Attachment: spatial.Attachment{Mode: spatial.AttachFixed}}
	if d.route != nil {
		m.Attachment = spatial.Attachment{Mode: spatial.AttachMounted, Carrier: d.id}
	}
	if sn.kind == spatial.KindCamera {
		in := d.declared
		m.Intrinsics = &in
	}
	return m
}

// Manifests is what every sensor announces when its device starts: kind,
// model and attachment hint, never a pose. A mobile camera is factory
// calibrated and declares its true intrinsics. A fixed camera declares only
// the nominal lens (no distortion, centred principal point); its true
// intrinsics are in Truth.
func (s *Site) Manifests() []spatial.SensorManifest {
	var out []spatial.SensorManifest
	for _, d := range s.devices {
		for i := range d.sensors {
			out = append(out, s.manifest(d, &d.sensors[i]))
		}
	}
	return out
}

// Fiducials is the known shape of every fiducial: robot markers, with points
// in the carrier's base frame at two heights, then wall tags, whose four
// corners are in the tag's own frame (x along the wall, y up, z out of it).
// Where a wall tag is in the site is Truth, not declared knowledge.
func (s *Site) Fiducials() []spatial.FiducialGeometry {
	var out []spatial.FiducialGeometry
	for _, d := range s.devices {
		if len(d.marker) > 0 {
			out = append(out, spatial.FiducialGeometry{ID: MarkerID(d.id), Carrier: d.id, Points: slices.Clone(d.marker)})
		}
	}
	for _, lv := range s.levels {
		for _, tag := range lv.tags {
			out = append(out, spatial.FiducialGeometry{ID: tag.ID, Points: slices.Clone(tagPoints)})
		}
	}
	return out
}

// MicArray is the surveyed mount of a microphone array: Pose.Yaw is the
// direction a Bearing azimuth of zero points along, Elevation its World Y.
type MicArray struct {
	Device    spatial.DeviceID
	Floor     spatial.FloorID
	Pose      geom.Pose2
	Elevation float64
}

// UwbTag is where a device's UWB antenna sits in its base frame (x forward,
// y up, z right), from the device's datasheet.
type UwbTag struct {
	Device spatial.DeviceID
	Mount  geom.Vec3
}

// Survey is what the installer of the fixed infrastructure wrote down: where
// the UWB beacons and the microphone arrays hang, and where each device
// carries its UWB antenna. An estimator may use it, as it may use Manifests.
// Cameras are deliberately absent: placing them is the job of calibration and
// registration.
//
// A Range is the 3D distance between two antennas. With the height difference
// dy between them known from the survey (beacon Y minus floor elevation minus
// the tag's Mount.Y), the planar distance is sqrt(max(d² − dy², 0)). On the
// default site dy is 2.1 m, so reading a 3D range as planar is wrong by 0.67 m
// three metres from a beacon.
type Survey struct {
	Beacons   []Beacon
	MicArrays []MicArray
	Tags      []UwbTag
}

func (s *Site) Survey() Survey {
	out := Survey{Beacons: slices.Clone(s.beacons)}
	for _, d := range s.devices {
		if d.route == nil && d.has(spatial.KindMic) {
			out.MicArrays = append(out.MicArrays, MicArray{Device: d.id, Floor: d.level.floorAt(d.pose.Pos()).id, Pose: d.pose, Elevation: d.elevation})
		}
		if d.has(spatial.KindUwb) {
			out.Tags = append(out.Tags, UwbTag{Device: d.id, Mount: d.uwbMount})
		}
	}
	return out
}

// Target is the geometry of the planar calibration target.
func (s *Site) Target() spatial.TargetGeometry { return s.target }

// TargetShows is the schedule on which the target is presented to cameras.
func (s *Site) TargetShows() []TargetShow { return slices.Clone(s.shows) }
