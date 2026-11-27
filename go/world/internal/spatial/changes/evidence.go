package changes

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	// Feature names this package in provenance: ModelID "wendy.spatial.changes".
	Feature = "changes"
	// ModelVersion changes whenever the same evidence could yield other events.
	ModelVersion = "1"
)

var (
	// ErrConfig reports a Config outside its documented ranges.
	ErrConfig = errors.New("changes: invalid config")
	// ErrInvalid reports a malformed argument.
	ErrInvalid = errors.New("changes: invalid argument")
	// ErrIncompatible reports two evidence maps that cannot be compared:
	// different floors or different configurations.
	ErrIncompatible = errors.New("changes: incompatible evidence")
	// ErrNoOverlap reports that no cell was confidently observed in both
	// maps, so nothing can be said about change.
	ErrNoOverlap = errors.New("changes: no cell confidently observed in both epochs")
	// ErrUnusable reports a scan that was not integrated because its
	// calibration is not valid or its pose is too uncertain.
	ErrUnusable = errors.New("changes: scan not usable as evidence")
)

// Config holds every threshold and noise assumption. A zero field takes its
// default; DefaultConfig lists them.
type Config struct {
	// CellSize is the cell side in metres. Default 0.05, the map cell.
	CellSize float64
	// MaxRange caps, in metres, how far a beam is integrated; a return
	// beyond it only clears free space up to it. A heading error e displaces
	// a return at range r by r·e, so MaxRange times the heading error of the
	// poses in use should stay under Tolerance. Default 10 (9 cm at 0.5°).
	MaxRange float64
	// HitMargin is the stretch before a return, in metres, over which the
	// beam is not counted as crossing free space. Range noise, pose error and
	// the cell raster smear one surface over two or three cells along the
	// beam; without the margin each of them is crossed by the returns that
	// land in the next and a thin surface (a door leaf on a cell boundary)
	// reads as Transient. Default 0.10, two cells: twice the 2σ of the
	// default range noise (1.5 cm) and pose error (2 cm) plus one cell.
	HitMargin float64
	// ScanInterval is the least time between two integrated sweeps of one
	// sensor; the Detector drops sweeps that come sooner. Looks must be
	// separate in time for MinObservations to mean anything, yet close
	// enough that a robot driving past an object at walking pace collects
	// MinObservations of each face it sees: a side face is in view for a
	// few seconds and its returns are shared between the two or three cells
	// the surface smears over. Default 0.5 s.
	ScanInterval time.Duration
	// MinObservations is how many sweeps must have looked at a cell before
	// its state is trusted. Default 6: at the default ScanInterval a walking
	// person covers five body widths in that time.
	MinObservations int
	// OccupiedRatio is the share of looks that must have returned from a
	// cell for it to be Occupied. Default 0.3, where the grid sensor model
	// (hit 0.85, miss 0.40) balances: range noise spreads a surface over two
	// or three cells and the near ones are crossed as often as they return.
	OccupiedRatio float64
	// FreeRatio is the share of returns at or below which a cell is Free.
	// Between FreeRatio and OccupiedRatio a cell is Transient. Default 0.05:
	// someone crossing a cell at walking pace fills it for a few percent of
	// an epoch.
	FreeRatio float64
	// Tolerance is the map misregistration to absorb, metres: a changed cell
	// with matching structure within this distance in the other map is not a
	// change, and a scan whose pose uncertainty exceeds it is refused.
	// Default 0.15, the localization target (10 cm) plus one cell.
	Tolerance float64
	// ClusterLink is the distance in metres within which changed cells of
	// the same sign belong to one object. Default 0.5, a robot body width:
	// a lidar cannot be driven between objects closer than that to tell
	// them apart.
	ClusterLink float64
	// MinClusterCells is the fewest changed cells an event needs. Default 6,
	// the outline of a 0.3 m object.
	MinClusterCells int
	// MoveMaxDistance is how far apart, in metres, a disappearance and an
	// appearance may be and still be one object that moved. Default 6, the
	// scale of a room; beyond it they are reported separately.
	MoveMaxDistance float64
	// FootprintRatio is the least ratio of the smaller to the larger extent
	// for a disappearance and an appearance to be the same object. Default
	// 0.5: the outline seen depends on where it was seen from.
	FootprintRatio float64
	// Epoch is the length of one evidence epoch of the Detector. It should
	// cover a patrol loop, so that an epoch looks at most of what the
	// reference knows. Default 2 minutes.
	Epoch time.Duration
	// OpenEpochs is how many epochs after its last evidence a change stays
	// open: able to take further evidence under the same id and to pair
	// into a move. Default 2.
	OpenEpochs int
	// TrackMargin is added to a track's radius when its returns are
	// excluded, metres. Default 0.3, the tracker's position target (0.2 m)
	// and two cells.
	TrackMargin float64
	// TrackTimeout is how long after its last observation a track still
	// excludes returns, extrapolated at its velocity. Default 2 s.
	TrackTimeout time.Duration
	// LevelHeight is the elevation difference in metres under which two
	// floors are the same level (an indoor floor and the yard outside it),
	// so that a change seen from both is one event. Default 1.5.
	LevelHeight float64
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	return Config{
		CellSize: 0.05, MaxRange: 10, HitMargin: 0.10, ScanInterval: 500 * time.Millisecond, MinObservations: 6, OccupiedRatio: 0.3, FreeRatio: 0.05,
		Tolerance: 0.15, ClusterLink: 0.5, MinClusterCells: 6, MoveMaxDistance: 6, FootprintRatio: 0.5,
		Epoch: 2 * time.Minute, OpenEpochs: 2, TrackMargin: 0.3, TrackTimeout: 2 * time.Second, LevelHeight: 1.5,
	}
}

