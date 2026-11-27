package geom

import (
	"errors"
	"math"
	"testing"
)

func TestSegment(t *testing.T) {
	s := Segment{Vec2{0, 0}, Vec2{4, 0}}
	near(t, s.Length(), 4, exactTol, "length")
	near(t, s.DistanceTo(Vec2{2, 3}), 3, exactTol, "distance to interior")
	near(t, s.DistanceTo(Vec2{7, 4}), 5, exactTol, "distance past the end")
	near(t, (Segment{Vec2{1, 1}, Vec2{1, 1}}).DistanceTo(Vec2{4, 5}), 5, exactTol, "point segment")
	if s.Midpoint() != (Vec2{2, 0}) || s.Dir() != (Vec2{1, 0}) {
		t.Fatal("midpoint or direction")
	}
	p, ok := s.Intersect(Segment{Vec2{1, -1}, Vec2{1, 1}})
	if !ok || p.Dist(Vec2{1, 0}) > exactTol {
		t.Fatalf("crossing: %+v %v", p, ok)
	}
	if _, ok := s.Intersect(Segment{Vec2{5, -1}, Vec2{5, 1}}); ok {
		t.Fatal("segments that miss reported a crossing")
	}
	if _, ok := s.Intersect(Segment{Vec2{0, 1}, Vec2{4, 1}}); ok {
		t.Fatal("parallel segments reported a crossing")
	}
	near(t, SegmentDistance(s, Segment{Vec2{1, -1}, Vec2{1, 1}}), 0, exactTol, "crossing segments")
	near(t, SegmentDistance(s, Segment{Vec2{0, 2}, Vec2{4, 2}}), 2, exactTol, "parallel segments")
	near(t, SegmentDistance(s, Segment{Vec2{7, 4}, Vec2{9, 9}}), 5, exactTol, "endpoint to endpoint")
}

func TestRayCasting(t *testing.T) {
	wall := Segment{Vec2{3, -2}, Vec2{3, 2}}
	r, ok := RaySegment(Vec2{0, 0}, Vec2{1, 0}, wall)
	if !ok {
		t.Fatal("ray missed the wall")
	}
	near(t, r, 3, exactTol, "range to wall")
	r, ok = RaySegment(Vec2{0, 0}, Heading(math.Pi/6), wall)
	if !ok {
		t.Fatal("oblique ray missed the wall")
	}
	near(t, r, 3/math.Cos(math.Pi/6), roundTol, "oblique range")
	if _, ok := RaySegment(Vec2{0, 0}, Vec2{-1, 0}, wall); ok {
		t.Fatal("ray pointing away hit the wall")
	}
	if _, ok := RaySegment(Vec2{0, 0}, Heading(1.2), wall); ok {
		t.Fatal("ray passing the end hit the wall")
	}
	if _, ok := RaySegment(Vec2{0, 0}, Vec2{0, 1}, wall); ok {
		t.Fatal("parallel ray hit the wall")
	}
	r, ok = RayCircle(Vec2{0, 0}, Vec2{1, 0}, Vec2{5, 0}, 1)
	if !ok {
		t.Fatal("ray missed the circle")
	}
	near(t, r, 4, exactTol, "near side of the circle")
	r, ok = RayCircle(Vec2{5, 0}, Vec2{0, 1}, Vec2{5, 0}, 1)
	if !ok {
		t.Fatal("ray from inside missed")
	}
	near(t, r, 1, exactTol, "exit point from inside")
	if _, ok := RayCircle(Vec2{0, 0}, Vec2{1, 0}, Vec2{5, 2}, 1); ok {
		t.Fatal("ray passing beside the circle hit")
	}
	if _, ok := RayCircle(Vec2{0, 0}, Vec2{-1, 0}, Vec2{5, 0}, 1); ok {
		t.Fatal("ray pointing away hit the circle")
	}
	// A unit direction makes t a range: check against the hit point on a random circle.
	rng := newRand(70)
	for i := 0; i < 500; i++ {
		o, c := Vec2{rng.Float64() * 4, rng.Float64() * 4}, Vec2{6 + rng.Float64()*4, rng.Float64() * 4}
		d := Heading(c.Sub(o).Angle() + (rng.Float64()-0.5)*0.1)
		if tt, ok := RayCircle(o, d, c, 0.5); ok {
			near(t, o.Add(d.Scale(tt)).Dist(c), 0.5, roundTol, "hit lies on the circle")
		}
	}
}

func TestPolygon(t *testing.T) {
	// An L-shaped room: 4x4 with a 2x2 notch removed from the +X +Z corner.
	room := []Vec2{{0, 0}, {4, 0}, {4, 2}, {2, 2}, {2, 4}, {0, 4}}
	for _, p := range []Vec2{{1, 1}, {3, 1}, {1, 3}, {0.01, 3.99}} {
		if !PointInPolygon(p, room) {
			t.Fatalf("%+v must be inside", p)
		}
	}
	for _, p := range []Vec2{{3, 3}, {-1, 1}, {5, 1}, {1, 5}, {2.01, 2.01}} {
		if PointInPolygon(p, room) {
			t.Fatalf("%+v must be outside", p)
		}
	}
	near(t, PolygonArea(room), 12, exactTol, "area")
	near(t, PolygonSignedArea(room), 12, exactTol, "vertices turning from +X toward +Z have positive area")
	reversed := []Vec2{room[5], room[4], room[3], room[2], room[1], room[0]}
	near(t, PolygonSignedArea(reversed), -12, exactTol, "reversed winding")
	if !PointInPolygon(Vec2{1, 1}, reversed) {
		t.Fatal("winding must not affect containment")
	}
	c, err := PolygonCentroid(room)
	if err != nil {
		t.Fatal(err)
	}
	// Three 2x2 squares centred at (1,1), (3,1), (1,3).
	near(t, c.X, 5.0/3, exactTol, "centroid x")
	near(t, c.Z, 5.0/3, exactTol, "centroid z")
	// The centroid is translation invariant even far from the origin.
	far := make([]Vec2, len(room))
	for i, v := range room {
		far[i] = v.Add(Vec2{1e6, -1e6})
	}
	c, err = PolygonCentroid(far)
	if err != nil {
		t.Fatal(err)
	}
	near(t, c.X-1e6, 5.0/3, 1e-8, "far centroid x")
	near(t, c.Z+1e6, 5.0/3, 1e-8, "far centroid z")
	if _, err := PolygonCentroid(room[:2]); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("two vertices: %v", err)
	}
	if _, err := PolygonCentroid([]Vec2{{0, 0}, {1, 1}, {2, 2}}); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("collinear polygon: %v", err)
	}
	if PointInPolygon(Vec2{0, 0}, nil) {
		t.Fatal("empty polygon contains nothing")
	}
}
