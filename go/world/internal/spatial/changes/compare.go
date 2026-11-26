package changes

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Change is one detected change with what publishing it needs.
type Change struct {
	Event spatial.ChangeEvent
	// Revision is 1 when the change is first reported and grows by one each
	// time later evidence revises it; it is the component sequence.
	Revision uint64
	// Inputs is the ancestry: the scans of the reference and of every epoch
	// that showed the change, reduced to spatial.MaxInputs.
	Inputs []world.Input
}

// Publication is what a component needs beyond its payload.
type Publication struct {
	Scope spatial.Scope
	// Epoch is the component version epoch; the sequence is Change.Revision.
	Epoch uint64
	// Frame is the site frame the evidence was placed in.
	Frame world.FrameRef
	// Timing supplies the clock domain and error bound; SampleTime is
	// replaced by the event's LastObserved.
	Timing     world.Timing
	TTL        time.Duration
	AppVersion string
}

// Component is the change as the World component of its ChangeEvent.
func (c Change) Component(p Publication) (world.Component, error) {
	prov, err := spatial.InferredProvenance(Feature, ModelVersion, p.AppVersion, spatial.ReduceInputs(c.Inputs))
	if err != nil {
		return world.Component{}, err
	}
	timing := p.Timing
	timing.SampleTime = c.Event.LastObserved
	return spatial.ToComponent(p.Scope, world.Version{Epoch: p.Epoch, Sequence: c.Revision}, p.Frame, timing, p.TTL, prov, c.Event)
}

// Report is the outcome of one comparison.
type Report struct {
	// Changes are the events, ordered by id.
	Changes []Change
	// Compared is how many cells were confidently observed in both maps:
	// the ground the verdict covers.
	Compared int
}

// Compare reports what changed between a reference map and a current map of
// the same floor, judging only cells confidently observed in both. Portals
// locate the doors, so that a leaf that came or went is reported as a door
// event; it may be nil. ErrNoOverlap means the maps share no confidently
// observed cell and nothing can be said.
func Compare(ref, cur *Evidence, portals []spatial.Portal) (Report, error) {
	if ref == nil || cur == nil {
		return Report{}, fmt.Errorf("%w: nil evidence", ErrInvalid)
	}
	if ref.floor != cur.floor || ref.cfg != cur.cfg {
		return Report{}, fmt.Errorf("%w: floors %q and %q", ErrIncompatible, ref.floor, cur.floor)
	}
	l := ledger{cfg: cur.cfg, portals: doors(portals)}
	l.epoch++
	compared, _ := l.observe(ref, cur)
	if compared == 0 {
		return Report{}, ErrNoOverlap
	}
	return Report{Changes: l.settle(), Compared: compared}, nil
}

// part is one cluster of changed cells: its inclusive bounding box, its size
// and its confidence.
type part struct {
	lo, hi grid.Cell
	cells  int
	conf   float64
}

func (p part) centre(cell float64) geom.Vec2 {
	return geom.Vec2{X: float64(p.lo.IX+p.hi.IX+1) / 2 * cell, Z: float64(p.lo.IZ+p.hi.IZ+1) / 2 * cell}
}

func (p part) sides(cell float64) (long, short float64) {
	w, h := float64(p.hi.IX-p.lo.IX+1)*cell, float64(p.hi.IZ-p.lo.IZ+1)*cell
	return math.Max(w, h), math.Min(w, h)
}

func (p part) radius(cell float64) float64 {
	long, short := p.sides(cell)
	return math.Hypot(long, short) / 2
}

// gap is the distance in cells between two bounding boxes; zero when they
// overlap.
func (p part) gap(q part) int32 {
	return max(q.lo.IX-p.hi.IX, p.lo.IX-q.hi.IX, q.lo.IZ-p.hi.IZ, p.lo.IZ-q.hi.IZ, 0)
}