func (c Config) withDefaults() (Config, error) {
	d := DefaultConfig()
	for _, f := range []struct{ v, def *float64 }{
		{&c.CellSize, &d.CellSize}, {&c.MaxRange, &d.MaxRange}, {&c.HitMargin, &d.HitMargin}, {&c.OccupiedRatio, &d.OccupiedRatio}, {&c.FreeRatio, &d.FreeRatio},
		{&c.Tolerance, &d.Tolerance}, {&c.ClusterLink, &d.ClusterLink}, {&c.MoveMaxDistance, &d.MoveMaxDistance},
		{&c.FootprintRatio, &d.FootprintRatio}, {&c.TrackMargin, &d.TrackMargin}, {&c.LevelHeight, &d.LevelHeight},
	} {
		if *f.v == 0 {
			*f.v = *f.def
		}
		if math.IsNaN(*f.v) || math.IsInf(*f.v, 0) || *f.v < 0 {
			return c, fmt.Errorf("%w: fields must be finite and positive", ErrConfig)
		}
	}
	for _, f := range []struct{ v, def *time.Duration }{{&c.ScanInterval, &d.ScanInterval}, {&c.Epoch, &d.Epoch}, {&c.TrackTimeout, &d.TrackTimeout}} {
		if *f.v == 0 {
			*f.v = *f.def
		}
	}
	for _, f := range []struct{ v, def *int }{{&c.MinObservations, &d.MinObservations}, {&c.MinClusterCells, &d.MinClusterCells}, {&c.OpenEpochs, &d.OpenEpochs}} {
		if *f.v == 0 {
			*f.v = *f.def
		}
	}
	switch {
	case c.ScanInterval < 0 || c.Epoch < c.ScanInterval || c.TrackTimeout < 0:
		return c, fmt.Errorf("%w: need ScanInterval ≥ 0, Epoch ≥ ScanInterval, TrackTimeout ≥ 0", ErrConfig)
	case c.MinObservations < 1 || c.MinClusterCells < 1 || c.OpenEpochs < 0:
		return c, fmt.Errorf("%w: need MinObservations ≥ 1, MinClusterCells ≥ 1, OpenEpochs ≥ 0", ErrConfig)
	case !(c.FreeRatio < c.OccupiedRatio && c.OccupiedRatio <= 1) || c.FootprintRatio > 1:
		return c, fmt.Errorf("%w: need FreeRatio < OccupiedRatio ≤ 1 and FootprintRatio ≤ 1", ErrConfig)
	case c.CellSize < 1e-3 || c.MaxRange/c.CellSize > 1e5 || c.ClusterLink/c.CellSize > 100 || c.Tolerance/c.CellSize > 100:
		return c, fmt.Errorf("%w: CellSize against MaxRange, ClusterLink or Tolerance", ErrConfig)
	}
	return c, nil
}

