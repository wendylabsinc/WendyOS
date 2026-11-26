package occupancy

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// elevationWindow is how many recent scan elevations the floor elevation is
// the median of: long enough to ignore a flight of stairs or a lift ride.
const elevationWindow = 64

// Stats counts what a Mapper did with its scans. Every scan given to Ingest
// ends in exactly one of the counters, or is still Pending; Integrate counts
// the scans it accepts and returns an error for the others.
type Stats struct {
	// Integrated scans are in the source's map (committed or not).
	Integrated int
	// Pending scans wait for a pose sample after their instant.
	Pending int
	// Uncalibrated scans came from a lidar without a valid calibration.
	Uncalibrated int
	// NoPose scans had no two pose samples on one floor bracketing their
	// instant within Config.MaxPoseGap, or outlived Config.MaxPending.
	NoPose int
	// Uncertain scans had a pose beyond Config.MaxPoseUncertaintyM.
	Uncertain int
	// WrongFrame scans had a pose in another frame than Config.FrameID.
	WrongFrame int
}

type tileState struct {
	inputs []world.Input // ancestry, kept reduced
	last   time.Time     // site time of the newest scan over the tile
	dirty  bool
	// The layer this mapper last wrote or adopted; anything else in the
	// replica under its key is evidence of an earlier run of this source.
	version, tag uint64
}

type floorMap struct {
	g          *grid.Grid
	tiles      map[[2]int32]*tileState // by (ix, iz)
	elevations []float64               // ring of recent scan elevations
	next       int
}

type poseSample struct {
	at   time.Time
	pose spatial.DevicePose
}

type lidar struct {
	mount  geom.Pose2
	offset time.Duration
}

type pendingScan struct {
	scan  spatial.Scan
	input world.Input
}

// Mapper is one source of the distributed map: it integrates the lidar scans
// of one device into that device's own layers of a Replica.
//
// Streaming use: Calibrate with the lidar's calibration, Locate with every
// pose estimate of the device in time order, Ingest every measurement of the
// device, and Commit at the rate the map should be shared. Integrate is the
// same step for a caller that already holds the pose at the scan's instant.
type Mapper struct {
	r       *Replica
	source  spatial.DeviceID
	floors  map[spatial.FloorID]*floorMap
	lidars  map[spatial.SensorID]lidar
	poses   []poseSample
	pending []pendingScan
	stats   Stats
}

// NewMapper returns the mapper of source writing into r. Layers r already
// holds for the source, from an earlier run of it, are kept: Commit folds
// them into the tiles it rewrites and continues their versions.
func NewMapper(r *Replica, source spatial.DeviceID) (*Mapper, error) {
	// A layer names its source in a MapTile version vector, which validates it.
	probe := Layer{LayerKey: LayerKey{Floor: "probe", Source: source}, Version: 1, CellSizeM: r.cfg.Grid.CellSize, Cells: make([]byte, 1)}
	if _, err := probe.Tile(); err != nil {
		return nil, fmt.Errorf("%w: source %q", ErrInvalid, source)
	}
	return &Mapper{r: r, source: source, floors: map[spatial.FloorID]*floorMap{}, lidars: map[spatial.SensorID]lidar{}}, nil
}

// Stats returns the counters.
func (m *Mapper) Stats() Stats {
	s := m.stats
	s.Pending = len(m.pending)
	return s
}

// Calibrate records the calibration of one of the device's sensors. Only a
// lidar mounted on the device matters; any state but CalibrationValid, or a
// mount that is not planar, withdraws it, so the scans of a quarantined
// lidar are refused until a valid revision arrives.
func (m *Mapper) Calibrate(c spatial.Calibration) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if c.Device != m.source {
		return ErrForeign
	}
	if c.Kind != spatial.KindLidar2D {
		return nil
	}
	mount, _, planar := c.SensorToParent.Planar()
	if c.State != spatial.CalibrationValid || c.Parent != spatial.ParentDeviceBase || c.ParentDevice != m.source || !planar {
		delete(m.lidars, c.Sensor)
		return nil
	}
	m.lidars[c.Sensor] = lidar{mount: mount, offset: c.TimeOffset}
	return nil
}

