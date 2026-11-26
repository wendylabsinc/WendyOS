package history

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Axis is the clock a query reads. See the package documentation.
type Axis int

const (
	// SampleTime orders history by Timing.SampleTime: when the state held.
	SampleTime Axis = iota
	// ApplyTime orders history by Record.Applied: when the journal learned
	// of it. It reproduces a world.Replica under the same clock.
	ApplyTime
)

// View is how a state query reads time.
type View struct {
	Axis Axis
	// HonourTTL leaves out a record whose lifetime has run out at the query
	// time. Without it a key stays in the state until it is tombstoned.
	HonourTTL bool
}

// Difference is what changed in the state between two times.
type Difference struct {
	Added   []world.Component // in the state at the later time only
	Removed []world.Component // in the state at the earlier time only, as it was then
	Changed []Change          // in both, at different versions
}

// Change is one key whose record differs between two times.
type Change struct {
	From, To world.Component
}

// Fix is one point of a Trajectory.
type Fix struct {
	// Time is the record's time on the query axis.
	Time time.Time
	// Entity is the published pose as spatial.ComponentEntities projects it:
	// position, floor, heading when it has one, and UncertaintyM as the
	// publisher stated it. The journal adds no error of its own.
	Entity spatial.Entity
}

func (e *entry) time(axis Axis) time.Time {
	if axis == ApplyTime {
		return e.applied
	}
	return e.sample
}

func (e *entry) expired(t time.Time, axis Axis) bool {
	if axis == ApplyTime {
		return !t.Before(e.expires)
	}
	return !t.Before(e.sample.Add(e.ttl))
}

// at is the record in force at t: the highest version timed at or before t.
//
// ponytail: a backwards scan, linear in the versions newer than t, so an old
// as-of over a long history touches most of the index. Keep a per-axis
// prefix-maximum list per key if deep queries need to be faster than a few
// milliseconds per million records.
func (s *series) at(t time.Time, axis Axis) *entry {
	for i := len(s.entries) - 1; i >= 0; i-- {
		if e := s.entries[i]; !e.time(axis).After(t) {
			return e
		}
	}
	return nil
}

// changes lists the records that became the state of s within [from, to],
// oldest first: a record outranked by an earlier-timed higher version never
// was the state and is left out.
func (s *series) changes(from, to time.Time, axis Axis) []*entry {
	var es []*entry
	for _, e := range s.entries {
		if !e.time(axis).After(to) {
			es = append(es, e)
		}
	}
	// Among records timed together only the highest version takes effect.
	slices.SortStableFunc(es, func(a, b *entry) int {
		return cmp.Or(a.time(axis).Compare(b.time(axis)), b.version.Compare(a.version))
	})
	var out []*entry
	var top *entry
	for _, e := range es {
		if top != nil && e.version.Compare(top.version) < 0 {
			continue
		}
		top = e
		if !e.time(axis).Before(from) {
			out = append(out, e)
		}
	}
	return out
}

// hit is one record with the key it belongs to.
type hit struct {
	s *series
	e *entry
}

