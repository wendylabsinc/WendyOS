package twin

import (
	"testing"
)

func TestAccumulatedRobotMapReplacesGhostsAndPreservesOtherSources(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	r, _ := s.Put(room())
	put := func(source string, sequence uint64, replace bool, points ...Point) {
		t.Helper()
		if err := s.Contribute(r.Site.ID, Contribution{Source: source, Sequence: sequence, ModelRevision: r.Site.ModelRevision, Replace: replace, Points: points}); err != nil {
			t.Fatal(err)
		}
	}
	put("go2:robot", 1, true, Point{1, 0, 1})
	put("ios:scan", 1, false, Point{2, 0, 2})
	put("go2:robot", 2, true, Point{3, 0, 3})
	saved, err := s.Get(r.Site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Points) != 2 {
		t.Fatalf("ghost scan retained: %v", saved.Points)
	}
	for _, p := range saved.Points {
		if p.X == 1 {
			t.Fatal("old alignment persisted")
		}
	}
	// Room edits and process restarts preserve source ownership.
	saved.Site.Name = "Edited room"
	saved.PointLayers, saved.LegacyPoints = nil, nil
	if _, err = s.Put(saved); err != nil {
		t.Fatal(err)
	}
	s, _ = NewStore(s.directory)
	put("go2:robot", 3, true, Point{4, 0, 4})
	saved, _ = s.Get(r.Site.ID)
	if len(saved.Points) != 2 {
		t.Fatal("source ownership lost across restart")
	}
}

func TestLegacyAccumulatedMapMigrationPreservesUnknownOwnership(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		s, _ := NewStore(t.TempDir())
		r, _ := s.Put(room())
		r.Points = []Point{{1, 0, 1}}
		r.Sources = map[string]uint64{"go2:robot": 1}
		if mixed {
			r.Sources["ios:scan"] = 1
		}
		if err := s.write(r); err != nil {
			t.Fatal(err)
		}
		if err := s.Contribute(r.Site.ID, Contribution{Source: "go2:robot", Sequence: 2, ModelRevision: r.Site.ModelRevision, Replace: true, Points: []Point{{2, 0, 2}}}); err != nil {
			t.Fatal(err)
		}
		saved, _ := s.Get(r.Site.ID)
		want := 1
		if mixed {
			want = 2
		}
		if len(saved.Points) != want {
			t.Fatalf("mixed=%v: %v", mixed, saved.Points)
		}
	}
}
