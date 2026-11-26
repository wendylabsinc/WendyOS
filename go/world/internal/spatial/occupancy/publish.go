package occupancy

import (
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Publication is what a component needs beyond its payload. Frame must be
// the frame the map is expressed in (Config.FrameID).
type Publication struct {
	// Scope is the tenant, site and source of the component key. A layer is
	// a source-owned register: Scope.Source.Device must be the layer's source.
	Scope spatial.Scope
	// Epoch is the component version epoch; the sequence comes from the
	// layer versions.
	Epoch uint64
	Frame world.FrameRef
	// Timing supplies the clock domain and error bound; SampleTime is
	// replaced by the time of the newest scan in the tile.
	Timing     world.Timing
	TTL        time.Duration
	AppVersion string
}

func (p Publication) component(t spatial.MapTile, sequence uint64, at time.Time, inputs []world.Input) (world.Component, error) {
	prov, err := spatial.InferredProvenance(Feature, ModelVersion, p.AppVersion, spatial.ReduceInputs(inputs))
	if err != nil {
		return world.Component{}, err
	}
	timing := p.Timing
	timing.SampleTime = at
	return spatial.ToComponent(p.Scope, world.Version{Epoch: p.Epoch, Sequence: sequence}, p.Frame, timing, p.TTL, prov, t)
}

// Component is the layer as the component its source publishes and peers
// relay in a delta sync: the MapTile of Tile under the source's own key, with
// the layer version as the sequence, the layer time as the sample time and
// the layer's scans as ancestry.
func (l Layer) Component(p Publication) (world.Component, error) {
	if p.Scope.Source.Device != string(l.Source) {
		return world.Component{}, fmt.Errorf("%w: layer of %q published as %q", ErrForeign, l.Source, p.Scope.Source.Device)
	}
	t, err := l.Tile()
	if err != nil {
		return world.Component{}, err
	}
	return p.component(t, l.Version, l.Time, l.Inputs)
}

// ApplyComponent decodes a layer component and applies it as Apply does. It
// rejects a component in another frame than Config.FrameID or at another
// log-odds scale (ErrIncompatible) and one that is not a single source's
// layer under that source's own key (ErrInvalid), such as a fused tile.
func (r *Replica) ApplyComponent(c world.Component) (bool, error) {
	t, err := spatial.FromComponent[spatial.MapTile](c)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if c.Frame.ID != r.cfg.FrameID || t.LogOddsScale != grid.TileLogOddsScale {
		return false, fmt.Errorf("%w: tile in frame %q at scale %g", ErrIncompatible, c.Frame.ID, t.LogOddsScale)
	}
	version, own := t.Versions[c.Key.Source.Device]
	if len(t.Versions) != 1 || !own || version != c.Version.Sequence {
		return false, fmt.Errorf("%w: not a layer of source %q", ErrInvalid, c.Key.Source.Device)
	}
	return r.Apply(Layer{
		LayerKey: LayerKey{Floor: t.Floor, IX: t.IX, IZ: t.IZ, Source: spatial.DeviceID(c.Key.Source.Device)},
		Version:  version, CellSizeM: t.CellSizeM, ElevationM: t.PositionM[1], Time: c.Timing.SampleTime, Cells: t.Cells, Inputs: c.Provenance.Inputs,
	})
}

// Component is the fused tile (ix, iz) of a floor as the component a map
// publisher emits: the MapTile of Tile, the newest layer time as the sample
// time and the union of the layers' ancestry, reduced. The sequence is the
// sum of the layer versions, which only grows as the replica merges.
func (r *Replica) Component(p Publication, floor spatial.FloorID, ix, iz int32) (world.Component, error) {
	if p.Frame.ID != r.cfg.FrameID {
		return world.Component{}, fmt.Errorf("%w: map in frame %q published in %q", ErrIncompatible, r.cfg.FrameID, p.Frame.ID)
	}
	t, err := r.Tile(floor, ix, iz)
	if err != nil {
		return world.Component{}, err
	}
	var (
		sequence uint64
		at       time.Time
		inputs   []world.Input
	)
	for _, e := range r.sources(tileKey{floor, ix, iz}) {
		sequence += e.Version
		if e.Time.After(at) {
			at = e.Time
		}
		inputs = append(inputs, e.Inputs...)
	}
	return p.component(t, sequence, at, inputs)
}
