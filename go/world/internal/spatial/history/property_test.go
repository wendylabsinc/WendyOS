package history

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// propertyRun drives one journal and one world.Replica with the same random
// deltas under the same clock, and keeps the brute-force model beside them.
type propertyRun struct {
	t       *testing.T
	rng     *rand.Rand
	dir     string
	cfg     Config
	clock   *fakeClock
	journal *Journal
	replica *world.Replica

	model   model
	keys    []world.Key
	devices []propertyKey
	live    []liveState // the replica's Snapshot at sampled instants
	// instants are the times worth querying: every sample time, apply time
	// and sample-axis expiry, where an off-by-one would show.
	instants []time.Time
	accepted map[world.Key]map[uint64]world.Component
	stepped  bool // the clock has moved since the last live sample
}

type propertyKey struct {
	scope  spatial.Scope
	device string
}

type liveState struct {
	at    time.Time
	state []world.Component
}

func newPropertyRun(t *testing.T, seed, stream uint64) *propertyRun {
	clock := &fakeClock{t: at(1000)}
	replica, err := world.NewReplica(world.Config{
		Tenant: testScope.Tenant, Site: testScope.Site, ClockDomain: testDomain, Now: clock.now,
		MaxFutureSkew: 5 * time.Second, MaxEntries: 1 << 12, AllowSynthetic: true, Frames: []world.FrameRef{testFrame},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &propertyRun{
		t: t, rng: rand.New(rand.NewPCG(seed, stream)), dir: t.TempDir(), clock: clock, replica: replica,
		accepted: map[world.Key]map[uint64]world.Component{}, stepped: true,
	}
	// Segments of about fifteen records, so that every run crosses some
	// twenty seals. (Each seal is two fsyncs: smaller would only be slower.)
	p.cfg = Config{Now: clock.now, SegmentBytes: 12 << 10}
	p.journal = mustOpen(t, p.dir, p.cfg)
	second := testScope
	second.Source.Stream = "second"
	for i := 0; i < 6; i++ {
		p.devices = append(p.devices, propertyKey{testScope, fmt.Sprintf("d%d", i)})
	}
	// Two devices are also published by a second source: one entity, two keys.
	p.devices = append(p.devices, propertyKey{second, "d0"}, propertyKey{second, "d1"})
	return p
}

// step applies n random deltas: new versions (some of them tombstones), late
// arrivals of lower versions, exact replays and conflicting replays, with
// sample times up to 20 s in the past or 1 s ahead and clock steps that are
// often zero.
func (p *propertyRun) step(n int) {
	t, rng := p.t, p.rng
	for i := 0; i < n; i++ {
		if !p.stepped || rng.Float64() < 0.6 {
			p.clock.t = p.clock.t.Add(1 + time.Duration(rng.Int64N(int64(3*time.Second))))
			p.stepped = true
		}
		dev := p.devices[rng.IntN(len(p.devices))]
		sample := p.clock.t.Add(time.Second - time.Duration(rng.Int64N(int64(21*time.Second))))
		ttl := 500*time.Millisecond + time.Duration(rng.Int64N(int64(30*time.Second)))
		c := poseAt(t, dev.scope, dev.device, 1, sample, ttl, rng.Float64()*30, rng.Float64()*20)
		if rng.Float64() < 0.2 {
			c.Timing.ErrorBound = time.Duration(rng.Int64N(int64(time.Second)))
		}
		if rng.Float64() < 0.2 {
			c.Timing.AgeHint = time.Duration(rng.Int64N(int64(25 * time.Second)))
		}
		seen := p.accepted[c.Key]
		if seen == nil {
			seen = map[uint64]world.Component{}
			p.accepted[c.Key] = seen
			p.keys = append(p.keys, c.Key)
		}
		var used []uint64
		for v := range seen {
			used = append(used, v)
		}
		sort.Slice(used, func(i, k int) bool { return used[i] < used[k] })
		high := uint64(0)
		if len(used) > 0 {
			high = used[len(used)-1]
		}

		// A replay is only recognised while the journal still holds that
		// version: one that compaction collapsed away is journaled again,
		// below the record in force, and so joins the model again.
		held := func(sequence uint64) bool {
			_, found := p.journal.keys[c.Key].find(world.Version{Epoch: 1, Sequence: sequence})
			return found
		}
		fresh := false
		switch roll := rng.Float64(); {
		case roll < 0.10 && len(used) > 0: // exact replay
			c = seen[used[rng.IntN(len(used))]]
			if held(c.Version.Sequence) {
				if appended, err := p.journal.Append(c); appended || err != nil {
					t.Fatalf("replay of %s: appended=%v err=%v", label(c), appended, err)
				}
				if _, err := p.replica.Apply(c.Key.Source, c); err != nil {
					t.Fatalf("replica replay of %s: %v", label(c), err)
				}
				continue
			}
		case roll < 0.16 && len(used) > 0: // same version, other content
			c.Version.Sequence = used[rng.IntN(len(used))]
			if held(c.Version.Sequence) {
				if _, err := p.journal.Append(c); !errors.Is(err, ErrConflict) {
					t.Fatalf("conflicting %s: %v, want ErrConflict", label(c), err)
				}
				continue
			}
		case roll < 0.30 && high > uint64(len(used)): // late arrival of a skipped version
			for {
				if c.Version.Sequence = 1 + rng.Uint64N(high); seen[c.Version.Sequence].Key == (world.Key{}) {
					break
				}
			}
			fresh = true
		default: // a newer version, sometimes skipping a few
			c.Version.Sequence = high + 1 + rng.Uint64N(3)
			fresh = true
		}
		if fresh && rng.Float64() < 0.15 {
			c = tombstoneOf(c, c.Version.Sequence, sample)
		}
		mustAppend(t, p.journal, c)
		if _, err := p.replica.Apply(c.Key.Source, c); err != nil {
			t.Fatalf("replica apply of %s: %v", label(c), err)
		}
		seen[c.Version.Sequence] = c
		p.model = append(p.model, Record{Seq: uint64(len(p.model) + 1), Applied: p.clock.t, Component: canon(t, c)})
		p.instants = append(p.instants, c.Timing.SampleTime, p.clock.t, c.Timing.SampleTime.Add(c.TTL))

		if rng.Float64() < 0.12 {
			// The live state, sometimes between two applications so that
			// only expiry separates it from the previous one. The clock
			// must move on before the next delta, or that delta would be
			// applied "at" this instant after the sample was taken.
			if rng.Float64() < 0.5 {
				p.clock.t = p.clock.t.Add(time.Duration(rng.Int64N(int64(8 * time.Second))))
			}
			state, err := p.replica.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			p.live = append(p.live, liveState{p.clock.t, state})
			p.stepped = false
		}
	}
}

func (p *propertyRun) reopen() {
	if err := p.journal.Close(); err != nil {
		p.t.Fatal(err)
	}
	p.journal = mustOpen(p.t, p.dir, p.cfg)
}

// queriesPerPhase is the number of random as-of, diff, timeline, trajectory
// and entity-timeline queries verify makes each time, spread over the views.
const queriesPerPhase = 45

// verify holds the journal to the replica's recorded live states and to the
// brute-force model, at and after horizon, and requires the typed refusal
// before it. It returns the number of live states that did not reconstruct.
func (p *propertyRun) verify(phase string, horizon time.Time) (mismatched int) {
	t, j, rng := p.t, p.journal, p.rng
	for _, l := range p.live {
		got, err := j.AsOf(l.at, View{Axis: ApplyTime, HonourTTL: true})
		if l.at.Before(horizon) {
			if !errors.Is(err, ErrCompacted) {
				t.Fatalf("%s: live state before the horizon: %v, want ErrCompacted", phase, err)
			}
			continue
		}
		if err != nil || !sameState(got, l.state) {
			mismatched++
			t.Errorf("%s: as of %v:\n got %s (%v)\nwant %s", phase, l.at.Sub(testEpoch), labels(got), err, labels(l.state))
		}
	}

	pick := func() time.Time {
		at := p.instants[rng.IntN(len(p.instants))].Add(time.Duration(rng.IntN(3) - 1)) // the instant, or 1 ns either side
		if rng.Float64() < 0.25 {
			at = at.Add(time.Duration(rng.Int64N(int64(10*time.Second))) - 5*time.Second)
		}
		if at.Before(horizon) {
			return horizon.Add(at.Sub(testEpoch) % (30 * time.Second))
		}
		return at
	}
	views := []View{{Axis: ApplyTime}, {Axis: SampleTime}, {Axis: SampleTime, HonourTTL: true}}
	for i := 0; i < queriesPerPhase; i++ {
		at, v := pick(), views[i%len(views)]
		got, err := j.AsOf(at, v)
		if want := p.model.asOf(at, v); err != nil || !sameState(got, want) {
			t.Fatalf("%s: as of %v %+v:\n got %s (%v)\nwant %s", phase, at.Sub(testEpoch), v, labels(got), err, labels(want))
		}
		from, to := pick(), pick()
		if to.Before(from) {
			from, to = to, from
		}
		d, err := j.Diff(from, to, v)
		if want := p.model.diff(from, to, v); err != nil || !sameDifference(d, want) {
			t.Fatalf("%s: diff %v to %v %+v:\n got %+v (%v)\nwant %+v", phase, from.Sub(testEpoch), to.Sub(testEpoch), v, d, err, want)
		}
		key := p.keys[rng.IntN(len(p.keys))]
		recs, err := j.Timeline(key, from, to, v.Axis)
		want := p.model.timeline(key, from, to, v.Axis)
		if err != nil || !sameRecords(recs, want) {
			t.Fatalf("%s: timeline of %s %v to %v axis %d:\n got %s (%v)\nwant %s", phase, key.Entity, from.Sub(testEpoch), to.Sub(testEpoch), v.Axis, labels(components(recs)), err, labels(components(want)))
		}
		// A trajectory is the timeline less its tombstones, as positions.
		fixes, err := j.Trajectory(key, from, to, v.Axis)
		if err != nil {
			t.Fatalf("%s: trajectory: %v", phase, err)
		}
		for _, r := range want {
			if r.Component.Tombstone {
				continue
			}
			pose, err := spatial.FromComponent[spatial.DevicePose](r.Component)
			if err != nil {
				t.Fatal(err)
			}
			if len(fixes) == 0 || !fixes[0].Time.Equal(r.time(v.Axis)) || fixes[0].Entity.Position != pose.Point() || fixes[0].Entity.UncertaintyM != pose.UncertaintyM {
				t.Fatalf("%s: trajectory of %s diverges from its timeline at %s", phase, key.Entity, label(r.Component))
			}
			fixes = fixes[1:]
		}
		if len(fixes) != 0 {
			t.Fatalf("%s: trajectory of %s has %d extra fixes", phase, key.Entity, len(fixes))
		}
		// An entity timeline is its keys' timelines merged by time, then key.
		var merged []Record
		for _, k := range p.keys {
			if k.Entity == key.Entity {
				merged = append(merged, p.model.timeline(k, from, to, v.Axis)...)
			}
		}
		sort.SliceStable(merged, func(i, k int) bool {
			if a, b := merged[i].time(v.Axis), merged[k].time(v.Axis); !a.Equal(b) {
				return a.Before(b)
			}
			return keyOrder(merged[i].Component.Key) < keyOrder(merged[k].Component.Key)
		})
		recs, err = j.EntityTimeline(key.Tenant, key.Site, key.Entity, from, to, v.Axis)
		if err != nil || !sameRecords(recs, merged) {
			t.Fatalf("%s: entity timeline of %s:\n got %s (%v)\nwant %s", phase, key.Entity, labels(components(recs)), err, labels(components(merged)))
		}
	}
	if !horizon.IsZero() {
		if _, err := j.AsOf(horizon.Add(-1), View{Axis: SampleTime}); !errors.Is(err, ErrCompacted) {
			t.Fatalf("%s: one nanosecond before the horizon: %v, want ErrCompacted", phase, err)
		}
	}
	return mismatched
}

// Random upserts, tombstones, late and replayed versions with out-of-order
// sample times: the journal must reproduce the replica's live states and the
// model's answers while live, after a restart, after compaction, after a
// restart on top of the compaction, and after carrying on from there.
func TestRandomHistoryMatchesReplicaAndModel(t *testing.T) {
	const (
		deltasPerPhase       = 150
		maxMismatchedStates  = 0 // design target: reconstruction equals the recorded live state
		minLiveStatesChecked = 20
	)
	for _, seed := range seeds(t) {
		liveStates, mismatched, records, dropped := 0, 0, 0, 0
		for run := uint64(0); run < propertyRuns; run++ {
			p := newPropertyRun(t, seed, run)
			var none time.Time
			p.step(deltasPerPhase)
			mismatched += p.verify("live", none)
			p.reopen()
			mismatched += p.verify("reopened", none)

			// Collapse the first part of the history, at an instant that is
			// a record time as often as not.
			horizon := p.instants[len(p.instants)/2+p.rng.IntN(len(p.instants)/4)]
			res, err := p.journal.Compact(horizon)
			if err != nil {
				t.Fatal(err)
			}
			dropped += res.Dropped
			mismatched += p.verify("compacted", horizon)
			p.reopen()
			mismatched += p.verify("compacted and reopened", horizon)

			// The journal carries on: more history, a later horizon.
			p.step(deltasPerPhase)
			mismatched += p.verify("continued", horizon)
			horizon = p.clock.t.Add(-time.Duration(p.rng.Int64N(int64(2 * time.Minute))))
			if res, err = p.journal.Compact(horizon); err != nil {
				t.Fatal(err)
			}
			dropped += res.Dropped
			horizon = res.Horizon
			p.reopen()
			mismatched += p.verify("compacted twice and reopened", horizon)
			if t.Failed() {
				t.Fatalf("seed %d run %d failed", seed, run)
			}
			liveStates += len(p.live)
			records += len(p.model)
		}
		t.Logf("seed %d: %d runs, %d deltas, %d dropped by compaction, %d live states", seed, propertyRuns, records, dropped, liveStates)
		t.Logf("METRIC property_mismatched_states=%d states (target <= %d)", mismatched, maxMismatchedStates)
		if mismatched > maxMismatchedStates || liveStates < minLiveStatesChecked || dropped == 0 {
			t.Errorf("seed %d: %d mismatched of %d live states, %d records dropped", seed, mismatched, liveStates, dropped)
		}
	}
}
