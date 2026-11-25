package selection

import (
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"strings"
	"testing"
	"time"
)

func TestCanonicalSelectionAndExactMatching(t *testing.T) {
	source := world.Source{Device: "d", App: "a", Service: "s", Stream: "x"}
	frame := world.FrameRef{ID: "floor", Revision: "1"}
	s := Selection{Types: []string{"z", "a"}, Entities: []string{"robot"}, Sources: []world.Source{source}, Frames: []world.FrameRef{frame}}
	n, err := s.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if s.Types[0] != "z" || n.Types[0] != "a" {
		t.Fatal("canonicalization mutates caller")
	}
	c := world.Component{Key: world.Key{Type: "a", Entity: "robot", Source: source}, Frame: frame}
	if !n.Matches(c) {
		t.Fatal("valid match")
	}
	c.Key.Source.Stream = "other"
	if n.Matches(c) {
		t.Fatal("partial source accepted")
	}
	c.Key.Source = source
	c.Frame.Revision = "2"
	if n.Matches(c) {
		t.Fatal("wrong frame revision")
	}
	for _, bad := range []Selection{{Types: []string{"a", "a"}}, {Entities: []string{" "}}, {Types: []string{strings.Repeat("x", 257)}}, {Sources: []world.Source{{Device: "d"}}}, {Frames: []world.FrameRef{{ID: "floor"}}}, {Types: make([]string, 33)}} {
		if _, err := bad.Normalize(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
func TestRequestAndResponseBounds(t *testing.T) {
	for _, r := range []Request{{Version: 2}, {PageSize: -1}, {PageSize: 129}, {MaxBytes: 4095}, {MaxBytes: 1 << 20}, {Cursor: strings.Repeat("x", 4097)}} {
		if _, err := r.Normalize(); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	for _, r := range []WatchRequest{{WaitMS: -1}, {WaitMS: 5001}, {Revision: strings.Repeat("x", 4097)}} {
		if _, err := r.Normalize(); err == nil {
			t.Fatal("unbounded watch")
		}
	}
	request, _ := (Request{Selection: Selection{Entities: []string{"yes"}}}).Normalize()
	r := Response{Version: 1, Components: []world.Component{}, Revision: "r", Complete: true, ObservedAt: time.Now()}
	if err := ValidateResponse(r, request); err != nil {
		t.Fatal(err)
	}
	r.Components = []world.Component{{Key: world.Key{Entity: "no"}}}
	if ValidateResponse(r, request) == nil {
		t.Fatal("unselected response")
	}
	r.Components[0].Key.Entity = "yes"
	r.Components = append(r.Components, r.Components[0])
	if ValidateResponse(r, request) == nil {
		t.Fatal("duplicate response")
	}
	r.Components = nil
	if ValidateResponse(r, request) == nil {
		t.Fatal("null page")
	}
}
