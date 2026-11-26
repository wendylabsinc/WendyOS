package spatialsim

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

func mustSite(t testing.TB, build func(uint64, Options) (*Site, error), seed uint64, opt Options) *Site {
	t.Helper()
	s, err := build(seed, opt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// office is the standard site with the default configuration, shared by the
// tests that only read it.
var office = sync.OnceValues(func() (*Site, error) { return Office(1, Options{}) })

func officeSite(t testing.TB) *Site {
	t.Helper()
	s, err := office()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const (
	// Bodies must keep this far from walls, closed doors and furniture
	// beyond their own radius.
	routeMargin = 0.05
	// Two moving bodies must never overlap.
	bodyMargin = 0.1
	// Routes are sampled this often for the free-space checks.
	routeStep = 100 * time.Millisecond
)

// body is a moving thing at one instant, for the free-space checks.
type body struct {
	id     string
	p      geom.Vec2
	radius float64
	lv     *level
}

func bodies(s *Site, t time.Duration) []body {
	var out []body
	for _, d := range s.devices {
		if d.route != nil && t >= d.start {
			p, _, lv := s.place(d, t)
			out = append(out, body{string(d.id), p.Pos(), d.radius, lv})
		}
	}
	for _, a := range s.actors {
		sh, _ := a.at(t)
		out = append(out, body{a.id, sh.Centre, sh.Bound(), a.level})
	}
	return out
}

// TestRoutesStayInFreeSpace walks every robot, handheld and actor through
// the whole scenario and checks that it stays inside a room, clear of walls,
// closed doors and furniture, and never overlaps another moving body.
func TestRoutesStayInFreeSpace(t *testing.T) {
	for name, s := range map[string]*Site{
		"office":   officeSite(t),
		"room":     mustSite(t, SingleRoom, 1, Options{}),
		"corridor": mustSite(t, Corridor, 1, Options{}),
		"tworooms": mustSite(t, TwoRooms, 1, Options{}),
	} {
		failures := 0
		fail := func(format string, args ...any) {
			if failures++; failures <= 20 {
				t.Errorf(name+": "+format, args...)
			}
		}
		prev := map[string]geom.Vec2{}
		for at := time.Duration(0); at <= s.Duration(); at += routeStep {
			bs := bodies(s, at)
			scenes := map[*level]*scene{}
			for i, b := range bs {
				sc := scenes[b.lv]
				if sc == nil {
					sc = s.scene(b.lv, at)
					scenes[b.lv] = sc
				}
				if room, _ := b.lv.roomAt(b.p); room == nil {
					fail("%s at %v is outside every room at %v", b.id, b.p, at)
				}
				clear := b.radius + routeMargin
				for _, w := range append(append([]geom.Segment{}, b.lv.walls...), sc.doors...) {
					if d := w.DistanceTo(b.p); d < clear {
						fail("%s at %v is %.2f m from wall %v at %v", b.id, b.p, d, w, at)
					}
					if last, ok := prev[b.id]; ok && cut(last, b.p, w) {
						fail("%s passes through wall %v at %v", b.id, w, at)
					}
				}
				for _, f := range b.lv.furniture {
					sh, ok := f.at(at)
					if !ok {
						continue
					}
					c := sh.Corners()
					d := b.p.Dist(sh.Centre) - sh.Radius
					if sh.Radius == 0 {
						d = math.Inf(1)
						for k := 0; k < 4; k++ {
							d = math.Min(d, geom.Segment{A: c[k], B: c[(k+1)%4]}.DistanceTo(b.p))
						}
					}
					if d < clear || sh.Contains(b.p) {
						fail("%s at %v is %.2f m from %s at %v", b.id, b.p, d, f.id, at)
					}
				}
				for _, o := range bs[i+1:] {
					if d := b.p.Dist(o.p); o.lv == b.lv && d < b.radius+o.radius+bodyMargin {
						fail("%s and %s are %.2f m apart at %v", b.id, o.id, d, at)
					}
				}
				prev[b.id] = b.p
			}
		}
		if failures > 20 {
			t.Errorf("%s: %d violations in total", name, failures)
		}
	}
}