func (p *part) join(q part) {
	p.lo = grid.Cell{IX: min(p.lo.IX, q.lo.IX), IZ: min(p.lo.IZ, q.lo.IZ)}
	p.hi = grid.Cell{IX: max(p.hi.IX, q.hi.IX), IZ: max(p.hi.IZ, q.hi.IZ)}
	p.cells += q.cells
	p.conf = math.Max(p.conf, q.conf)
}

type cluster struct {
	appeared bool
	part
	members []grid.Cell
}

// diff clusters the cells whose confident state differs between ref and cur.
// compared counts the cells confidently observed in both.
func diff(ref, cur *Evidence) (clusters []cluster, compared int) {
	cfg := cur.cfg
	reach := int32(math.Ceil(cfg.Tolerance/cfg.CellSize - 1e-9))
	// strength is how settled a cell's state is: it saturates with looks.
	strength := func(t tally) float64 {
		n := float64(t.hit) + float64(t.miss)
		return n / (n + float64(cfg.MinObservations))
	}
	type candidate struct {
		appeared bool
		conf     float64
	}
	cands := map[grid.Cell]candidate{}
	for key, cc := range cur.chunks {
		rc := ref.chunks[key]
		if rc == nil {
			continue
		}
		for i := range cc {
			cs, rs := cur.state(cc[i]), ref.state(rc[i])
			if (cs != Free && cs != Occupied) || (rs != Free && rs != Occupied) {
				continue
			}
			compared++
			if cs == rs {
				continue
			}
			c := cellOf(key, i)
			// The map in which the cell is Free must hold open space all
			// around it. Structure within Tolerance is the same structure
			// placed slightly elsewhere, and ground it did not see there
			// makes its Free the word of grazing beams alone.
			open := ref
			if cs == Free {
				open = cur
			}
			if !open.openAround(c, reach) {
				continue
			}
			cands[c] = candidate{cs == Occupied, math.Min(strength(rc[i]), strength(cc[i]))}
		}
	}
	order := make([]grid.Cell, 0, len(cands))
	for c := range cands {
		order = append(order, c)
	}
	slices.SortFunc(order, func(a, b grid.Cell) int { return cmp.Or(cmp.Compare(a.IZ, b.IZ), cmp.Compare(a.IX, b.IX)) })
	link := int32(cfg.ClusterLink / cfg.CellSize)
	seen := make(map[grid.Cell]bool, len(cands))
	for _, start := range order {
		if seen[start] {
			continue
		}
		appeared := cands[start].appeared
		p, sum := part{lo: start, hi: start}, 0.0
		seen[start] = true
		queue := []grid.Cell{start}
		for head := 0; head < len(queue); head++ {
			c := queue[head]
			p.join(part{lo: c, hi: c, cells: 1})
			sum += cands[c].conf
			for dz := -link; dz <= link; dz++ {
				for dx := -link; dx <= link; dx++ {
					n := grid.Cell{IX: c.IX + dx, IZ: c.IZ + dz}
					if k, ok := cands[n]; ok && k.appeared == appeared && !seen[n] {
						seen[n] = true
						queue = append(queue, n)
					}
				}
			}
		}
		if p.cells < cfg.MinClusterCells {
			continue
		}
		// Mean cell strength, discounted for small clusters.
		p.conf = sum / float64(p.cells) * float64(p.cells) / float64(p.cells+cfg.MinClusterCells)
		clusters = append(clusters, cluster{appeared, p, queue})
	}
	return clusters, compared
}

// change is one open change: what appeared, what disappeared, or both for a
// move.
type change struct {
	id          string
	floor       spatial.FloorID
	elev        float64
	app, dis    *part
	door        string // portal id when the change is a door leaf
	first, last time.Time
	born, seen  int // epochs in which it was created and last observed
	rev         uint64
	inputs      []world.Input
}

