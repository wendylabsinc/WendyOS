package twinrobot

import (
	"math"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func cacheFixture(t testing.TB) ([]sitenav.Triangle, []sitenav.Triangle) {
	t.Helper()
	request, _ := passiveLocalizationFixture(false)
	a, err := fusionTriangles(request.Architecture)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fusionTriangles(request.Furniture)
	if err != nil {
		t.Fatal(err)
	}
	return a, f
}

func TestLayeredGeometryCacheTracksMeshContents(t *testing.T) {
	a, f := cacheFixture(t)
	var cache layeredGeometryCache
	original, err := cache.load(a, f)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := cache.load(append([]sitenav.Triangle(nil), a...), f)
	if err != nil || &original.Walls[0] != &reused.Walls[0] {
		t.Fatal("identical geometry rebuilt", err)
	}
	moved := append([]sitenav.Triangle(nil), a...)
	for i := range moved {
		for j := range moved[i] {
			moved[i][j].Y += 1
		}
	}
	changed, err := cache.load(moved, f)
	if err != nil || changed.Base.FloorY != original.Base.FloorY+1 || &changed.Walls[0] == &original.Walls[0] {
		t.Fatal("geometry edit reused stale grid", err)
	}
	if original.Base.FloorY != 0 {
		t.Fatal("cache replacement mutated retained model")
	}
	moved[0][0].X = math.NaN()
	if _, err := cache.load(moved, f); err == nil {
		t.Fatal("invalid geometry bypassed validation")
	}
	if geometryDigest(a, f) == geometryDigest(nil, append(append([]sitenav.Triangle(nil), a...), f...)) {
		t.Fatal("architecture/furniture boundary missing from key")
	}
}

func TestLayeredGeometryCacheConcurrentSameMesh(t *testing.T) {
	a, f := cacheFixture(t)
	var cache layeredGeometryCache
	var workers sync.WaitGroup
	results := make([]sitenav.LayeredRoom, 8)
	for i := range results {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			model, err := cache.load(a, f)
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = model
		}(i)
	}
	workers.Wait()
	for i := range results {
		if len(results[i].Walls) == 0 || &results[i].Walls[0] != &results[0].Walls[0] {
			t.Fatal("parallel requests rebuilt immutable geometry")
		}
	}
}

func BenchmarkLayeredGeometryCache(b *testing.B) {
	a, f := cacheFixture(b)
	for _, cached := range []bool{false, true} {
		name := "Cold"
		if cached {
			name = "Repeated"
		}
		b.Run(name, func(b *testing.B) {
			var cache layeredGeometryCache
			if _, err := cache.load(a, f); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !cached {
					cache.model = nil
				}
				if _, err := cache.load(a, f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
