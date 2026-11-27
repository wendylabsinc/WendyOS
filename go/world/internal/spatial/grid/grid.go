// Package grid is a log-odds occupancy grid on the planar (x, z) map of the
// spatial layer, with the derived rasters scan matching needs.
//
// Cell (ix, iz) covers [ix·s, (ix+1)·s) × [iz·s, (iz+1)·s) metres for cell
// size s; its centre is ((ix+½)·s, (iz+½)·s). Storage is sparse in 32x32-cell
// chunks and grows wherever evidence lands.
//
// A cell holds its log-odds of being occupied as an integer multiple of
// LogOddsStep, clamped to [Config.Min, Config.Max]. Zero means unknown, as in
// spatial.MapTile: a cell whose evidence cancels exactly is indistinguishable
// from one never observed. Because the stored values are integers, Merge is
// exact addition: commutative always, and associative until a clamp engages.
// Sum is the order-independent fusion of several grids.
//
// Tile, Tiles, SetTile and FromTiles convert to and from spatial.MapTile with
// one fixed tiling and scale (TileLogOddsScale), so every publisher and
// consumer of map tiles agrees on them; with the default Config the
// conversion is lossless.
//
// Matching while mapping. The likelihood field (Field) follows the grid:
// after a scan is inserted, the next Field call folds in the cells that
// crossed the Occupied threshold. On a real map nearly every scan flips a few
// tens of cells (range noise against the cell lattice, people walking), so a
// match-insert loop pays for that on every scan. Measured on a 45 x 20 m
// office at 5 cm with 30 flips per scan: 0.8 ms per scan, most of it the
// copy of the raster, next to 0.4 ms for InsertScan itself; under the race
// detector 3.5 ms and 5 ms. A scan that moves the bounding box of the
// Occupied cells costs a full rebuild, about 3 ms on that map. A front end
// that matches every scan should therefore insert at keyframe rate, or match
// against a Clone taken at keyframe rate; a Clone shares the field already
// built.
//
// A Grid must not be mutated concurrently with any other use. Read-only
// methods, Field included, may run concurrently with each other.
package grid

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// LogOddsStep is the resolution of stored log-odds.
const LogOddsStep = 0.01

const (
	chunkBits  = 5
	chunkSide  = 1 << chunkBits
	chunkMask  = chunkSide - 1
	chunkCells = chunkSide * chunkSide

	// maxLogOdds keeps raw values and their sums inside int16.
	maxLogOdds = 300.0
	// maxIndex bounds cell indices so index arithmetic cannot overflow int32.
	maxIndex = 1 << 30
	// maxWindowCells bounds a dense raster (a scan footprint or a Field).
	maxWindowCells = 1 << 26
)

var (
	// ErrConfig reports a Config outside its documented ranges.
	ErrConfig = errors.New("grid: invalid config")
	// ErrInvalid reports a non-finite or out-of-range argument.
	ErrInvalid = errors.New("grid: invalid argument")
	// ErrIncompatible reports two grids with different cell sizes.
	ErrIncompatible = errors.New("grid: incompatible grids")
	// ErrTooLarge reports a dense raster that would exceed 2^26 cells: a scan
	// footprint (lower Config.MaxRange) or the bounding box of the occupied
	// cells (split the map).
	ErrTooLarge = errors.New("grid: raster too large")
	// ErrFormat reports bytes that are not a canonical encoded grid.
	ErrFormat = errors.New("grid: malformed encoding")
)