// State is what an Evidence map knows about one cell.
type State uint8

const (
	// Unobserved cells were looked at fewer than Config.MinObservations times.
	Unobserved State = iota
	// Free cells were crossed by nearly every sweep that reached them.
	Free
	// Occupied cells returned a steady share of the sweeps that reached them.
	Occupied
	// Transient cells read both ways: something moved through.
	Transient
)

func (s State) String() string {
	return [...]string{"unobserved", "free", "occupied", "transient"}[s]
}

// Disc is a planar disc in the site frame.
type Disc struct {
	Centre geom.Vec2
	Radius float64
}

// Sweep is one lidar sweep placed in the site frame.
type Sweep struct {
	Scan spatial.Scan
	// Sensor is the pose of the scan frame in the site frame: the device
	// pose composed with the lidar mount.
	Sensor geom.Pose2
	// Elevation is the height of the floor the device stands on, World Y.
	Elevation float64
	// Time is the site time of the sweep.
	Time time.Time
	// Input is the ancestry entry of the scan; one with an empty ID is not
	// recorded.
	Input world.Input
	// Dynamic are the known movers at Time: a return inside one is theirs,
	// not the environment's, and is not counted.
	Dynamic []Disc
}

const (
	chunkBits  = 5
	chunkSide  = 1 << chunkBits
	chunkMask  = chunkSide - 1
	chunkCells = chunkSide * chunkSide
)

// tally is the evidence of one cell: sweeps that returned from it and sweeps
// that crossed it. mark is the sweep that last touched it, shifted left, with
// the low bit set when that sweep returned from it.
type tally struct {
	hit, miss uint16
	mark      uint32
}

type chunkKey struct{ x, z int32 }

type chunk [chunkCells]tally

// Evidence is what the lidars of one floor saw during one span of time:
// per-cell counts of returns and crossings. It is not safe for concurrent
// use.
type Evidence struct {
	cfg      Config
	floor    spatial.FloorID
	chunks   map[chunkKey]*chunk
	sweeps   uint32
	from, to time.Time
	elev     float64
	levels   map[int32]uint32 // sweeps per decimetre of elevation
	level    int32            // the fullest of them
	inputs   []world.Input

	lastKey   chunkKey // one-entry cache for at
	lastChunk *chunk
}

// NewEvidence returns an empty map of one floor.
func NewEvidence(floor spatial.FloorID, cfg Config) (*Evidence, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if floor == "" {
		return nil, fmt.Errorf("%w: empty floor", ErrInvalid)
	}
	return newEvidence(floor, cfg), nil
}

func newEvidence(floor spatial.FloorID, cfg Config) *Evidence {
	return &Evidence{cfg: cfg, floor: floor, chunks: map[chunkKey]*chunk{}, levels: map[int32]uint32{}}
}

// Floor is the floor the map belongs to.
func (e *Evidence) Floor() spatial.FloorID { return e.floor }

// Span is the site time of the first and the last sweep; both are zero for
// an empty map.
func (e *Evidence) Span() (from, to time.Time) { return e.from, e.to }

// Sweeps is how many sweeps the map holds.
func (e *Evidence) Sweeps() int { return int(e.sweeps) }

// State is what the map knows about the cell holding p.
func (e *Evidence) State(p geom.Vec2) State {
	return e.state(e.get(grid.Cell{IX: floorCell(p.X / e.cfg.CellSize), IZ: floorCell(p.Z / e.cfg.CellSize)}))
}

func (e *Evidence) state(t tally) State {
	n := int(t.hit) + int(t.miss)
	if n < e.cfg.MinObservations {
		return Unobserved
	}
	switch r := float64(t.hit) / float64(n); {
	case r >= e.cfg.OccupiedRatio:
		return Occupied
	case r <= e.cfg.FreeRatio:
		return Free
	}
	return Transient
}

