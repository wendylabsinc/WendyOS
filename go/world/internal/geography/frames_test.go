package geography

import (
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var frameTestTime = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
var frameTestRoot = world.FrameRef{ID: "site", Revision: "root-v1"}

func frameFixture() []world.LocalFrame {
	q := math.Sqrt(.5)
	return []world.LocalFrame{
		{Ref: world.FrameRef{ID: "floor", Revision: "v1"}, Parent: frameTestRoot, Translation: [3]float64{10, 2, 3}, RotationXYZW: [4]float64{0, q, 0, q}, ValidFrom: frameTestTime.Add(-time.Hour), ValidUntil: frameTestTime.Add(time.Hour), UncertaintyMetres: .2},
		{Ref: world.FrameRef{ID: "room", Revision: "v1"}, Parent: world.FrameRef{ID: "floor", Revision: "v1"}, Translation: [3]float64{1, 0, 2}, RotationXYZW: [4]float64{0, 0, q, q}, ValidFrom: frameTestTime.Add(-time.Minute), ValidUntil: frameTestTime.Add(time.Minute), UncertaintyMetres: .1},
		{Ref: world.FrameRef{ID: "camera", Revision: "v1"}, Parent: frameTestRoot, Translation: [3]float64{-1, 0, 1}, RotationXYZW: [4]float64{0, 0, 0, 1}, ValidFrom: frameTestTime.Add(-2 * time.Hour), ValidUntil: frameTestTime.Add(2 * time.Hour), UncertaintyMetres: .4},
	}
}
func mustFrameGraph(t *testing.T, frames []world.LocalFrame) *Graph {
	t.Helper()
	g, err := NewGraph(frameTestRoot, frames)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func frameNear(t *testing.T, got, want [3]float64) {
	t.Helper()
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-10 {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestKnownNestedRotationsAndInverse(t *testing.T) {
	f := frameFixture()
	g := mustFrameGraph(t, f)
	// +90 around Z: (2,3,4) -> (-3,2,4), then translate -> (-2,2,6).
	// +90 around Y: (-2,2,6) -> (6,2,2), then translate -> (16,4,5).
	r, err := g.Transform([3]float64{2, 3, 4}, f[1].Ref, frameTestRoot, frameTestTime)
	if err != nil {
		t.Fatal(err)
	}
	frameNear(t, r.Position, [3]float64{16, 4, 5})
	r, err = g.Transform([3]float64{2, 3, 4}, f[1].Ref, f[2].Ref, frameTestTime)
	if err != nil {
		t.Fatal(err)
	}
	frameNear(t, r.Position, [3]float64{17, 4, 4})
	if !reflect.DeepEqual(r.Path, []world.FrameRef{f[1].Ref, f[0].Ref, frameTestRoot, f[2].Ref}) {
		t.Fatal(r.Path)
	}
	if math.Abs(r.UncertaintyMetres-.7) > 1e-12 || r.ValidFrom != f[1].ValidFrom || r.ValidUntil != f[1].ValidUntil {
		t.Fatal(r)
	}
	reverse, err := g.Transform([3]float64{17, 4, 4}, f[2].Ref, f[1].Ref, frameTestTime)
	if err != nil {
		t.Fatal(err)
	}
	frameNear(t, reverse.Position, [3]float64{2, 3, 4})
}

func TestCommonAncestorsStillConstrainValidityAndUncertainty(t *testing.T) {
	f := frameFixture()
	g := mustFrameGraph(t, f)
	r, err := g.Transform([3]float64{2, 3, 4}, f[1].Ref, f[1].Ref, frameTestTime)
	if err != nil {
		t.Fatal(err)
	}
	frameNear(t, r.Position, [3]float64{2, 3, 4})
	if math.Abs(r.UncertaintyMetres-.3) > 1e-12 || len(r.Path) != 1 || r.Path[0] != f[1].Ref || r.ValidUntil != f[1].ValidUntil {
		t.Fatal(r)
	}
	f[0].ValidUntil = frameTestTime
	g = mustFrameGraph(t, f)
	if _, err = g.Transform([3]float64{}, f[1].Ref, f[1].Ref, frameTestTime); err == nil {
		t.Fatal("expired ancestor accepted for identity transform")
	}
	if _, err = g.Transform([3]float64{}, f[1].Ref, f[0].Ref, frameTestTime); err == nil {
		t.Fatal("expired common ancestor accepted")
	}
}

func TestExplicitRevisionsAndHalfOpenValidity(t *testing.T) {
	f := frameFixture()
	old := f[0]
	old.Ref.Revision = "old"
	old.Translation = [3]float64{20, 2, 3}
	f = append(f, old)
	g := mustFrameGraph(t, f)
	r, err := g.Transform([3]float64{}, old.Ref, f[0].Ref, frameTestTime)
	if err != nil {
		t.Fatal(err)
	}
	frameNear(t, r.Position, [3]float64{0, 0, 10})
	for _, at := range []time.Time{f[1].ValidFrom.Add(-time.Nanosecond), f[1].ValidUntil} {
		if _, err = g.Transform([3]float64{}, f[1].Ref, frameTestRoot, at); err == nil {
			t.Fatal("accepted outside half-open validity", at)
		}
	}
	if _, err = g.Transform([3]float64{}, f[1].Ref, frameTestRoot, f[1].ValidFrom); err != nil {
		t.Fatal(err)
	}
	unknown := world.FrameRef{ID: f[0].Ref.ID, Revision: "unpublished"}
	if _, err = g.Transform([3]float64{}, unknown, unknown, frameTestTime); err == nil {
		t.Fatal("unknown same-frame revision accepted")
	}
	if _, err = g.Transform([3]float64{}, frameTestRoot, unknown, frameTestTime); err == nil {
		t.Fatal("unknown target revision accepted")
	}
}

func TestGraphRejectsMalformedTopology(t *testing.T) {
	tests := map[string]func([]world.LocalFrame) []world.LocalFrame{
		"duplicate":                func(f []world.LocalFrame) []world.LocalFrame { return append(f, f[0]) },
		"conflicting duplicate":    func(f []world.LocalFrame) []world.LocalFrame { x := f[0]; x.Translation[0]++; return append(f, x) },
		"dangling parent revision": func(f []world.LocalFrame) []world.LocalFrame { f[1].Parent.Revision = "absent"; return f },
		"cycle":                    func(f []world.LocalFrame) []world.LocalFrame { f[0].Parent = f[1].Ref; return f },
		"root redefinition": func(f []world.LocalFrame) []world.LocalFrame {
			f[2].Ref = frameTestRoot
			f[2].Parent = f[1].Ref
			return f
		},
		"nan translation": func(f []world.LocalFrame) []world.LocalFrame { f[0].Translation[0] = math.NaN(); return f },
		"huge translation": func(f []world.LocalFrame) []world.LocalFrame {
			f[0].Translation[0] = MaxFrameCoordinateMetres + 1
			return f
		},
		"bad quaternion":       func(f []world.LocalFrame) []world.LocalFrame { f[0].RotationXYZW = [4]float64{0, 0, 0, 2}; return f },
		"missing validity":     func(f []world.LocalFrame) []world.LocalFrame { f[0].ValidFrom = time.Time{}; return f },
		"negative uncertainty": func(f []world.LocalFrame) []world.LocalFrame { f[0].UncertaintyMetres = -1; return f },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewGraph(frameTestRoot, change(frameFixture())); err == nil {
				t.Fatal("accepted malformed graph")
			}
		})
	}
	if _, err := NewGraph(world.FrameRef{}, nil); err == nil {
		t.Fatal("accepted absent root")
	}
	if _, err := NewGraph(frameTestRoot, make([]world.LocalFrame, MaxFrameEdges+1)); err == nil {
		t.Fatal("accepted excess edges")
	}
	frames := make([]world.LocalFrame, MaxFrameDepth+1)
	parent := frameTestRoot
	for i := range frames {
		frames[i] = world.LocalFrame{Ref: world.FrameRef{ID: fmt.Sprintf("depth-%d", i), Revision: "v1"}, Parent: parent, RotationXYZW: [4]float64{0, 0, 0, 1}, ValidFrom: frameTestTime.Add(-time.Second), ValidUntil: frameTestTime.Add(time.Second)}
		parent = frames[i].Ref
	}
	if _, err := NewGraph(frameTestRoot, frames); err == nil {
		t.Fatal("accepted excess depth")
	}
	g := mustFrameGraph(t, frames[:MaxFrameDepth])
	if _, err := g.Transform([3]float64{}, frames[MaxFrameDepth-1].Ref, frameTestRoot, frameTestTime); err != nil {
		t.Fatal(err)
	}
}

func TestFrameGraphCopiesAndConcurrentReads(t *testing.T) {
	f := frameFixture()
	g := mustFrameGraph(t, f)
	room := f[1].Ref
	f[0].Translation = [3]float64{100, 100, 100}
	f[1].Ref.Revision = "tampered"
	r, err := g.Transform([3]float64{2, 3, 4}, room, frameTestRoot, frameTestTime)
	if err != nil {
		t.Fatal(err)
	}
	r.Path[0] = world.FrameRef{}
	var group sync.WaitGroup
	check := func() {
		defer group.Done()
		for range 20 {
			r, err := g.Transform([3]float64{2, 3, 4}, room, frameTestRoot, frameTestTime)
			if err != nil {
				t.Error(err)
				return
			}
			frameNear(t, r.Position, [3]float64{16, 4, 5})
			if r.Path[0] != room {
				t.Error(r.Path)
			}
		}
	}
	group.Add(2)
	go check()
	go check()
	group.Wait()
}

func TestTransformInputAndArithmeticBounds(t *testing.T) {
	g := mustFrameGraph(t, nil)
	r, err := g.Transform([3]float64{1, 2, 3}, frameTestRoot, frameTestRoot, frameTestTime)
	if err != nil || !r.ValidFrom.IsZero() || !r.ValidUntil.IsZero() || r.UncertaintyMetres != 0 {
		t.Fatal(r, err)
	}
	if _, err = g.Transform([3]float64{}, frameTestRoot, frameTestRoot, time.Time{}); err == nil {
		t.Fatal("accepted absent time")
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), MaxFrameCoordinateMetres + 1} {
		if _, err = g.Transform([3]float64{v, 0, 0}, frameTestRoot, frameTestRoot, frameTestTime); err == nil {
			t.Fatal("accepted invalid point")
		}
	}
	if _, err = (*Graph)(nil).Transform([3]float64{}, frameTestRoot, frameTestRoot, frameTestTime); err == nil {
		t.Fatal("nil graph")
	}
	if _, err = (&Graph{}).Transform([3]float64{}, frameTestRoot, frameTestRoot, frameTestTime); err == nil {
		t.Fatal("uninitialized graph")
	}
	f := frameFixture()
	f[2].Translation[0] = MaxFrameCoordinateMetres
	g = mustFrameGraph(t, f)
	if _, err = g.Transform([3]float64{1, 0, 0}, f[2].Ref, frameTestRoot, frameTestTime); err == nil {
		t.Fatal("accepted out-of-bound result")
	}
	f = frameFixture()
	f[0].UncertaintyMetres = .7e9
	f[1].UncertaintyMetres = .7e9
	g = mustFrameGraph(t, f)
	if _, err = g.Transform([3]float64{}, f[1].Ref, frameTestRoot, frameTestTime); err == nil {
		t.Fatal("accepted excessive uncertainty sum")
	}
}

func TestAcceptedQuaternionToleranceRemainsRigid(t *testing.T) {
	f := frameFixture()
	f[2].Translation = [3]float64{}
	q := math.Sqrt(.5) * (1 + 1e-7)
	f[2].RotationXYZW = [4]float64{0, 0, q, q}
	g := mustFrameGraph(t, f)
	r, err := g.Transform([3]float64{3, 4, 0}, f[2].Ref, frameTestRoot, frameTestTime)
	if err != nil {
		t.Fatal(err)
	}
	frameNear(t, r.Position, [3]float64{-4, 3, 0})
}
