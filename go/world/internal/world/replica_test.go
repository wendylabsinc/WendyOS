package world

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func fixture(t *testing.T, allowSynthetic bool) (*Replica, *time.Time, Component) {
	t.Helper()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	r, err := NewReplica(Config{Tenant: "wendy", Site: "office", ClockDomain: "test", Now: func() time.Time { return now }, MaxFutureSkew: 10 * time.Millisecond, MaxEntries: 100, AllowSynthetic: allowSynthetic, Frames: []FrameRef{{"floor", "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := Component{Key: Key{Tenant: "wendy", Site: "office", Entity: "robot", Type: "pose", Source: Source{"device", "app", "tracker", "poses"}}, Version: Version{1, 1}, Frame: FrameRef{"floor", "1"}, Timing: Timing{SampleTime: now, ClockDomain: "test", ErrorKnown: true}, TTL: time.Second, Provenance: Provenance{Origin: Measured, AppVersion: "1"}, Payload: json.RawMessage(`{"x":1}`)}
	return r, &now, c
}
func apply(t *testing.T, r *Replica, c Component) Result {
	t.Helper()
	result, err := r.Apply(c.Key.Source, c)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func visible(t *testing.T, r *Replica, c Component) bool {
	t.Helper()
	_, ok, err := r.Get(c.Key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestMergeConvergesUnderPermutationAndDuplicates(t *testing.T) {
	_, _, base := fixture(t, false)
	var records []Component
	for source := 0; source < 3; source++ {
		for epoch := 1; epoch <= 3; epoch++ {
			for seq := 1; seq <= 4; seq++ {
				c := base
				c.Key.Source.Stream = string(rune('a' + source))
				c.Version = Version{uint64(epoch), uint64(seq)}
				c.Payload = json.RawMessage(`{"x":2}`)
				records = append(records, c, c)
			}
		}
	}
	var expected []Component
	for seed := int64(0); seed < 50; seed++ {
		r, _, _ := fixture(t, false)
		shuffled := append([]Component{}, records...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for _, c := range shuffled {
			apply(t, r, c)
		}
		got, err := r.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d sources", len(got))
		}
		for _, c := range got {
			if c.Version != (Version{3, 4}) {
				t.Fatal(c.Version)
			}
		}
		if expected == nil {
			expected = got
		} else if !reflect.DeepEqual(got, expected) {
			t.Fatalf("seed %d failed convergence", seed)
		}
	}
}
func TestDuplicateNeverExtendsExpiryAndHintsCanOnlyShorten(t *testing.T) {
	r, now, c := fixture(t, false)
	apply(t, r, c)
	*now = now.Add(100 * time.Millisecond)
	c.Timing.AgeHint = 900 * time.Millisecond
	if got := apply(t, r, c); got != Duplicate {
		t.Fatal(got)
	}
	c.Timing.AgeHint = 0
	apply(t, r, c)
	*now = now.Add(100 * time.Millisecond)
	if visible(t, r, c) {
		t.Fatal("stripping hint extended accepted deadline")
	}
	*now = now.Add(2 * time.Second)
	apply(t, r, c)
	if visible(t, r, c) {
		t.Fatal("duplicate resurrected expired record")
	}
}
func TestTombstoneIsSourceScopedAndRetainsHighWater(t *testing.T) {
	r, now, c := fixture(t, false)
	other := c
	other.Key.Source.Service = "other"
	apply(t, r, c)
	apply(t, r, other)
	tomb := c
	tomb.Version.Sequence = 2
	tomb.Tombstone = true
	tomb.Payload = nil
	tomb.TTL = 100 * time.Millisecond
	apply(t, r, tomb)
	if visible(t, r, c) || !visible(t, r, other) {
		t.Fatal("tombstone affected incorrect source")
	}
	*now = now.Add(200 * time.Millisecond)
	if got := apply(t, r, c); got != Obsolete {
		t.Fatal(got)
	}
	if visible(t, r, c) {
		t.Fatal("old value resurrected after tombstone TTL")
	}
	newEpoch := c
	newEpoch.Version = Version{2, 1}
	newEpoch.Timing.SampleTime = *now
	apply(t, r, newEpoch)
	if !visible(t, r, c) {
		t.Fatal("new publisher epoch not accepted")
	}
	if _, err := r.Apply(other.Key.Source, tomb); !errors.Is(err, ErrWrongSource) {
		t.Fatal(err)
	}
}
func TestFramesAreHeldAndReevaluatedWithoutRestartingTTL(t *testing.T) {
	r, now, c := fixture(t, false)
	c.Frame.Revision = "2"
	apply(t, r, c)
	if visible(t, r, c) {
		t.Fatal("mismatched frame visible")
	}
	if err := r.SetFrame(c.Frame); err != nil {
		t.Fatal(err)
	}
	if !visible(t, r, c) {
		t.Fatal("compatible held record not visible")
	}
	*now = now.Add(time.Second)
	if err := r.SetFrame(FrameRef{"floor", "3"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetFrame(c.Frame); err != nil {
		t.Fatal(err)
	}
	if visible(t, r, c) {
		t.Fatal("frame update restarted TTL")
	}
}
func TestFreshnessScopeAndVersionAdmission(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Component)
		want   error
	}{
		{"unknown error", func(c *Component) { c.Timing.ErrorKnown = false }, ErrUnknownFreshness},
		{"other clock", func(c *Component) { c.Timing.ClockDomain = "simulation" }, ErrUnknownFreshness},
		{"future", func(c *Component) { c.Timing.SampleTime = c.Timing.SampleTime.Add(time.Second) }, ErrFutureSample},
		{"other tenant", func(c *Component) { c.Key.Tenant = "elsewhere" }, ErrWrongScope},
		{"other site", func(c *Component) { c.Key.Site = "elsewhere" }, ErrWrongScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, c := fixture(t, false)
			tt.mutate(&c)
			if _, err := r.Apply(c.Key.Source, c); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	r, _, c := fixture(t, false)
	apply(t, r, c)
	c.Payload = json.RawMessage(`{"x":999}`)
	if _, err := r.Apply(c.Key.Source, c); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
func TestUncertaintyConsumesLifetimeAndExpiredHigherVersionSupersedes(t *testing.T) {
	r, now, c := fixture(t, false)
	c.Timing.ErrorBound = 500 * time.Millisecond
	apply(t, r, c)
	*now = now.Add(500 * time.Millisecond)
	if visible(t, r, c) {
		t.Fatal("clock error did not consume TTL")
	}
	next := c
	next.Version.Sequence = 2
	next.Timing.SampleTime = now.Add(-2 * time.Second)
	apply(t, r, next)
	if visible(t, r, next) {
		t.Fatal("stale higher version visible")
	}
	if got := apply(t, r, c); got != Obsolete {
		t.Fatal(got)
	}
}
func TestSyntheticAncestryRejectedByPhysicalReplica(t *testing.T) {
	r, _, c := fixture(t, false)
	c.Provenance = Provenance{Origin: Inferred, AppVersion: "1", ModelID: "fusion", ModelVersion: "1", Inputs: []Input{{ID: "gas-sim", Origin: Simulated, Simulation: &SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "test"}}}}
	if _, err := r.Apply(c.Key.Source, c); !errors.Is(err, ErrSynthetic) {
		t.Fatal(err)
	}
	sim, _, _ := fixture(t, true)
	apply(t, sim, c)
	if !visible(t, sim, c) {
		t.Fatal("explicit simulation replica rejected observation")
	}
}
func TestReplicaDoesNotAliasCallerMemory(t *testing.T) {
	r, _, c := fixture(t, true)
	c.Provenance = Provenance{Origin: Simulated, AppVersion: "1", Simulation: &SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "test"}}
	apply(t, r, c)
	c.Payload[5] = '9'
	c.Provenance.Simulation.RunID = "mutated"
	got, ok, err := r.Get(c.Key)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if string(got.Payload) != `{"x":1}` || got.Provenance.Simulation.RunID != "run" {
		t.Fatal("input aliases replica")
	}
	got.Payload[5] = '8'
	got.Provenance.Simulation.RunID = "again"
	again, _, _ := r.Get(c.Key)
	if string(again.Payload) != `{"x":1}` || again.Provenance.Simulation.RunID != "run" {
		t.Fatal("output aliases replica")
	}
}
func TestCapacityDoesNotEvictReplayProtection(t *testing.T) {
	r, now, c := fixture(t, false)
	r.config.MaxEntries = 1
	apply(t, r, c)
	*now = now.Add(2 * time.Second)
	other := c
	other.Key.Entity = "another"
	if _, err := r.Apply(other.Key.Source, other); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if got := apply(t, r, c); got != Duplicate {
		t.Fatal(got)
	}
	if visible(t, r, c) {
		t.Fatal("eviction revived expired value")
	}
}
func TestClockRegressionFailsClosed(t *testing.T) {
	r, now, c := fixture(t, false)
	apply(t, r, c)
	*now = now.Add(-time.Second)
	if _, _, err := r.Get(c.Key); !errors.Is(err, ErrClockRegression) {
		t.Fatal(err)
	}
	if _, err := r.Apply(c.Key.Source, c); !errors.Is(err, ErrClockRegression) {
		t.Fatal(err)
	}
}

func TestAgeArithmeticSaturatesWithoutRevivingRecords(t *testing.T) {
	r, now, c := fixture(t, false)
	c.TTL = time.Duration(1<<63 - 1)
	c.Timing.SampleTime = now.Add(-time.Hour)
	c.Timing.ErrorBound = time.Duration(1<<63 - 1)
	apply(t, r, c)
	if visible(t, r, c) {
		t.Fatal("overflow in age plus uncertainty revived record")
	}
	r, _, c = fixture(t, false)
	c.Timing.AgeHint = time.Duration(1<<63 - 1)
	apply(t, r, c)
	c.Timing.AgeHint = 0
	apply(t, r, c)
	if visible(t, r, c) {
		t.Fatal("stripping saturated age hint revived record")
	}
}