func (c *change) export(cfg Config) Change {
	now, kind := c.app, spatial.ChangeAppeared
	switch {
	case c.app != nil && c.dis != nil:
		kind = spatial.ChangeMoved
	case c.app != nil && c.door != "":
		kind = spatial.ChangeDoorClosed
	case c.app == nil:
		now, kind = c.dis, spatial.ChangeDisappeared
		if c.door != "" {
			kind = spatial.ChangeDoorOpened
		}
	}
	// The outline of an object seen from one side is a line: its centre lies
	// behind it by half a depth nobody saw. Take the depth to be at most the
	// width, so the bounding box's shortfall from a square bounds the error.
	// ponytail: assumes roughly square footprints; a long shelf seen whole is
	// given more uncertainty than it has. Fit an oriented box to fix that.
	uncertainty := cfg.Tolerance
	if long, short := now.sides(cfg.CellSize); c.door == "" {
		uncertainty += (long - short) / 2
	}
	ev := spatial.ChangeEvent{
		ID: c.id, Kind: kind, Floor: c.floor, Location: spatial.At(now.centre(cfg.CellSize).Lift(c.elev), uncertainty),
		Extent: spatial.Extent{RadiusM: now.radius(cfg.CellSize)}, Subject: c.door, Confidence: now.conf,
		FirstObserved: c.first, LastObserved: c.last,
	}
	if kind == spatial.ChangeMoved {
		from, to := c.dis.centre(cfg.CellSize).Lift(c.elev).Array(), ev.PositionM
		ev.FromM, ev.ToM, ev.Confidence = &from, &to, math.Min(c.app.conf, c.dis.conf)
	}
	return Change{Event: ev, Revision: c.rev, Inputs: slices.Clone(c.inputs)}
}

// ledger holds the open changes and folds each comparison into them.
type ledger struct {
	cfg     Config
	portals []spatial.Portal
	open    []*change
	epoch   int
	touched map[*change]bool // created or revised in the current epoch
	minted  int              // ids issued in the current epoch
}