// Locate records a pose estimate of the device at site time at. Samples must
// arrive in time order. Scans waiting for a pose at or after their instant
// are integrated.
func (m *Mapper) Locate(at time.Time, p spatial.DevicePose) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if p.Device != m.source {
		return ErrForeign
	}
	if n := len(m.poses); n > 0 && !at.After(m.poses[n-1].at) {
		return fmt.Errorf("%w: pose sample out of order", ErrInvalid)
	}
	m.poses = append(m.poses, poseSample{at, p})
	err := m.drain()
	old := sort.Search(len(m.poses), func(i int) bool { return !m.poses[i].at.Before(at.Add(-m.r.cfg.PoseHistory)) })
	m.poses = slices.Delete(m.poses, 0, old)
	return err
}

// Ingest takes one measurement of the device with its ancestry entry. A
// lidar scan is integrated at the pose interpolated between the two Locate
// samples around its site time (its timestamp less the calibrated clock
// offset), or waits for the later one. Other measurements are ignored. A
// scan that cannot be placed is not an error: it is counted in Stats.
func (m *Mapper) Ingest(meas spatial.Measurement, in world.Input) error {
	s, ok := meas.(spatial.Scan)
	if !ok {
		return nil
	}
	if err := checkScan(m.source, s, in); err != nil {
		return err
	}
	if len(m.pending) >= m.r.cfg.MaxPending {
		m.pending = slices.Delete(m.pending, 0, 1)
		m.stats.NoPose++
	}
	m.pending = append(m.pending, pendingScan{s, in})
	return m.drain()
}