func (j *Journal) queryable(axis Axis, from, to time.Time) error {
	if err := j.usable(); err != nil {
		return err
	}
	if axis != SampleTime && axis != ApplyTime {
		return fmt.Errorf("%w: axis %d", ErrInvalid, axis)
	}
	if from.IsZero() || to.Before(from) {
		return fmt.Errorf("%w: time range %s to %s", ErrInvalid, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}
	if from.Before(j.horizon) {
		return fmt.Errorf("%w: %s is before %s", ErrCompacted, from.Format(time.RFC3339Nano), j.horizon.Format(time.RFC3339Nano))
	}
	return nil
}

// live is the state at t in key order: per key the record in force, unless
// that is a tombstone or, when asked, has outlived its TTL.
func (j *Journal) live(t time.Time, v View) ([]hit, error) {
	if err := j.queryable(v.Axis, t, t); err != nil {
		return nil, err
	}
	var out []hit
	for _, s := range j.keys {
		if e := s.at(t, v.Axis); e != nil && !e.tombstone && !(v.HonourTTL && e.expired(t, v.Axis)) {
			out = append(out, hit{s, e})
		}
	}
	slices.SortFunc(out, func(a, b hit) int { return cmp.Compare(a.s.order, b.s.order) })
	return out, nil
}

func (j *Journal) readAll(es []*entry) ([]Record, error) {
	out := make([]Record, 0, len(es))
	for _, e := range es {
		rec, err := j.read(e)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// AsOf reconstructs the state at t: every key's record in force then, in the
// order of world.Replica.Snapshot. With View{ApplyTime, true} the result
// equals what a replica fed the journaled records under the journal's clock
// returned from Snapshot at t. A time later than the newest record answers
// from the journal as it stands. Before the retention horizon it returns
// ErrCompacted.
func (j *Journal) AsOf(t time.Time, v View) ([]world.Component, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	state, err := j.live(t, v)
	if err != nil {
		return nil, err
	}
	out := make([]world.Component, 0, len(state))
	for _, h := range state {
		rec, err := j.read(h.e)
		if err != nil {
			return nil, err
		}
		out = append(out, rec.Component)
	}
	return out, nil
}

// Diff compares the states AsOf(from, v) and AsOf(to, v), each list in key
// order. With View.HonourTTL a record that expired in between is Removed.
// Only the records that differ are read from disk.
func (j *Journal) Diff(from, to time.Time, v View) (Difference, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	var d Difference
	if err := j.queryable(v.Axis, from, to); err != nil {
		return d, err
	}
	a, err := j.live(from, v)
	if err != nil {
		return d, err
	}
	b, err := j.live(to, v)
	if err != nil {
		return d, err
	}
	get := func(h hit) world.Component {
		rec, rerr := j.read(h.e)
		if rerr != nil && err == nil {
			err = rerr
		}
		return rec.Component
	}
	for len(a) > 0 || len(b) > 0 {
		switch {
		case len(b) == 0 || (len(a) > 0 && a[0].s.order < b[0].s.order):
			d.Removed = append(d.Removed, get(a[0]))
			a = a[1:]
		case len(a) == 0 || b[0].s.order < a[0].s.order:
			d.Added = append(d.Added, get(b[0]))
			b = b[1:]
		default:
			if a[0].e != b[0].e {
				d.Changed = append(d.Changed, Change{From: get(a[0]), To: get(b[0])})
			}
			a, b = a[1:], b[1:]
		}
	}
	if err != nil {
		return Difference{}, err
	}
	return d, nil
}

// Timeline lists the records that became the state of key within [from, to]
// on axis, oldest first, tombstones included. The state at from itself is
// AsOf(from). Expiry is not an entry: a timeline is the sequence of deltas
// that took effect. An unknown key has an empty timeline.
func (j *Journal) Timeline(key world.Key, from, to time.Time, axis Axis) ([]Record, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if err := j.queryable(axis, from, to); err != nil {
		return nil, err
	}
	s := j.keys[key]
	if s == nil {
		return nil, nil
	}
	return j.readAll(s.changes(from, to, axis))
}

// EntityTimeline merges the timelines of every key of one entity (the
// Key.Entity of a tenant and site, such as "device/rover-1") whatever its
// component type and source: by time, then by key.
func (j *Journal) EntityTimeline(tenant, site, entity string, from, to time.Time, axis Axis) ([]Record, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if err := j.queryable(axis, from, to); err != nil {
		return nil, err
	}
	var hits []hit
	for k, s := range j.keys {
		if k.Tenant == tenant && k.Site == site && k.Entity == entity {
			for _, e := range s.changes(from, to, axis) {
				hits = append(hits, hit{s, e})
			}
		}
	}
	// Stable, so one key's entries keep the order changes gave them.
	slices.SortStableFunc(hits, func(a, b hit) int {
		return cmp.Or(a.e.time(axis).Compare(b.e.time(axis)), cmp.Compare(a.s.order, b.s.order))
	})
	es := make([]*entry, len(hits))
	for i, h := range hits {
		es[i] = h.e
	}
	return j.readAll(es)
}

// Trajectory is the Timeline of a pose-like component (a device pose, a
// track, an anchor, a landmark, ...) as positions: one Fix per upsert, with
// the uncertainty its publisher stated. Tombstones are left out; use Timeline
// to see where a path ends. A component that does not project to exactly one
// spatial.Entity, such as a map tile, returns ErrNotPose.
func (j *Journal) Trajectory(key world.Key, from, to time.Time, axis Axis) ([]Fix, error) {
	recs, err := j.Timeline(key, from, to, axis)
	if err != nil {
		return nil, err
	}
	var out []Fix
	for _, rec := range recs {
		if rec.Component.Tombstone {
			continue
		}
		entities, err := spatial.ComponentEntities(rec.Component)
		if err != nil {
			return nil, fmt.Errorf("%w: %s version %d.%d: %v", ErrNotPose, key.Type, rec.Component.Version.Epoch, rec.Component.Version.Sequence, err)
		}
		if len(entities) != 1 {
			return nil, fmt.Errorf("%w: %s projects to %d entities", ErrNotPose, key.Type, len(entities))
		}
		at := rec.Component.Timing.SampleTime
		if axis == ApplyTime {
			at = rec.Applied
		}
		out = append(out, Fix{Time: at, Entity: entities[0]})
	}
	return out, nil
}