// Config holds the sensor model and thresholds. A zero field takes its
// default; DefaultConfig lists them.
type Config struct {
	// CellSize is the cell side in metres. Default 0.05.
	CellSize float64
	// Hit is the log-odds added to the cell holding a beam's return.
	// Default 0.85 (p = 0.70).
	Hit float64
	// Miss is the log-odds subtracted from each cell a beam crosses before
	// its return. Default 0.40 (p = 0.40).
	Miss float64
	// Min and Max clamp a cell's log-odds, which bounds how many contrary
	// observations it takes to flip a cell. Defaults −2 and 3.5.
	Min, Max float64
	// Occupied is the log-odds at or above which a cell is Occupied.
	// Default 0.5, so one return marks a cell.
	Occupied float64
	// Free is the log-odds at or below which a cell is Free. Default −0.3,
	// so one crossing beam frees an unknown cell.
	Free float64
	// MaxRange caps, in metres, how far a beam is integrated: a return
	// beyond it only clears free space up to it. Zero uses the scan's own
	// RangeMax.
	MaxRange float64
	// FieldDistance is the distance in metres at which the likelihood field
	// is clipped. Default 1.0.
	FieldDistance float64
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	return Config{CellSize: 0.05, Hit: 0.85, Miss: 0.40, Min: -2, Max: 3.5, Occupied: 0.5, Free: -0.3, FieldDistance: 1.0}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	for _, f := range []struct{ v, def *float64 }{
		{&c.CellSize, &d.CellSize}, {&c.Hit, &d.Hit}, {&c.Miss, &d.Miss}, {&c.Min, &d.Min}, {&c.Max, &d.Max},
		{&c.Occupied, &d.Occupied}, {&c.Free, &d.Free}, {&c.FieldDistance, &d.FieldDistance},
	} {
		if *f.v == 0 {
			*f.v = *f.def
		}
	}
	return c
}

func (c Config) validate() error {
	if !finite(c.CellSize, c.Hit, c.Miss, c.Min, c.Max, c.Occupied, c.Free, c.MaxRange, c.FieldDistance) {
		return fmt.Errorf("%w: non-finite field", ErrConfig)
	}
	switch {
	case c.CellSize <= 0:
		return fmt.Errorf("%w: CellSize must be positive", ErrConfig)
	case c.Hit < LogOddsStep || c.Miss < LogOddsStep:
		return fmt.Errorf("%w: Hit and Miss must be at least %g", ErrConfig, LogOddsStep)
	case c.Min < -maxLogOdds || c.Max > maxLogOdds:
		return fmt.Errorf("%w: clamp beyond ±%g", ErrConfig, maxLogOdds)
	case !(c.Min <= c.Free && c.Free <= -LogOddsStep && c.Occupied >= LogOddsStep && c.Occupied <= c.Max):
		return fmt.Errorf("%w: need Min ≤ Free < 0 < Occupied ≤ Max", ErrConfig)
	case c.MaxRange < 0 || c.FieldDistance < c.CellSize:
		return fmt.Errorf("%w: MaxRange must be ≥ 0 and FieldDistance ≥ CellSize", ErrConfig)
	}
	return nil
}

// Cell indexes one grid cell.
type Cell struct {
	IX int32 `json:"ix"`
	IZ int32 `json:"iz"`
}

// State classifies a cell against Config.Occupied and Config.Free. Unknown
// covers both unobserved cells and evidence between the two thresholds.
type State uint8

const (
	Unknown State = iota
	Free
	Occupied
)

func (s State) String() string {
	return [...]string{"unknown", "free", "occupied"}[s]
}

type chunkKey struct{ x, z int32 }

type chunk [chunkCells]int16

// Grid is a sparse log-odds occupancy grid.
type Grid struct {
	cfg                          Config
	hit, miss, lo, hi, occ, free int // raw units of LogOddsStep
	chunks                       map[chunkKey]*chunk

	mu    sync.Mutex // guards field and flips
	field *Field     // the field as of the last build; nil when it must be rebuilt
	flips []Cell     // cells that crossed the Occupied threshold since then

	mark    []uint8 // InsertScan scratch, all zero between calls
	touched []int32
}

// New returns an empty grid.
func New(cfg Config) (*Grid, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Grid{
		cfg: cfg,
		hit: toRaw(cfg.Hit), miss: toRaw(cfg.Miss), lo: toRaw(cfg.Min), hi: toRaw(cfg.Max),
		occ: toRaw(cfg.Occupied), free: toRaw(cfg.Free),
		chunks: map[chunkKey]*chunk{},
	}, nil
}

// Config returns the configuration with defaults filled in.
func (g *Grid) Config() Config { return g.cfg }

