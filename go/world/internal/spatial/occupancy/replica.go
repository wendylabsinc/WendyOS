package occupancy

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	// Feature names this package in provenance: ModelID "wendy.spatial.occupancy".
	Feature = "occupancy"
	// ModelVersion is the provenance model version of the records built here.
	ModelVersion = "1"
)

var (
	// ErrConfig reports a Config outside its documented ranges.
	ErrConfig = errors.New("occupancy: invalid config")
	// ErrInvalid reports a malformed layer, pose, scan or calibration.
	ErrInvalid = errors.New("occupancy: invalid argument")
	// ErrIncompatible reports a layer on another tiling, cell size or frame
	// than the replica's.
	ErrIncompatible = errors.New("occupancy: incompatible tiling")
	// ErrNoEvidence reports a tile or floor no source has written.
	ErrNoEvidence = errors.New("occupancy: no evidence")
	// ErrTooManySources reports a tile with more sources than a MapTile
	// version vector holds (spatial.MaxTileSources).
	ErrTooManySources = errors.New("occupancy: too many sources for one tile")
	// ErrForeign reports a measurement, pose or calibration of another device
	// than the Mapper's source.
	ErrForeign = errors.New("occupancy: not this source's device")
	// ErrFrame reports a pose expressed in another frame than Config.FrameID.
	ErrFrame = errors.New("occupancy: pose in another frame")
	// ErrUncertain reports a pose too uncertain to map with.
	ErrUncertain = errors.New("occupancy: pose too uncertain")
	// ErrNoPose reports a scan with no pose samples bracketing its instant.
	ErrNoPose = errors.New("occupancy: no pose for the scan")
)

// Config holds the tiling, the sensor model and the gates of a Mapper. A zero
// field takes its default; DefaultConfig lists them.
type Config struct {
	// Grid is the sensor model and clamp of every source's map and the clamp
	// of the fused map. Default grid.DefaultConfig(), which tiles encode
	// without loss. Min and Max must reach one tile step
	// (grid.TileLogOddsScale); a layer saturates at 127 steps (6.35).
	Grid grid.Config
	// TileSide is the tile side in cells, at most spatial.MaxTileSide.
	// Default 64: 3.2 m tiles of 4 KiB at the default cell size, small enough
	// that a robot rewrites a few tens of them per sweep.
	TileSide int
	// FrameID is the frame the map is expressed in. Poses in any other frame
	// are refused, and so are layer components published in one. Default
	// "site".
	FrameID string
	// MaxPoseUncertaintyM is the largest DevicePose.UncertaintyM a scan is
	// integrated at. Default 0.25 m, five default cells: beyond it a scan
	// smears walls wider than doorways are resolved.
	MaxPoseUncertaintyM float64
	// MaxPoseGap is the longest interval between two pose samples across
	// which a scan pose is interpolated. Default 500 ms.
	MaxPoseGap time.Duration
	// PoseHistory is how far behind the newest pose sample a scan may arrive
	// and still be placed. Default 2 s.
	PoseHistory time.Duration
	// MaxPending bounds the scans waiting for a later pose sample; the oldest
	// is refused when it overflows. Default 64.
	MaxPending int
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	return Config{
		Grid: grid.DefaultConfig(), TileSide: 64, FrameID: "site",
		MaxPoseUncertaintyM: 0.25, MaxPoseGap: 500 * time.Millisecond, PoseHistory: 2 * time.Second, MaxPending: 64,
	}
}

