package world

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestRetainedCopiesOwnAllAncestry(t *testing.T) {
	for _, inferred := range []bool{false, true} {
		r, now, c := fixture(t, true)
		capture := now.Add(-time.Second)
		sim := &SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "sim"}
		c.Provenance = Provenance{Origin: Replayed, AppVersion: "v1", Simulation: sim, OriginalCaptureTime: &capture, OriginalClockDomain: "source"}
		if inferred {
			c.Provenance = Provenance{Origin: Inferred, AppVersion: "v1", ModelID: "model", ModelVersion: "v1", Inputs: []Input{
				{ID: "a", Origin: Replayed, Simulation: sim, OriginalCaptureTime: &capture, OriginalClockDomain: "source"},
				{ID: "b", Origin: Simulated, Simulation: sim},
			}}
		}
		// Retain the established admission normalization for opaque JSON,
		// escaped strings, empty optional slices and wall-clock timestamps.
		c.Payload = json.RawMessage(" { \"label\" : \"<chair>&\" } ")
		apply(t, r, c)
		want := normalizeComponent(c)
		mutate := func(got Component) {
			t.Helper()
			if !reflect.DeepEqual(got, want) {
				t.Fatal("retained copy differs from normalized admission")
			}
			got.Payload[0] = '!'
			if got.Provenance.Simulation != nil {
				got.Provenance.Simulation.RunID = "changed"
			}
			if got.Provenance.OriginalCaptureTime != nil {
				*got.Provenance.OriginalCaptureTime = time.Time{}
			}
			for i := range got.Provenance.Inputs {
				p := &got.Provenance.Inputs[i]
				p.ID = "changed"
				if p.Simulation != nil {
					p.Simulation.RunID = "changed"
				}
				if p.OriginalCaptureTime != nil {
					*p.OriginalCaptureTime = time.Time{}
				}
			}
		}
		got, ok, err := r.Get(c.Key)
		if err != nil || !ok {
			t.Fatal(err)
		}
		mutate(got)
		for _, read := range []func() ([]Component, error){r.Snapshot, r.Records} {
			rows, err := read()
			if err != nil || len(rows) != 1 {
				t.Fatal(err)
			}
			mutate(rows[0])
		}
		cp, err := r.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		mutate(cp.Entries[0].Component)
		cloned, err := r.Clone()
		if err != nil {
			t.Fatal(err)
		}
		mutate(cloned.entries[c.Key].component)
		got, ok, err = r.Get(c.Key)
		if err != nil || !ok || !reflect.DeepEqual(got, want) {
			t.Fatal("a caller modified retained ancestry or payload", err)
		}
	}
}

func TestCachedKeySortPreservesJSONOrdering(t *testing.T) {
	var keys []Key
	for _, text := range []string{"a", "A", "<", ">", "&", "\\", "\"", "\n", "\u2028", "é", "\xff", "z"} {
		keys = append(keys, Key{Tenant: text, Entity: "fixed"}, Key{Entity: text, Source: Source{Device: text}})
	}
	want := append([]Key(nil), keys...)
	sort.Slice(want, func(i, j int) bool { return keyString(want[i]) < keyString(want[j]) })
	sortByKey(keys, func(k Key) Key { return k })
	if !reflect.DeepEqual(keys, want) {
		t.Fatal("cached keys changed escaped JSON ordering")
	}
}
