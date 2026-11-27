package sitenav

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
)

type legacyReferenceIndex map[referenceCell][]Surface

func legacyReference(points []Surface) legacyReferenceIndex {
	cells := make(legacyReferenceIndex)
	for _, p := range points {
		k := surfaceReferenceKey(p.Position)
		cells[k] = append(cells[k], p)
	}
	return cells
}

// Kept as the exact previous 27-probe traversal for equivalence verification.
func (index legacyReferenceIndex) distance(p Vertex, direction Point, horizontal bool) (float64, bool) {
	best, stable := .0625, false
	k := surfaceReferenceKey(p)
	for dz := -1; dz <= 1; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				for _, v := range index[referenceCell{k.X + dx, k.Y + dy, k.Z + dz}] {
					if v.Horizontal != horizontal || (!horizontal && math.Abs(v.Direction.X*direction.X+v.Direction.Y*direction.Y) < .966) {
						continue
					}
					d := (v.Position.X-p.X)*(v.Position.X-p.X) + (v.Position.Y-p.Y)*(v.Position.Y-p.Y) + (v.Position.Z-p.Z)*(v.Position.Z-p.Z)
					if d < best {
						best, stable = d, v.Stable
					}
				}
			}
		}
	}
	return best, stable
}
func referenceTestPoints(n int, sparse bool) []Surface {
	out := make([]Surface, n)
	step := .05
	if sparse {
		step = 1
	}
	for i := range out {
		out[i] = Surface{Position: Vertex{float64(i%50) * step, float64(i/50) * step, float64(i%3) * .2}, Direction: Point{1, 0}, Horizontal: i%5 == 0, Stable: i%7 == 0}
	}
	return out
}
func TestReferenceIndexExactlyMatchesPreviousTraversal(t *testing.T) {
	random := rand.New(rand.NewSource(1729))
	points := referenceTestPoints(600, false)
	for i := range points {
		if i%3 == 0 {
			points[i].Direction = Point{0, 1}
		}
		points[i].Position.X -= .75
		points[i].Position.Y -= .5
	}
	index, err := newSurfaceReferenceIndex(points)
	if err != nil {
		t.Fatal(err)
	}
	old := legacyReference(points)
	for i := 0; i < 10000; i++ {
		p := Vertex{random.Float64()*5 - 2, random.Float64()*3 - 1, random.Float64()*1.2 - .4}
		direction := Point{1, 0}
		if i%3 == 0 {
			direction = Point{0, 1}
		}
		if i < 100 {
			p = Vertex{float64(i%10-5) * .2, float64(i/10-5) * .2, .2}
		} // exact negative/positive cell boundaries
		want, stable := old.distance(p, direction, i%5 == 0)
		got, gotStable := index.distance(p, direction, i%5 == 0)
		if math.Float64bits(want) != math.Float64bits(got) || stable != gotStable {
			t.Fatalf("query%+v: old(%v,%v) new(%v,%v)", p, want, stable, got, gotStable)
		}
	}
}
func TestReferenceIndexPreservesEqualDistanceStableTies(t *testing.T) {
	for scenario := 0; scenario < 4; scenario++ {
		sameCell := scenario%2 == 1
		points := []Surface{{Position: Vertex{.125, 0, .5}, Direction: Point{1, 0}, Stable: true}, {Position: Vertex{-.125, 0, .5}, Direction: Point{1, 0}, Stable: false}}
		if sameCell {
			points[1].Position = points[0].Position
		}
		if scenario >= 2 {
			for _, p := range referenceTestPoints(300, true) {
				p.Position.X += 10
				points = append(points, p)
			}
		}
		index, err := newSurfaceReferenceIndex(points)
		if err != nil {
			t.Fatal(err)
		}
		if index.expanded == (scenario >= 2) {
			t.Fatal("tie fixture selected wrong index mode")
		}
		old := legacyReference(points)
		got, stable := index.distance(Vertex{0, 0, .5}, Point{1, 0}, false)
		want, wantStable := old.distance(Vertex{0, 0, .5}, Point{1, 0}, false)
		if got != want || stable != wantStable || stable != sameCell {
			t.Fatalf("tie order changed: samecell%v d%v stable%v", sameCell, got, stable)
		}
	}
}
func TestReferenceIndexExpansionBoundAndDetachedInput(t *testing.T) {
	points := referenceTestPoints(3000, true)
	index, err := newSurfaceReferenceIndex(points)
	if err != nil {
		t.Fatal(err)
	}
	entries := 0
	for _, span := range index.neighbors {
		entries += int(span.count)
		if int(span.offset+span.count) > len(index.indices) {
			t.Fatal("reference span outside arena")
		}
	}
	if index.expanded || entries != len(points) || len(index.neighbors) > len(points) || len(index.indices) != entries || cap(index.indices) != entries {
		t.Fatalf("unbounded index: buckets%d indices%d capacity%d", len(index.neighbors), entries, cap(index.indices))
	}
	old := legacyReference(points)
	for _, p := range points {
		p.Position.X -= .025
		got, stable := index.distance(p.Position, p.Direction, p.Horizontal)
		want, wantStable := old.distance(p.Position, p.Direction, p.Horizontal)
		if got != want || stable != wantStable {
			t.Fatal("sparse fallback changed result")
		}
	}
	point := points[0]
	points[0].Position.X = 999
	got, _ := index.distance(point.Position, point.Direction, point.Horizontal)
	if got != 0 {
		t.Fatal("caller mutation changed indexed reference")
	}
	if _, err := newSurfaceReferenceIndex(make([]Surface, 3001)); err == nil {
		t.Fatal("accepted oversized reference index")
	}
}
func BenchmarkReferenceLookup(b *testing.B) {
	points := referenceTestPoints(3000, false)
	index, err := newSurfaceReferenceIndex(points)
	if err != nil {
		b.Fatal(err)
	}
	old := legacyReference(points)
	for _, modern := range []bool{false, true} {
		name := "Previous27Probes"
		if modern {
			name = "Indexed"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p := points[i%len(points)]
				p.Position.X += .025
				var distance float64
				if modern {
					distance, _ = index.distance(p.Position, p.Direction, p.Horizontal)
				} else {
					distance, _ = old.distance(p.Position, p.Direction, p.Horizontal)
				}
				if distance > .0625 {
					b.Fatal(distance)
				}
			}
		})
	}
}
func BenchmarkReferenceIndexBuild(b *testing.B) {
	for _, count := range []int{285, 3000} {
		for _, sparse := range []bool{false, true} {
			name := fmt.Sprintf("Dense%d", count)
			if sparse {
				name = fmt.Sprintf("Sparse%d", count)
			}
			points := referenceTestPoints(count, sparse)
			b.Run(name, func(b *testing.B) {
				for _, modern := range []bool{false, true} {
					version := "Previous"
					if modern {
						version = "Indexed"
					}
					b.Run(version, func(b *testing.B) {
						b.ReportAllocs()
						for i := 0; i < b.N; i++ {
							if modern {
								if _, err := newSurfaceReferenceIndex(points); err != nil {
									b.Fatal(err)
								}
							} else {
								_ = legacyReference(points)
							}
						}
					})
				}
			})
		}
	}
}
func TestReferenceIndexConcurrentQueries(t *testing.T) {
	points := referenceTestPoints(285, false)
	index, err := newSurfaceReferenceIndex(points)
	if err != nil {
		t.Fatal(err)
	}
	old := legacyReference(points)
	var work sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		work.Add(1)
		go func() {
			defer work.Done()
			for i := 0; i < 1000; i++ {
				p := points[i%len(points)]
				p.Position.X -= .025
				want, stable := old.distance(p.Position, p.Direction, p.Horizontal)
				got, gotStable := index.distance(p.Position, p.Direction, p.Horizontal)
				if got != want || stable != gotStable {
					t.Error("concurrent reference query changed result")
					return
				}
			}
		}()
	}
	work.Wait()
}