// CellSize is the cell side in metres.
func (g *Grid) CellSize() float64 { return g.cfg.CellSize }

func toRaw(logOdds float64) int { return int(math.Round(logOdds / LogOddsStep)) }

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func cellIndex(v float64) int32 {
	f := math.Floor(v)
	if !(f > -maxIndex) {
		return -maxIndex
	}
	if f > maxIndex {
		return maxIndex
	}
	return int32(f)
}

func local(ix, iz int32) int { return int(iz&chunkMask)<<chunkBits | int(ix&chunkMask) }

// CellAt returns the cell containing p. Coordinates beyond ±2^30 cells
// saturate.
func (g *Grid) CellAt(p geom.Vec2) Cell {
	return Cell{cellIndex(p.X / g.cfg.CellSize), cellIndex(p.Z / g.cfg.CellSize)}
}

// Center is the centre of c in metres.
func (g *Grid) Center(c Cell) geom.Vec2 {
	return geom.Vec2{X: (float64(c.IX) + 0.5) * g.cfg.CellSize, Z: (float64(c.IZ) + 0.5) * g.cfg.CellSize}
}

func (g *Grid) raw(c Cell) int {
	ch := g.chunks[chunkKey{c.IX >> chunkBits, c.IZ >> chunkBits}]
	if ch == nil {
		return 0
	}
	return int(ch[local(c.IX, c.IZ)])
}

// cursor caches the last chunk touched by a run of nearby cell accesses.
type cursor struct {
	g   *Grid
	key chunkKey
	ch  *chunk
	set bool
}

func (cu *cursor) chunk(ix, iz int32, create bool) *chunk {
	key := chunkKey{ix >> chunkBits, iz >> chunkBits}
	if !cu.set || key != cu.key || (cu.ch == nil && create) {
		ch := cu.g.chunks[key]
		if ch == nil && create {
			ch = new(chunk)
			cu.g.chunks[key] = ch
		}
		cu.key, cu.ch, cu.set = key, ch, true
	}
	return cu.ch
}

func (cu *cursor) get(ix, iz int32) int {
	if ch := cu.chunk(ix, iz, false); ch != nil {
		return int(ch[local(ix, iz)])
	}
	return 0
}

// add adds delta to a cell and reports whether that moved it across the
// Occupied threshold, which is all the likelihood field depends on.
func (cu *cursor) add(ix, iz int32, delta int) (flipped bool) {
	ch := cu.chunk(ix, iz, true)
	i := local(ix, iz)
	old := int(ch[i])
	v := min(max(old+delta, cu.g.lo), cu.g.hi)
	ch[i] = int16(v)
	return (old >= cu.g.occ) != (v >= cu.g.occ)
}

// maxPendingFlips bounds the changes remembered for an incremental field
// update; beyond it the next Field call rebuilds from scratch.
const maxPendingFlips = 4096

// stale records cells that crossed the Occupied threshold, which is all the
// likelihood field depends on. The next Field call folds them in.
func (g *Grid) stale(flips ...Cell) {
	if len(flips) == 0 {
		return
	}
	g.mu.Lock()
	switch {
	case g.field == nil:
	case len(g.flips)+len(flips) > maxPendingFlips:
		g.field, g.flips = nil, nil
	default:
		g.flips = append(g.flips, flips...)
	}
	g.mu.Unlock()
}

func (g *Grid) state(raw int) State {
	switch {
	case raw >= g.occ:
		return Occupied
	case raw <= g.free:
		return Free
	}
	return Unknown
}

// LogOdds is the log-odds of c being occupied; 0 for an unknown cell.
func (g *Grid) LogOdds(c Cell) float64 { return float64(g.raw(c)) * LogOddsStep }

// LogOddsAt is LogOdds of the cell containing p.
func (g *Grid) LogOddsAt(p geom.Vec2) float64 { return g.LogOdds(g.CellAt(p)) }

// Probability is the occupancy probability of c; 0.5 for an unknown cell.
func (g *Grid) Probability(c Cell) float64 { return 1 / (1 + math.Exp(-g.LogOdds(c))) }

