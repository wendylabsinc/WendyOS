package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"testing"
)

func regionalPolicy() *subscription.Policy {
	return &subscription.Policy{Version: 1, Tenant: "tenant", Site: "office", UnknownLocation: "retain", Selection: selection.Selection{Types: []string{"wendy.hazard.v1"}, Region: &selection.Region{Frame: world.FrameRef{ID: "floor", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{5, 5}}}, RequiredTypes: []string{sitepub.Type}}
}
func regionalValue(c world.Component, seq uint64, x int) world.Component {
	c.Key.Type = "wendy.hazard.v1"
	c.Version.Sequence = seq
	c.Payload, _ = json.Marshal(map[string]any{"position_m": []int{x, 0, 1}, "uncertainty_m": 0, "private_outside_marker": x == 20})
	return c
}
func TestRegionalFenceRestartConflictReentryAndCapacity(t *testing.T) {
	path, cfg, _, base := fixture(t)
	cfg.MaxEntries = 1
	p := regionalPolicy()
	s, e := OpenSubscribed(path, cfg, p)
	if e != nil {
		t.Fatal(e)
	}
	inside, outside := regionalValue(base, 1, 1), regionalValue(base, 2, 20)
	apply(t, s, inside)
	apply(t, s, outside)
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("private_outside_marker")) {
		t.Fatal("payload retained in fence")
	}
	cp, e := s.RepairSnapshot()
	if e != nil || len(cp.Entries) != 0 || len(cp.Fences) != 1 {
		t.Fatal(cp, e)
	}
	s.Close()
	s, e = OpenSubscribed(path, cfg, p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if apply(t, s, inside) != world.Obsolete {
		t.Fatal("old record resurrected")
	}
	if apply(t, s, outside) != world.Duplicate {
		t.Fatal("outside duplicate differs")
	}
	conflict := outside
	conflict.Payload = json.RawMessage(`{"position_m":[1,0,1],"uncertainty_m":0}`)
	if _, e = s.Apply(conflict.Key.Source, conflict); !errors.Is(e, world.ErrConflict) {
		t.Fatal("fence conflict lost", e)
	}
	other := outside
	other.Key.Entity = "other"
	if _, e = s.Apply(other.Key.Source, other); !errors.Is(e, world.ErrCapacity) {
		t.Fatal("fences evade capacity", e)
	}
	apply(t, s, regionalValue(base, 3, 1))
	got, e := s.Snapshot()
	if e != nil || len(got) != 1 {
		t.Fatal("newer inside failed", e)
	}
	tomb := regionalValue(base, 4, 1)
	tomb.Tombstone = true
	tomb.Payload = nil
	apply(t, s, tomb)
	cp, e = s.RepairSnapshot()
	if e != nil || len(cp.Fences) != 0 || len(cp.Entries) != 1 || !cp.Entries[0].Component.Tombstone {
		t.Fatal("origin tombstone lost", e)
	}
}
func TestRegionalEveryAdmissionPathAndFailedWriteIsAtomic(t *testing.T) {
	for _, kind := range []string{"direct", "prepared", "batch"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, _, base := fixture(t)
			s, e := OpenSubscribed(path, cfg, regionalPolicy())
			if e != nil {
				t.Fatal(e)
			}
			inside, outside := regionalValue(base, 1, 1), regionalValue(base, 2, 20)
			apply(t, s, inside)
			before, _ := os.ReadFile(path)
			s.write = func(string, []byte) error { return errors.New("injected persistence failure") }
			switch kind {
			case "direct":
				_, e = s.Apply(outside.Key.Source, outside)
			case "prepared":
				_, e = s.ApplyPrepared(outside.Key.Source, outside, func(PublicationState) error { return nil })
			case "batch":
				_, e = s.ApplyPreparedBatch(repairPage(outside), func(BatchPublicationState) error { return nil })
			}
			if e == nil {
				t.Fatal("write failure accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("failed write changed state")
			}
			s.Close()
			s, e = OpenSubscribed(path, cfg, regionalPolicy())
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			rows, e := s.Snapshot()
			if e != nil || len(rows) != 1 || rows[0].Version != inside.Version {
				t.Fatal("old record lost", e)
			}
		})
	}
}
func TestRegionalUnknownFrameAndPolicyMismatch(t *testing.T) {
	path, cfg, _, base := fixture(t)
	p := regionalPolicy()
	s, e := OpenSubscribed(path, cfg, p)
	if e != nil {
		t.Fatal(e)
	}
	unknown := regionalValue(base, 1, 1)
	unknown.Payload = json.RawMessage(`{"position_m":null,"uncertainty_m":0}`)
	apply(t, s, unknown)
	cp, _ := s.RepairSnapshot()
	if len(cp.Entries) != 1 {
		t.Fatal("unknown silently dropped")
	}
	moved := regionalValue(base, 2, 1)
	moved.Frame.Revision = "2"
	apply(t, s, moved)
	cp, _ = s.RepairSnapshot()
	if len(cp.Fences) != 1 || len(cp.Entries) != 0 {
		t.Fatal("mismatched frame relabeled")
	}
	s.Close()
	p.Selection.Region.MaxXZ[0] = 6
	if reopened, e := OpenSubscribed(path, cfg, p); !errors.Is(e, subscription.ErrMismatch) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("policy changed", e)
	}
}