// resolve fills defaults, validates and returns the fused clamp in tile steps.
func (c Config) resolve() (Config, int, int, error) {
	d := DefaultConfig()
	if c.TileSide == 0 {
		c.TileSide = d.TileSide
	}
	if c.FrameID == "" {
		c.FrameID = d.FrameID
	}
	if c.MaxPoseUncertaintyM == 0 {
		c.MaxPoseUncertaintyM = d.MaxPoseUncertaintyM
	}
	if c.MaxPoseGap == 0 {
		c.MaxPoseGap = d.MaxPoseGap
	}
	if c.PoseHistory == 0 {
		c.PoseHistory = d.PoseHistory
	}
	if c.MaxPending == 0 {
		c.MaxPending = d.MaxPending
	}
	g, err := grid.New(c.Grid)
	if err != nil {
		return c, 0, 0, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	c.Grid = g.Config()
	// Truncation toward zero keeps the fused clamp inside [Min, Max].
	lo := max(int(math.Round(c.Grid.Min/grid.LogOddsStep))/tileStep, -127)
	hi := min(int(math.Round(c.Grid.Max/grid.LogOddsStep))/tileStep, 127)
	switch {
	case c.TileSide < 1 || c.TileSide > spatial.MaxTileSide:
		return c, 0, 0, fmt.Errorf("%w: TileSide must be in [1, %d]", ErrConfig, spatial.MaxTileSide)
	case lo > -1 || hi < 1:
		return c, 0, 0, fmt.Errorf("%w: Grid.Min and Grid.Max must reach ±%g", ErrConfig, grid.TileLogOddsScale)
	case math.IsNaN(c.MaxPoseUncertaintyM) || c.MaxPoseUncertaintyM < 0 || c.MaxPoseGap < 0 || c.PoseHistory < 0 || c.MaxPending < 1:
		return c, 0, 0, fmt.Errorf("%w: pose gates must be non-negative and MaxPending positive", ErrConfig)
	}
	return c, lo, hi, nil
}

// tileStep is one tile step in units of grid.LogOddsStep.
const tileStep = int(grid.TileLogOddsScale / grid.LogOddsStep)

// LayerKey names one layer: the evidence of one source for one tile of one
// floor.
type LayerKey struct {
	Floor  spatial.FloorID  `json:"floor"`
	IX     int32            `json:"ix"`
	IZ     int32            `json:"iz"`
	Source spatial.DeviceID `json:"source"`
}

// Compare orders keys by floor, tile row, tile column and source: the order
// of every listing in this package.
func (k LayerKey) Compare(o LayerKey) int {
	return cmp.Or(cmp.Compare(k.Floor, o.Floor), cmp.Compare(k.IZ, o.IZ), cmp.Compare(k.IX, o.IX), cmp.Compare(k.Source, o.Source))
}

// Layer is one version of a source's evidence for a tile. Cells holds one
// int8 per cell, row-major with index iz·side+ix, in steps of
// grid.TileLogOddsScale, 0 meaning unknown: the cells of a spatial.MapTile.
// Cells and Inputs are shared between a replica and the layers it hands out
// and must not be modified.
type Layer struct {
	LayerKey
	// Version orders the rewrites of this layer by its source; it starts at 1.
	Version uint64
	// CellSizeM is the cell side in metres.
	CellSizeM float64
	// ElevationM is the source's estimate of the floor's World Y.
	ElevationM float64
	// Time is the site time of the latest scan in the layer.
	Time  time.Time
	Cells []byte
	// Inputs is the ancestry of the layer: the scans it was built from,
	// reduced by spatial.ReduceInputs.
	Inputs []world.Input
}

// Side is the tile side in cells, 0 when Cells is not a square.
func (l Layer) Side() int {
	side := int(math.Round(math.Sqrt(float64(len(l.Cells)))))
	if side*side != len(l.Cells) {
		return 0
	}
	return side
}

// Tile is the layer as a sealed spatial.MapTile whose version vector names
// the one source. The tile shares the layer's cells.
func (l Layer) Tile() (spatial.MapTile, error) {
	side := l.Side()
	if side == 0 {
		return spatial.MapTile{}, fmt.Errorf("%w: %d cells are not a square tile", ErrInvalid, len(l.Cells))
	}
	t := spatial.MapTile{
		Floor: l.Floor, IX: l.IX, IZ: l.IZ, Side: side, CellSizeM: l.CellSizeM, OriginXZ: spatial.TileOrigin(l.IX, l.IZ, side, l.CellSizeM),
		LogOddsScale: grid.TileLogOddsScale, Cells: l.Cells, Versions: map[string]uint64{string(l.Source): l.Version},
	}
	t.Seal(l.ElevationM)
	if err := t.Validate(); err != nil {
		return spatial.MapTile{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return t, nil
}

// entry is a validated layer with the hash of its content.
type entry struct {
	Layer
	sum [sha256.Size]byte
}

// tag is the part of the content hash that breaks version ties.
func (e *entry) tag() uint64 { return binary.BigEndian.Uint64(e.sum[:8]) }

// newer is the total order merge keeps the greatest of.
func newer(version, tag, thanVersion, thanTag uint64) bool {
	return version > thanVersion || (version == thanVersion && tag > thanTag)
}

type tileKey struct {
	floor  spatial.FloorID
	ix, iz int32
}

// Replica is one copy of the distributed map: the newest layer it has seen
// for every (floor, tile, source).
type Replica struct {
	cfg    Config
	lo, hi int // fused clamp in tile steps
	tiles  map[tileKey]map[spatial.DeviceID]*entry
}

// NewReplica returns an empty replica.
func NewReplica(cfg Config) (*Replica, error) {
	cfg, lo, hi, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	return &Replica{cfg: cfg, lo: lo, hi: hi, tiles: map[tileKey]map[spatial.DeviceID]*entry{}}, nil
}

// Config returns the configuration with defaults filled in.
func (r *Replica) Config() Config { return r.cfg }

// check validates a layer against the replica's tiling and normalizes it.
func (r *Replica) check(l Layer) (*entry, error) {
	if l.Side() != r.cfg.TileSide || l.CellSizeM != r.cfg.Grid.CellSize {
		return nil, fmt.Errorf("%w: layer of %d cells of %g m, replica tiles are %d x %d cells of %g m",
			ErrIncompatible, len(l.Cells), l.CellSizeM, r.cfg.TileSide, r.cfg.TileSide, r.cfg.Grid.CellSize)
	}
	if l.Time.IsZero() {
		return nil, fmt.Errorf("%w: layer without a time", ErrInvalid)
	}
	if _, err := l.Tile(); err != nil { // floor, source, version, geometry, elevation
		return nil, err
	}
	l.Time = spatial.Stamp(l.Time.UnixNano())
	l.Inputs = spatial.ReduceInputs(l.Inputs)
	if _, err := spatial.InferredProvenance(Feature, ModelVersion, ModelVersion, l.Inputs); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	inputs, err := json.Marshal(l.Inputs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var head [24]byte
	binary.BigEndian.PutUint64(head[0:], l.Version)
	binary.BigEndian.PutUint64(head[8:], uint64(l.Time.UnixNano()))
	binary.BigEndian.PutUint64(head[16:], math.Float64bits(l.ElevationM))
	h := sha256.New()
	h.Write(head[:])
	h.Write(l.Cells)
	h.Write(inputs)
	e := &entry{Layer: l}
	h.Sum(e.sum[:0])
	return e, nil
}

func (r *Replica) entry(k LayerKey) *entry {
	return r.tiles[tileKey{k.Floor, k.IX, k.IZ}][k.Source]
}

func (r *Replica) put(e *entry) bool {
	if cur := r.entry(e.LayerKey); cur != nil && !newer(e.Version, e.tag(), cur.Version, cur.tag()) {
		return false
	}
	k := tileKey{e.Floor, e.IX, e.IZ}
	if r.tiles[k] == nil {
		r.tiles[k] = map[spatial.DeviceID]*entry{}
	}
	r.tiles[k][e.Source] = e
	return true
}

// Apply merges one layer: it replaces the layer held for the same key when
// it is newer, by version and then by content. It reports whether the state
// changed; an older or equal layer is not an error. A layer that is
// malformed, lacks a time or input ancestry (ErrInvalid) or is on another
// tiling (ErrIncompatible) is rejected and changes nothing.
func (r *Replica) Apply(l Layer) (bool, error) {
	e, err := r.check(l)
	if err != nil {
		return false, err
	}
	return r.put(e), nil
}

// Merge applies every layer of other and returns how many changed r. Merging
// is commutative, associative and idempotent. The replicas must share tile
// side and cell size (ErrIncompatible).
func (r *Replica) Merge(other *Replica) (int, error) {
	if other.cfg.TileSide != r.cfg.TileSide || other.cfg.Grid.CellSize != r.cfg.Grid.CellSize {
		return 0, ErrIncompatible
	}
	n := 0
	for _, sources := range other.tiles {
		for _, e := range sources {
			if r.put(e) {
				n++
			}
		}
	}
	return n, nil
}

// entries lists the layers held, ordered by key.
func (r *Replica) entries() []*entry {
	var out []*entry
	for _, sources := range r.tiles {
		for _, e := range sources {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *entry) int { return a.LayerKey.Compare(b.LayerKey) })
	return out
}

// Layer returns the layer held for a key.
func (r *Replica) Layer(k LayerKey) (Layer, bool) {
	if e := r.entry(k); e != nil {
		return e.Layer, true
	}
	return Layer{}, false
}

// LayerVersion is one line of a Summary. Tag tells apart two layers a source
// wrongly wrote with one version.
type LayerVersion struct {
	LayerKey
	Version uint64 `json:"version"`
	Tag     uint64 `json:"tag"`
}

// Summary is what a replica holds: one line per layer, ordered by key. It is
// the request of a delta sync. A line is about 100 bytes of JSON, so the
// summary of a large site is split by the transport, by floor or key range;
// any subset is a valid request (see Delta).
type Summary []LayerVersion

// Summary lists the version of every layer held.
func (r *Replica) Summary() Summary {
	es := r.entries()
	out := make(Summary, len(es))
	for i, e := range es {
		out[i] = LayerVersion{LayerKey: e.LayerKey, Version: e.Version, Tag: e.tag()}
	}
	return out
}

// Delta answers a peer's summary with exactly the layers r holds that the
// peer lacks or holds older, ordered by key. Delta(nil) is every layer. A
// layer missing from a partial summary is sent again, which is harmless.
func (r *Replica) Delta(peer Summary) []Layer {
	have := make(map[LayerKey]LayerVersion, len(peer))
	for _, v := range peer {
		if cur, ok := have[v.LayerKey]; !ok || newer(v.Version, v.Tag, cur.Version, cur.Tag) {
			have[v.LayerKey] = v
		}
	}
	var out []Layer
	for _, e := range r.entries() {
		if v, ok := have[e.LayerKey]; !ok || newer(e.Version, e.tag(), v.Version, v.Tag) {
			out = append(out, e.Layer)
		}
	}
	return out
}

// Digest is the hex SHA-256 of the whole state. Replicas that have merged
// the same layers have equal digests, whatever the order.
func (r *Replica) Digest() string {
	h := sha256.New()
	var n [8]byte
	text := func(s string) {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	for _, e := range r.entries() {
		text(string(e.Floor))
		text(string(e.Source))
		binary.BigEndian.PutUint32(n[:4], uint32(e.IX))
		binary.BigEndian.PutUint32(n[4:], uint32(e.IZ))
		h.Write(n[:])
		h.Write(e.sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Floors lists the floors with at least one layer, sorted.
func (r *Replica) Floors() []spatial.FloorID {
	var out []spatial.FloorID
	for k := range r.tiles {
		out = append(out, k.floor)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// sources returns the layers of one tile ordered by source.
func (r *Replica) sources(k tileKey) []*entry {
	var out []*entry
	for _, e := range r.tiles[k] {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b *entry) int { return cmp.Compare(a.Source, b.Source) })
	return out
}

// Tile is the fused tile (ix, iz) of a floor: each cell is the sum of the
// sources' layers clamped to the range of Config.Grid, Versions lists every
// source's layer version and the elevation is the median of the sources'.
// It is ErrNoEvidence for a tile no source wrote and ErrTooManySources
// beyond spatial.MaxTileSources.
//
// ponytail: every source counts the same however old its layer is, so an
// obstacle one source saw at full confidence (Grid.Max) and never revisited
// outweighs one other source that later sees the cell free (Grid.Min); it
// takes two. Weigh layers by Layer.Time, or retire the layers of sources
// that left, when stale evidence shows up in a trial.
func (r *Replica) Tile(floor spatial.FloorID, ix, iz int32) (spatial.MapTile, error) {
	layers := r.sources(tileKey{floor, ix, iz})
	if len(layers) == 0 {
		return spatial.MapTile{}, fmt.Errorf("%w: tile %s/%d/%d", ErrNoEvidence, floor, ix, iz)
	}
	// ponytail: a tile fuses at most 64 sources, the MapTile version vector;
	// beyond that, fold retired sources into one archive source.
	if len(layers) > spatial.MaxTileSources {
		return spatial.MapTile{}, fmt.Errorf("%w: %d on tile %s/%d/%d", ErrTooManySources, len(layers), floor, ix, iz)
	}
	side := r.cfg.TileSide
	total := make([]int, side*side)
	versions := make(map[string]uint64, len(layers))
	elevations := make([]float64, len(layers))
	for i, e := range layers {
		versions[string(e.Source)], elevations[i] = e.Version, e.ElevationM
		for j, b := range e.Cells {
			total[j] += int(int8(b))
		}
	}
	slices.Sort(elevations)
	t := spatial.MapTile{
		Floor: floor, IX: ix, IZ: iz, Side: side, CellSizeM: r.cfg.Grid.CellSize, OriginXZ: spatial.TileOrigin(ix, iz, side, r.cfg.Grid.CellSize),
		LogOddsScale: grid.TileLogOddsScale, Cells: make([]byte, side*side), Versions: versions,
	}
	for j, v := range total {
		t.Cells[j] = byte(int8(min(max(v, r.lo), r.hi)))
	}
	t.Seal(elevations[len(elevations)/2])
	if err := t.Validate(); err != nil {
		return spatial.MapTile{}, err
	}
	return t, nil
}

// Tiles is every fused tile of a floor, ordered by (IZ, IX); empty for a
// floor without layers.
func (r *Replica) Tiles(floor spatial.FloorID) ([]spatial.MapTile, error) {
	var keys []tileKey
	for k := range r.tiles {
		if k.floor == floor {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b tileKey) int { return cmp.Or(cmp.Compare(a.iz, b.iz), cmp.Compare(a.ix, b.ix)) })
	out := make([]spatial.MapTile, 0, len(keys))
	for _, k := range keys {
		t, err := r.Tile(floor, k.ix, k.iz)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Grid is the fused map of a floor as a grid, for ray casting, matching and
// classification. Each cell carries its own uncertainty as log-odds; a cell
// no source observed, or whose evidence cancels, is unknown. A floor without
// layers is ErrNoEvidence.
func (r *Replica) Grid(floor spatial.FloorID) (*grid.Grid, error) {
	tiles, err := r.Tiles(floor)
	if err != nil {
		return nil, err
	}
	if len(tiles) == 0 {
		return nil, fmt.Errorf("%w: floor %s", ErrNoEvidence, floor)
	}
	return grid.FromTiles(r.cfg.Grid, tiles)
}