func checkScan(source spatial.DeviceID, s spatial.Scan, in world.Input) error {
	if s.Device != source {
		return ErrForeign
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if in.ID == "" {
		return fmt.Errorf("%w: scan without an ancestry entry", ErrInvalid)
	}
	return nil
}

// drain places every pending scan whose instant the pose samples now reach.
func (m *Mapper) drain() error {
	keep := m.pending[:0]
	var first error
	for _, p := range m.pending {
		cal, ok := m.lidars[p.scan.Sensor]
		if !ok {
			m.stats.Uncalibrated++
			continue
		}
		at := p.scan.Time.Add(-cal.offset)
		if n := len(m.poses); n == 0 || at.After(m.poses[n-1].at) {
			keep = append(keep, p)
			continue
		}
		pose, err := m.poseAt(at)
		if err == nil {
			err = m.insert(at, pose, cal.mount, p.scan, p.input)
		}
		switch {
		case err == nil:
		case errors.Is(err, ErrNoPose):
			m.stats.NoPose++
		case errors.Is(err, ErrUncertain):
			m.stats.Uncertain++
		case errors.Is(err, ErrFrame):
			m.stats.WrongFrame++
		case first == nil:
			first = err
		}
	}
	clear(m.pending[len(keep):])
	m.pending = keep
	return first
}

// poseAt interpolates the device pose at a site instant no later than the
// newest sample. It refuses rather than extrapolate or bridge a long gap or
// a floor change.
func (m *Mapper) poseAt(at time.Time) (spatial.DevicePose, error) {
	i := sort.Search(len(m.poses), func(i int) bool { return !m.poses[i].at.Before(at) })
	b := m.poses[i]
	if b.at.Equal(at) {
		return b.pose, nil
	}
	if i == 0 {
		return spatial.DevicePose{}, ErrNoPose
	}
	a := m.poses[i-1]
	if b.at.Sub(a.at) > m.r.cfg.MaxPoseGap || a.pose.Floor != b.pose.Floor || a.pose.FrameID != b.pose.FrameID {
		return spatial.DevicePose{}, ErrNoPose
	}
	f := float64(at.Sub(a.at)) / float64(b.at.Sub(a.at))
	p := a.pose
	base := a.pose.Pose2().Interpolate(b.pose.Pose2(), f)
	p.PositionM = [3]float64{base.X, a.pose.PositionM[1] + f*(b.pose.PositionM[1]-a.pose.PositionM[1]), base.Z}
	p.Yaw = base.Yaw
	p.UncertaintyM = max(a.pose.UncertaintyM, b.pose.UncertaintyM)
	return p, nil
}

// Integrate adds one scan taken at site time at, with the device at pose and
// the lidar mounted at mount (scan frame in the device base frame). in is the
// scan's ancestry entry. It refuses a scan or pose of another device
// (ErrForeign), a pose in another frame (ErrFrame) and a pose beyond
// Config.MaxPoseUncertaintyM (ErrUncertain). The evidence reaches the replica
// at the next Commit.
func (m *Mapper) Integrate(at time.Time, pose spatial.DevicePose, mount geom.Pose2, s spatial.Scan, in world.Input) error {
	if err := checkScan(m.source, s, in); err != nil {
		return err
	}
	if pose.Device != m.source {
		return ErrForeign
	}
	if err := pose.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if at.IsZero() {
		return fmt.Errorf("%w: scan without a site time", ErrInvalid)
	}
	return m.insert(at, pose, mount, s, in)
}

func (m *Mapper) insert(at time.Time, pose spatial.DevicePose, mount geom.Pose2, s spatial.Scan, in world.Input) error {
	cfg := m.r.cfg
	if pose.FrameID != cfg.FrameID {
		return fmt.Errorf("%w: %q, map is in %q", ErrFrame, pose.FrameID, cfg.FrameID)
	}
	if pose.UncertaintyM > cfg.MaxPoseUncertaintyM {
		return fmt.Errorf("%w: %.3f m, limit %.3f m", ErrUncertain, pose.UncertaintyM, cfg.MaxPoseUncertaintyM)
	}
	fm := m.floors[pose.Floor]
	if fm == nil {
		g, err := grid.New(cfg.Grid)
		if err != nil {
			return err
		}
		fm = &floorMap{g: g, tiles: map[[2]int32]*tileState{}}
		m.floors[pose.Floor] = fm
	}
	sensor := pose.Pose2().Compose(mount)
	if err := fm.g.InsertScan(sensor, s); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	m.stats.Integrated++
	if len(fm.elevations) < elevationWindow {
		fm.elevations = append(fm.elevations, pose.PositionM[1])
	} else {
		fm.elevations[fm.next] = pose.PositionM[1]
		fm.next = (fm.next + 1) % elevationWindow
	}

	// The scan can only have changed cells inside the bounding box of its
	// origin and beam ends (the footprint InsertScan rasterizes), so the
	// tiles over that box, plus a cell of margin, are the ones to re-export.
	// ponytail: the box over-marks the tiles between diverging beams, which
	// costs an export and credits the scan in their ancestry; walk the beams
	// per tile if Commit shows up in a profile.
	limit := s.RangeMax
	if cfg.Grid.MaxRange > 0 && cfg.Grid.MaxRange < limit {
		limit = cfg.Grid.MaxRange
	}
	lo, hi := sensor.Pos(), sensor.Pos()
	for i := range s.Ranges {
		a, r, state := s.Beam(i)
		if state == spatial.BeamInvalid {
			continue
		}
		if state != spatial.BeamHit || r > limit {
			r = limit
		}
		e := sensor.Pos().Add(geom.Heading(sensor.Yaw + a).Scale(r))
		lo, hi = geom.Vec2{X: min(lo.X, e.X), Z: min(lo.Z, e.Z)}, geom.Vec2{X: max(hi.X, e.X), Z: max(hi.Z, e.Z)}
	}
	margin := geom.Vec2{X: cfg.Grid.CellSize, Z: cfg.Grid.CellSize}
	x0, z0 := grid.TileOf(fm.g.CellAt(lo.Sub(margin)), cfg.TileSide)
	x1, z1 := grid.TileOf(fm.g.CellAt(hi.Add(margin)), cfg.TileSide)
	for iz := z0; iz <= z1; iz++ {
		for ix := x0; ix <= x1; ix++ {
			ts := fm.tiles[[2]int32{ix, iz}]
			if ts == nil {
				ts = &tileState{}
				fm.tiles[[2]int32{ix, iz}] = ts
			}
			ts.dirty = true
			if at.After(ts.last) {
				ts.last = at
			}
			if ts.inputs = append(ts.inputs, in); len(ts.inputs) > spatial.MaxInputs {
				ts.inputs = spatial.ReduceInputs(ts.inputs)
			}
		}
	}
	return nil
}

// Commit writes every tile the scans since the last Commit changed as a new
// version of the source's layer in the replica, and returns those layers
// ordered by key: what the source publishes. A tile whose cells did not
// change keeps its version. A tile this run mapped whose layer a sync has
// since replaced with one of an earlier run is rewritten with both.
func (m *Mapper) Commit() ([]Layer, error) {
	cfg := m.r.cfg
	floors := make([]spatial.FloorID, 0, len(m.floors))
	for f := range m.floors {
		floors = append(floors, f)
	}
	slices.Sort(floors)
	var out []Layer
	for _, floor := range floors {
		fm := m.floors[floor]
		var dirty [][2]int32
		for k, ts := range fm.tiles {
			// A tile is also due when a sync replaced what this run wrote with
			// a higher version from an earlier run: this run's evidence for it
			// is then in the grid only.
			cur := m.r.entry(LayerKey{Floor: floor, IX: k[0], IZ: k[1], Source: m.source})
			if ts.dirty || (cur != nil && (cur.Version != ts.version || cur.tag() != ts.tag)) {
				dirty = append(dirty, k)
			}
		}
		if len(dirty) == 0 {
			continue
		}
		slices.SortFunc(dirty, func(a, b [2]int32) int { return cmp.Or(cmp.Compare(a[1], b[1]), cmp.Compare(a[0], b[0])) })
		elevation := slices.Sorted(slices.Values(fm.elevations))[len(fm.elevations)/2]
		for _, k := range dirty {
			ts := fm.tiles[k]
			key := LayerKey{Floor: floor, IX: k[0], IZ: k[1], Source: m.source}
			cur := m.r.entry(key)
			if cur != nil && (cur.Version != ts.version || cur.tag() != ts.tag) {
				// An earlier run of this source wrote the layer: its evidence
				// is not in this run's grid yet.
				if err := absorb(fm.g, cur, cfg.TileSide); err != nil {
					return out, err
				}
				ts.inputs = spatial.ReduceInputs(append(ts.inputs, cur.Inputs...))
				if cur.Time.After(ts.last) {
					ts.last = cur.Time
				}
				ts.version, ts.tag = cur.Version, cur.tag()
			}
			version := uint64(1)
			if cur != nil {
				version = cur.Version + 1
			}
			t, err := fm.g.Tile(floor, k[0], k[1], cfg.TileSide, elevation, map[string]uint64{string(m.source): version})
			if err != nil {
				return out, fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			ts.dirty = false
			if cur == nil && bytes.Count(t.Cells, []byte{0}) == len(t.Cells) {
				continue // nothing observed in the tile yet
			}
			if cur != nil && bytes.Equal(cur.Cells, t.Cells) {
				continue
			}
			e, err := m.r.check(Layer{LayerKey: key, Version: version, CellSizeM: cfg.Grid.CellSize, ElevationM: elevation, Time: ts.last, Cells: t.Cells, Inputs: ts.inputs})
			if err != nil {
				return out, err
			}
			m.r.put(e)
			ts.version, ts.tag = e.Version, e.tag()
			out = append(out, e.Layer)
		}
	}
	return out, nil
}

// absorb adds the cells of a layer to the grid.
func absorb(g *grid.Grid, e *entry, side int) error {
	x0, z0 := e.IX*int32(side), e.IZ*int32(side)
	for i, b := range e.Cells {
		if b == 0 {
			continue
		}
		c := grid.Cell{IX: x0 + int32(i%side), IZ: z0 + int32(i/side)}
		if err := g.Add(c, float64(int8(b))*grid.TileLogOddsScale); err != nil {
			return err
		}
	}
	return nil
}