// ProbabilityAt is Probability of the cell containing p.
func (g *Grid) ProbabilityAt(p geom.Vec2) float64 { return g.Probability(g.CellAt(p)) }

// Known reports whether c holds any evidence.
func (g *Grid) Known(c Cell) bool { return g.raw(c) != 0 }

// State classifies c.
func (g *Grid) State(c Cell) State { return g.state(g.raw(c)) }

// StateAt classifies the cell containing p.
func (g *Grid) StateAt(p geom.Vec2) State { return g.State(g.CellAt(p)) }

func validCell(c Cell) bool {
	return c.IX >= -maxIndex && c.IX <= maxIndex && c.IZ >= -maxIndex && c.IZ <= maxIndex
}

// Set overwrites the log-odds of c, rounded to LogOddsStep and clamped.
// Setting 0 makes the cell unknown.
func (g *Grid) Set(c Cell, logOdds float64) error {
	if !finite(logOdds) || !validCell(c) {
		return ErrInvalid
	}
	raw := min(max(toRaw(math.Max(-2*maxLogOdds, math.Min(2*maxLogOdds, logOdds))), g.lo), g.hi)
	old := g.raw(c)
	if raw == old {
		return nil
	}
	cu := cursor{g: g}
	cu.chunk(c.IX, c.IZ, true)[local(c.IX, c.IZ)] = int16(raw)
	if (old >= g.occ) != (raw >= g.occ) {
		g.stale(c)
	}
	return nil
}

// Add adds delta to the log-odds of c, rounded to LogOddsStep, and clamps
// the sum.
func (g *Grid) Add(c Cell, delta float64) error {
	if !finite(delta) || !validCell(c) {
		return ErrInvalid
	}
	raw := toRaw(math.Max(-2*maxLogOdds, math.Min(2*maxLogOdds, delta)))
	if raw == 0 {
		return nil
	}
	cu := cursor{g: g}
	if cu.add(c.IX, c.IZ, raw) {
		g.stale(c)
	}
	return nil
}

func (g *Grid) sortedKeys() []chunkKey {
	keys := make([]chunkKey, 0, len(g.chunks))
	for k := range g.chunks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].z != keys[j].z {
			return keys[i].z < keys[j].z
		}
		return keys[i].x < keys[j].x
	})
	return keys
}

// Each calls fn for every known cell until fn returns false. The order is
// fixed: by 32-cell chunk (chunk row, then chunk column), and row-major
// (iz, then ix) inside a chunk.
func (g *Grid) Each(fn func(c Cell, logOdds float64) bool) {
	for _, k := range g.sortedKeys() {
		for i, v := range g.chunks[k] {
			if v == 0 {
				continue
			}
			c := Cell{k.x<<chunkBits | int32(i&chunkMask), k.z<<chunkBits | int32(i>>chunkBits)}
			if !fn(c, float64(v)*LogOddsStep) {
				return
			}
		}
	}
}

// Counts tallies the known cells by class. Known − Occupied − Free cells
// hold evidence between the thresholds.
type Counts struct {
	Known, Occupied, Free int
}

// Counts returns the tallies over the whole grid.
func (g *Grid) Counts() Counts {
	var n Counts
	for _, ch := range g.chunks {
		for _, v := range ch {
			if v == 0 {
				continue
			}
			n.Known++
			switch g.state(int(v)) {
			case Occupied:
				n.Occupied++
			case Free:
				n.Free++
			}
		}
	}
	return n
}

// bounds returns the inclusive cell bounds of the cells accepted by keep.
func (g *Grid) bounds(keep func(raw int) bool) (lo, hi Cell, ok bool) {
	for k, ch := range g.chunks {
		for i, v := range ch {
			if v == 0 || !keep(int(v)) {
				continue
			}
			c := Cell{k.x<<chunkBits | int32(i&chunkMask), k.z<<chunkBits | int32(i>>chunkBits)}
			if !ok {
				lo, hi, ok = c, c, true
				continue
			}
			lo = Cell{min(lo.IX, c.IX), min(lo.IZ, c.IZ)}
			hi = Cell{max(hi.IX, c.IX), max(hi.IZ, c.IZ)}
		}
	}
	return lo, hi, ok
}