// doors keeps the portals that have a leaf, ordered by id.
func doors(portals []spatial.Portal) []spatial.Portal {
	var out []spatial.Portal
	for _, p := range portals {
		if p.Kind == spatial.PortalDoor && p.WidthM > 0 {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b spatial.Portal) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

func (l *ledger) sameLevel(a, b float64) bool { return math.Abs(a-b) < l.cfg.LevelHeight }

// doorAt names the door a cluster is the leaf of: the nearest door on the
// level whose doorway holds the cluster.
func (l *ledger) doorAt(p part, elev float64) string {
	best, id := math.Inf(1), ""
	for _, d := range l.portals {
		reach := d.WidthM/2 + l.cfg.Tolerance
		dist := p.centre(l.cfg.CellSize).Dist(d.Point().XZ())
		if l.sameLevel(d.PositionM[1], elev) && dist <= reach && p.radius(l.cfg.CellSize) <= reach && dist < best {
			best, id = dist, d.ID
		}
	}
	return id
}

// observe compares one floor's epoch with its reference and folds the
// clusters into the open changes: a cluster next to an open change of the
// same kind on the same level is more evidence of that change, any other
// opens a new one. It returns the number of cells compared and the changed
// cells that went into a change.
func (l *ledger) observe(ref, cur *Evidence) (compared int, reported map[grid.Cell]bool) {
	clusters, compared := diff(ref, cur)
	if len(clusters) == 0 {
		return compared, nil
	}
	reported = map[grid.Cell]bool{}
	if l.touched == nil {
		l.touched = map[*change]bool{}
	}
	inputs := spatial.ReduceInputs(append(slices.Clone(ref.inputs), cur.inputs...))
	link := int32(l.cfg.ClusterLink / l.cfg.CellSize)
	for _, k := range clusters {
		for _, c := range k.members {
			reported[c] = true
		}
		door := l.doorAt(k.part, cur.elev)
		side := func(c *change) **part {
			if k.appeared {
				return &c.app
			}
			return &c.dis
		}
		i := slices.IndexFunc(l.open, func(c *change) bool {
			p := *side(c)
			return p != nil && c.door == door && l.sameLevel(c.elev, cur.elev) && p.gap(k.part) <= link
		})
		var c *change
		if i >= 0 {
			c = l.open[i]
			(*side(c)).join(k.part)
			c.inputs = spatial.ReduceInputs(append(c.inputs, inputs...))
		} else {
			l.minted++
			p := k.part
			c = &change{id: newID(cur.floor, cur.from, l.minted), floor: cur.floor, elev: cur.elev, door: door, first: cur.from, born: l.epoch, inputs: inputs}
			*side(c) = &p
			l.open = append(l.open, c)
		}
		if cur.to.After(c.last) {
			c.last = cur.to
		}
		c.seen = l.epoch
		l.touched[c] = true
	}
	return compared, reported
}

// newID is unique per floor, epoch and cluster, and the same for the same
// evidence.
func newID(floor spatial.FloorID, at time.Time, n int) string {
	f := string(floor)
	if len(f) > 64 {
		f = f[:64]
	}
	return fmt.Sprintf("%s-%d-%d", f, at.Unix(), n)
}

// settle closes the epoch: it pairs disappearances with appearances into
// moves, drops the changes that have had no evidence for Config.OpenEpochs
// and returns those created or revised this epoch, ordered by id.
func (l *ledger) settle() []Change {
	type pair struct {
		app, dis *change
		dist     float64
	}
	cell := l.cfg.CellSize
	var pairs []pair
	for _, a := range l.open {
		if a.app == nil || a.dis != nil || a.door != "" {
			continue
		}
		for _, d := range l.open {
			if d.dis == nil || d.app != nil || d.door != "" || !l.sameLevel(a.elev, d.elev) || !(l.touched[a] || l.touched[d]) {
				continue
			}
			// What came and later went from the same spot did not move.
			if a.born != d.born && a.app.gap(*d.dis) == 0 {
				continue
			}
			ra, rd := a.app.radius(cell), d.dis.radius(cell)
			dist := a.app.centre(cell).Dist(d.dis.centre(cell))
			if dist <= l.cfg.MoveMaxDistance && math.Min(ra, rd) >= l.cfg.FootprintRatio*math.Max(ra, rd) {
				pairs = append(pairs, pair{a, d, dist})
			}
		}
	}
	slices.SortStableFunc(pairs, func(x, y pair) int {
		return cmp.Or(cmp.Compare(x.dist, y.dist), cmp.Compare(x.app.id, y.app.id), cmp.Compare(x.dis.id, y.dis.id))
	})
	gone := map[*change]bool{}
	for _, p := range pairs {
		if gone[p.app] || gone[p.dis] || p.app.dis != nil || p.dis.app != nil {
			continue
		}
		// The move keeps the id of whichever half was reported first and
		// stands where the object now is.
		keep, drop := p.app, p.dis
		if p.dis.born < p.app.born {
			keep, drop = p.dis, p.app
		}
		keep.app, keep.dis = p.app.app, p.dis.dis
		keep.floor, keep.elev = p.app.floor, p.app.elev
		if drop.first.Before(keep.first) {
			keep.first = drop.first
		}
		if drop.last.After(keep.last) {
			keep.last = drop.last
		}
		keep.seen = max(keep.seen, drop.seen)
		keep.inputs = spatial.ReduceInputs(append(keep.inputs, drop.inputs...))
		gone[drop] = true
		l.touched[keep] = true
	}
	var out []Change
	l.open = slices.DeleteFunc(l.open, func(c *change) bool {
		if gone[c] || l.epoch-c.seen > l.cfg.OpenEpochs {
			return true
		}
		if l.touched[c] {
			c.rev++
			out = append(out, c.export(l.cfg))
		}
		return false
	})
	l.touched, l.minted = nil, 0
	slices.SortFunc(out, func(a, b Change) int { return cmp.Compare(a.Event.ID, b.Event.ID) })
	return out
}
