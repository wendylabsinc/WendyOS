package twin

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"os"
	"sync"
	"testing"
)

func room() Room {
	return Room{Site: Site{Version: 1, ID: uuid.NewString(), Name: "Robot room", ModelRevision: uuid.NewString(), MetresPerModelUnit: 1, Cameras: []json.RawMessage{}}}
}
func TestRobotStartsRoomAndPersistsWithoutAppleModel(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	r, e := s.Put(room())
	if e != nil {
		t.Fatal(e)
	}
	contribution := Contribution{Source: "g1:lidar", Sequence: 1, ModelRevision: r.Site.ModelRevision, Points: []Point{{1, 2, 3}, {1.01, 2.01, 3.01}}}
	if e = s.Contribute(r.Site.ID, contribution); e != nil {
		t.Fatal(e)
	}
	restarted, _ := NewStore(dir)
	saved, e := restarted.Get(r.Site.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(saved.Points) != 1 || saved.Model != nil || saved.Revision != 2 {
		t.Fatalf("unexpected restored room: %+v", saved)
	}
	if e = restarted.Contribute(r.Site.ID, contribution); !errors.Is(e, ErrConflict) {
		t.Fatalf("replayed packet: %v", e)
	}
	contribution.Sequence++
	contribution.ModelRevision = uuid.NewString()
	if e = restarted.Contribute(r.Site.ID, contribution); e == nil {
		t.Fatal("accepted foreign frame")
	}
	if _, e = restarted.Put(r); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale client overwrote observations: %v", e)
	}
}
func TestConcurrentContributorsAndEdits(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	r, _ := s.Put(room())
	var wg sync.WaitGroup
	for _, source := range []string{"go2", "ios", "g1"} {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			if e := s.Contribute(r.Site.ID, Contribution{Source: source, Sequence: 1, ModelRevision: r.Site.ModelRevision, Points: []Point{{1, 2, 3}}}); e != nil {
				t.Error(e)
			}
		}(source)
	}
	wg.Wait()
	saved, _ := s.Get(r.Site.ID)
	if len(saved.Sources) != 3 {
		t.Fatal(saved.Sources)
	}
	saved.Site.Name = "Shared room"
	saved.Points = nil
	saved.Sources = nil
	updated, e := s.Put(saved)
	if e != nil || len(updated.Points) != 1 || len(updated.Sources) != 3 {
		t.Fatalf("metadata edit erased map: %+v %v", updated, e)
	}
}
func TestTraversalAndInvalidPoint(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	r, _ := s.Put(room())
	if _, e := s.Get("../x"); e == nil {
		t.Fatal("accepted traversal")
	}
	if e := s.Contribute(r.Site.ID, Contribution{Source: "g1", Sequence: 1, ModelRevision: r.Site.ModelRevision, Points: []Point{{1e10, 0, 0}}}); e == nil {
		t.Fatal("accepted invalid point")
	}
}

func TestImmutableAssetsAreSeparateFromMutableRoomState(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	r := room()
	r.Model = []byte("PK\x03\x04render-model")
	created, e := s.Put(r)
	if e != nil {
		t.Fatal(e)
	}
	metadata, e := os.ReadFile(s.path(r.Site.ID))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(metadata, []byte(`"model":`)) {
		t.Fatal("model embedded in mutable state")
	}
	c := Contribution{Source: "g1", Sequence: 1, ModelRevision: r.Site.ModelRevision, Points: []Point{{1, 0, 1}}}
	if e = s.Contribute(r.Site.ID, c); e != nil {
		t.Fatal(e)
	}
	restart, _ := NewStore(dir)
	loaded, e := restart.Get(r.Site.ID)
	if e != nil || !bytes.Equal(loaded.Model, r.Model) {
		t.Fatalf("lost asset: %v", e)
	}
	state, e := restart.State(r.Site.ID)
	if e != nil || state.Model != nil || !state.HasModel {
		t.Fatalf("state response included/lost asset: %+v %v", state, e)
	}
	state.Site.Name = "Renamed"
	updated, e := restart.Put(state)
	if e != nil || !bytes.Equal(updated.Model, created.Model) {
		t.Fatalf("metadata update lost model: %v", e)
	}
	updated.Model = []byte("PK\x03\x04different")
	if _, e = restart.Put(updated); e == nil {
		t.Fatal("silently changed an aligned model")
	}
}