func floorCell(v float64) int32 { return int32(math.Floor(v)) }

// cellOf is the cell at index i of the chunk at key.
func cellOf(key chunkKey, i int) grid.Cell {
	return grid.Cell{IX: key.x<<chunkBits | int32(i&chunkMask), IZ: key.z<<chunkBits | int32(i>>chunkBits)}
}

func (e *Evidence) get(c grid.Cell) tally {
	if ch := e.chunks[chunkKey{c.IX >> chunkBits, c.IZ >> chunkBits}]; ch != nil {
		return ch[int(c.IZ&chunkMask)<<chunkBits|int(c.IX&chunkMask)]
	}
	return tally{}
}

// at returns the cell for writing, allocating its chunk.
func (e *Evidence) at(ix, iz int32) *tally {
	key := chunkKey{ix >> chunkBits, iz >> chunkBits}
	if e.lastChunk == nil || key != e.lastKey {
		ch := e.chunks[key]
		if ch == nil {
			ch = new(chunk)
			e.chunks[key] = ch
		}
		e.lastKey, e.lastChunk = key, ch
	}
	return &e.lastChunk[int(iz&chunkMask)<<chunkBits|int(ix&chunkMask)]
}

// Add counts one sweep. Each beam with a return crosses the cells before it,
// up to Config.HitMargin short of it, and returns from the cell holding it; a
// beam with no return or a return beyond Config.MaxRange only crosses; a cell
// counts once per sweep and a return wins over a crossing. A return inside a
// Dynamic disc is not counted.
func (e *Evidence) Add(sw Sweep) error {
	s, p := sw.Scan, sw.Sensor
	for _, v := range []float64{p.X, p.Z, p.Yaw, sw.Elevation, s.AngleMin, s.AngleIncrement, s.RangeMax} {
		if math.IsNaN(v) || math.Abs(v) > spatial.MaxCoordinateM {
			return fmt.Errorf("%w: sweep geometry or sensor pose", ErrInvalid)
		}
	}
	if !(s.RangeMax > 0) || sw.Time.IsZero() {
		return fmt.Errorf("%w: sweep needs a positive range and a time", ErrInvalid)
	}
	inv := 1 / e.cfg.CellSize
	ox, oz := p.X*inv, p.Z*inv
	limit := math.Min(s.RangeMax, e.cfg.MaxRange)
	e.sweeps++
	crossed, returned := e.sweeps<<1, e.sweeps<<1|1
	for i := range s.Ranges {
		a, r, state := s.Beam(i)
		if state == spatial.BeamInvalid {
			continue
		}
		hit := state == spatial.BeamHit && r <= limit
		clear := limit
		if hit {
			clear = math.Max(r-e.cfg.HitMargin, 0)
		}
		d := geom.Heading(p.Yaw + a)
		walk(ox, oz, ox+d.X*clear*inv, oz+d.Z*clear*inv, func(ix, iz int32) {
			if t := e.at(ix, iz); t.mark>>1 != e.sweeps {
				t.mark = crossed
				if t.miss < math.MaxUint16 {
					t.miss++
				}
			}
		})
		if !hit || inDiscs(sw.Dynamic, geom.Vec2{X: p.X + d.X*r, Z: p.Z + d.Z*r}) {
			continue
		}
		t := e.at(floorCell(ox+d.X*r*inv), floorCell(oz+d.Z*r*inv))
		if t.mark == returned {
			continue
		}
		if t.mark == crossed {
			t.miss--
		}
		t.mark = returned
		if t.hit < math.MaxUint16 {
			t.hit++
		}
	}
	if e.from.IsZero() || sw.Time.Before(e.from) {
		e.from = sw.Time
	}
	if sw.Time.After(e.to) {
		e.to = sw.Time
	}
	// The floor's elevation is the one most sweeps were taken at: a device on
	// stairs or in a lift is on its floor before it is level with it.
	k := int32(math.Round(sw.Elevation * 10))
	e.levels[k]++
	if e.sweeps == 1 || e.levels[k] >= e.levels[e.level] {
		e.level, e.elev = k, sw.Elevation
	}
	if sw.Input.ID != "" {
		e.inputs = append(e.inputs, sw.Input)
		if len(e.inputs) > 8*spatial.MaxInputs {
			e.inputs = spatial.ReduceInputs(e.inputs)
		}
	}
	return nil
}