// Bounds returns the inclusive bounds of the known cells; ok is false for an
// empty grid.
func (g *Grid) Bounds() (lo, hi Cell, ok bool) {
	return g.bounds(func(int) bool { return true })
}

// BoundsM returns the metric rectangle covered by the known cells.
func (g *Grid) BoundsM() (lo, hi geom.Vec2, ok bool) {
	a, b, ok := g.Bounds()
	if !ok {
		return geom.Vec2{}, geom.Vec2{}, false
	}
	s := g.cfg.CellSize
	return geom.Vec2{X: float64(a.IX) * s, Z: float64(a.IZ) * s}, geom.Vec2{X: float64(b.IX+1) * s, Z: float64(b.IZ+1) * s}, true
}

// Clone returns an independent copy.
func (g *Grid) Clone() *Grid {
	out := &Grid{cfg: g.cfg, hit: g.hit, miss: g.miss, lo: g.lo, hi: g.hi, occ: g.occ, free: g.free, chunks: make(map[chunkKey]*chunk, len(g.chunks))}
	for k, ch := range g.chunks {
		cp := *ch
		out.chunks[k] = &cp
	}
	g.mu.Lock()
	out.field, out.flips = g.field, slices.Clone(g.flips) // a Field is immutable once built
	g.mu.Unlock()
	return out
}

func (g *Grid) compatible(o *Grid) error {
	if o == nil || g.cfg.CellSize != o.cfg.CellSize {
		return ErrIncompatible
	}
	return nil
}

// Merge adds other's log-odds into g cell by cell and clamps each sum to g's
// range. The cell sizes must be equal. Merging is commutative, and
// associative as long as no sum reaches a clamp; it is not idempotent. Use
// Sum when the result must not depend on the order of the parts.
func (g *Grid) Merge(other *Grid) error {
	if err := g.compatible(other); err != nil {
		return err
	}
	var flipped []Cell
	for k, src := range other.chunks {
		dst := g.chunks[k]
		for i, v := range src {
			if v == 0 {
				continue
			}
			if dst == nil {
				dst = new(chunk)
				g.chunks[k] = dst
			}
			old := int(dst[i])
			sum := min(max(old+int(v), g.lo), g.hi)
			dst[i] = int16(sum)
			if (old >= g.occ) != (sum >= g.occ) {
				flipped = append(flipped, Cell{k.x<<chunkBits | int32(i&chunkMask), k.z<<chunkBits | int32(i>>chunkBits)})
			}
		}
	}
	g.stale(flipped...)
	return nil
}

// Delta is one cell whose log-odds differ between two grids.
type Delta struct {
	Cell Cell
	From float64 // log-odds in the receiver
	To   float64 // log-odds in the argument
}

// Diff lists the cells whose log-odds differ between g and other, ordered by
// (IZ, IX). The cell sizes must be equal.
func (g *Grid) Diff(other *Grid) ([]Delta, error) {
	if err := g.compatible(other); err != nil {
		return nil, err
	}
	var out []Delta
	scan := func(a, b *Grid, onlyMissing bool) {
		for k, ch := range a.chunks {
			och := b.chunks[k]
			if onlyMissing && och != nil {
				continue
			}
			for i, v := range ch {
				var w int16
				if och != nil {
					w = och[i]
				}
				if v == w {
					continue
				}
				d := Delta{Cell: Cell{k.x<<chunkBits | int32(i&chunkMask), k.z<<chunkBits | int32(i>>chunkBits)}, From: float64(v) * LogOddsStep, To: float64(w) * LogOddsStep}
				if onlyMissing {
					d.From, d.To = d.To, d.From
				}
				out = append(out, d)
			}
		}
	}
	scan(g, other, false)
	scan(other, g, true)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cell.IZ != out[j].Cell.IZ {
			return out[i].Cell.IZ < out[j].Cell.IZ
		}
		return out[i].Cell.IX < out[j].Cell.IX
	})
	return out, nil
}
