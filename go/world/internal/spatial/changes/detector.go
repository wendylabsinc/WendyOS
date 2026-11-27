package changes

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Detector finds changes in a stream of lidar scans. Feed it scans in time
// order; every Config.Epoch it compares what the epoch saw with the
// reference, reports what changed and lets the reference adopt the epoch.
// The first epoch only builds the reference. A Detector is not safe for
// concurrent use, and its results depend only on the order of the calls.
type Detector struct {
	ledger
	ref, cur map[spatial.FloorID]*Evidence
	start    time.Time // start of the open epoch; zero when none is open
	last     map[spatial.SensorRef]time.Time
	tracks   map[string]spatial.Track
	compared int
}

// NewDetector returns a detector with an empty reference.
func NewDetector(cfg Config) (*Detector, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Detector{
		ledger: ledger{cfg: cfg},
		ref:    map[spatial.FloorID]*Evidence{}, cur: map[spatial.FloorID]*Evidence{},
		last: map[spatial.SensorRef]time.Time{}, tracks: map[string]spatial.Track{},
	}, nil
}

// SetPortals replaces the door locations. A cluster of changed cells that
// fits the doorway of a spatial.PortalDoor is reported as that door closing
// or opening, with the portal id as Subject. Portal.State is not read.
func (d *Detector) SetPortals(portals []spatial.Portal) error {
	for _, p := range portals {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	d.portals = doors(portals)
	return nil
}

// ObserveTrack records where a mover is. Until Config.TrackTimeout after its
// LastSeen, lidar returns within its radius plus Config.TrackMargin of its
// extrapolated position are not counted as evidence. A lost track is
// forgotten.
func (d *Detector) ObserveTrack(t spatial.Track) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if t.State == spatial.TrackLost {
		delete(d.tracks, t.ID)
	} else {
		d.tracks[t.ID] = t
	}
	return nil
}

// AddScan integrates one scan: pose is the device's pose at the scan's site
// time and calib the calibration of its lidar, whose TimeOffset converts the
// scan's sensor time to site time. in is the scan's ancestry entry. When the
// scan starts a new epoch, the finished one is compared and the changes it
// created or revised are returned.
//
// ErrUnusable means the scan was skipped because its calibration is not
// valid or its pose is less certain than Config.Tolerance; the detector is
// unchanged. A scan sooner than Config.ScanInterval after the last one
// integrated from its sensor is dropped without error.
func (d *Detector) AddScan(pose spatial.DevicePose, calib spatial.Calibration, s spatial.Scan, in world.Input) ([]Change, error) {
	for _, err := range []error{s.Validate(), pose.Validate(), calib.Validate()} {
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if pose.Device != s.Device || calib.Device != s.Device || calib.Sensor != s.Sensor || calib.Kind != spatial.KindLidar2D || calib.Parent != spatial.ParentDeviceBase {
		return nil, fmt.Errorf("%w: pose and calibration must be those of the scan's lidar", ErrInvalid)
	}
	mount, _, planar := calib.SensorToParent.Planar()
	if calib.State != spatial.CalibrationValid || !planar || pose.UncertaintyM > d.cfg.Tolerance {
		return nil, fmt.Errorf("%w: calibration %q, pose uncertainty %.2f m", ErrUnusable, calib.State, pose.UncertaintyM)
	}
	at := s.Time.Add(-calib.TimeOffset)
	var out []Change
	switch {
	case d.start.IsZero():
		d.start = at
	case at.Sub(d.start) >= d.cfg.Epoch:
		out = d.close()
		d.start = d.start.Add(at.Sub(d.start) / d.cfg.Epoch * d.cfg.Epoch)
	}
	key := spatial.SensorRef{Device: s.Device, Sensor: s.Sensor}
	if last, ok := d.last[key]; ok && at.Sub(last) < d.cfg.ScanInterval {
		return out, nil
	}
	d.last[key] = at
	// ponytail: evidence, and so an event, belongs to the floor the observing
	// device stands on. A change seen only through a doorway from the floor
	// next door (the yard from the corridor) is reported on the observer's
	// floor. Assign floors from region polygons when the engine has them.
	e := d.cur[pose.Floor]
	if e == nil {
		e = newEvidence(pose.Floor, d.cfg)
		d.cur[pose.Floor] = e
	}
	elev := pose.PositionM[1]
	var dynamic []Disc
	for id, t := range d.tracks {
		dt := at.Sub(t.LastSeen)
		if dt > d.cfg.TrackTimeout {
			delete(d.tracks, id)
		} else if d.sameLevel(t.PositionM[1], elev) && dt >= -d.cfg.TrackTimeout {
			dynamic = append(dynamic, Disc{Centre: t.Point().XZ().Add(t.VelocityMPS.Scale(dt.Seconds())), Radius: t.RadiusM + d.cfg.TrackMargin})
		}
	}
	return out, e.Add(Sweep{Scan: s, Sensor: pose.Pose2().Compose(mount), Elevation: elev, Time: at, Input: in, Dynamic: dynamic})
}

// Flush closes the open epoch now, as the end of a stream does, and returns
// the changes it created or revised. The next scan starts a new epoch.
func (d *Detector) Flush() []Change {
	if d.start.IsZero() {
		return nil
	}
	d.start = time.Time{}
	return d.close()
}

// close compares every floor's finished epoch with its reference, floor by
// floor in id order, and lets the reference adopt it.
func (d *Detector) close() []Change {
	d.epoch++
	d.compared = 0
	floors := make([]spatial.FloorID, 0, len(d.cur))
	for f := range d.cur {
		floors = append(floors, f)
	}
	slices.Sort(floors)
	for _, f := range floors {
		ref, cur := d.ref[f], d.cur[f]
		if ref == nil {
			ref = newEvidence(f, d.cfg)
			d.ref[f] = ref
		}
		compared, reported := d.observe(ref, cur)
		d.compared += compared
		ref.adopt(cur, reported)
		delete(d.cur, f)
	}
	return d.settle()
}

// Changes is every open change in its current form, ordered by id: those
// with evidence in the last Config.OpenEpochs epochs. An id that is no
// longer listed has aged out or was merged into the move that replaced it.
func (d *Detector) Changes() []Change {
	out := make([]Change, len(d.open))
	for i, c := range d.open {
		out[i] = c.export(d.cfg)
	}
	slices.SortFunc(out, func(a, b Change) int { return cmp.Compare(a.Event.ID, b.Event.ID) })
	return out
}

// Compared is how many cells the last finished epoch observed confidently
// that the reference also knew. Zero means that epoch could not be judged:
// it was the first, or it looked only at new ground.
func (d *Detector) Compared() int { return d.compared }

// Reference is the reference map of a floor: the last confirmed state of
// every cell. It is nil until an epoch on that floor has finished, and it is
// the detector's own: do not modify it.
func (d *Detector) Reference(floor spatial.FloorID) *Evidence { return d.ref[floor] }