func inDiscs(discs []Disc, p geom.Vec2) bool {
	for _, d := range discs {
		if p.Dist(d.Centre) <= d.Radius {
			return true
		}
	}
	return false
}

// walk visits, in order, every cell the segment from (ax, az) to (bx, bz)
// crosses, in cell units, except the cell holding the end point. It is the
// traversal of grid.InsertScan, which the grid does not export.
func walk(ax, az, bx, bz float64, visit func(ix, iz int32)) {
	ix, iz := floorCell(ax), floorCell(az)
	nx, nz := int(floorCell(bx))-int(ix), int(floorCell(bz))-int(iz)
	stepX, tMaxX, tDeltaX := axis(ax, bx-ax, ix)
	stepZ, tMaxZ, tDeltaZ := axis(az, bz-az, iz)
	if nx < 0 {
		nx = -nx
	}
	if nz < 0 {
		nz = -nz
	}
	for nx+nz > 0 {
		visit(ix, iz)
		// The remaining counts decide the last steps, so rounding cannot
		// overshoot the end cell.
		if nz == 0 || (nx > 0 && tMaxX < tMaxZ) {
			ix += stepX
			tMaxX += tDeltaX
			nx--
		} else {
			iz += stepZ
			tMaxZ += tDeltaZ
			nz--
		}
	}
}

// axis sets up one axis of the traversal from position a (cell units, in cell
// i) moving by d: the index step, the ray parameter at the first boundary and
// the parameter between boundaries.
func axis(a, d float64, i int32) (step int32, tMax, tDelta float64) {
	switch {
	case d > 0:
		return 1, (float64(i) + 1 - a) / d, 1 / d
	case d < 0:
		return -1, (a - float64(i)) / -d, -1 / d
	}
	return 0, math.Inf(1), math.Inf(1)
}

// openAround reports whether every cell within reach cells of c is Free:
// open space the map is sure of. A Free cell with anything else beside it is
// on the frontier of what was seen, where a pose error of a cell or two lets
// beams grazing a surface cross the cells just inside it.
func (e *Evidence) openAround(c grid.Cell, reach int32) bool {
	for dz := -reach; dz <= reach; dz++ {
		for dx := -reach; dx <= reach; dx++ {
			if e.state(e.get(grid.Cell{IX: c.IX + dx, IZ: c.IZ + dz})) != Free {
				return false
			}
		}
	}
	return true
}

// adopt makes e the last confirmed state of every cell. A cell cur observed
// confidently takes cur's counts when e did not know it, agrees with it, or
// the difference was reported (the cell is in reported). A difference that
// was not reported stays a difference: adopting it would lose a change whose
// first sighting was too small to report.
// ponytail: sightings below Config.MinClusterCells are not added up across
// epochs, so a change that never shows MinClusterCells cells within one epoch
// is never reported. Carry unreported candidates forward to fix that.
func (e *Evidence) adopt(cur *Evidence, reported map[grid.Cell]bool) {
	for key, cc := range cur.chunks {
		ec := e.chunks[key]
		for i := range cc {
			s := cur.state(cc[i])
			if s != Free && s != Occupied {
				continue
			}
			if ec != nil {
				if rs := e.state(ec[i]); (rs == Free || rs == Occupied) && rs != s && !reported[cellOf(key, i)] {
					continue
				}
			}
			if ec == nil {
				ec = new(chunk)
				e.chunks[key] = ec
			}
			ec[i] = tally{hit: cc[i].hit, miss: cc[i].miss}
		}
	}
	e.lastChunk = nil
	if e.from.IsZero() || (!cur.from.IsZero() && cur.from.Before(e.from)) {
		e.from = cur.from
	}
	if cur.to.After(e.to) {
		e.to = cur.to
	}
	e.elev = cur.elev
	e.sweeps += cur.sweeps
	e.inputs = spatial.ReduceInputs(append(e.inputs, cur.inputs...))
}
