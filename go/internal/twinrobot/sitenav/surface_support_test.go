package sitenav

import (
	"math"
	"reflect"
	"testing"
)

func TestExtractSurfacesRetainsMeasuredWallSupport(t *testing.T) {
	for _, angle := range []float64{0, .37, 1.1} {
		s, c := math.Sincos(angle)
		var points []Vertex
		// A partially visible wall has enough measured length to establish a
		// direction, even though only its middle has a full local neighborhood.
		for i := 0; i <= 16; i++ {
			for _, z := range []float64{.15, .20, .25} {
				x := float64(i) * .05
				points = append(points, Vertex{2 + c*x, -1 + s*x, z})
			}
		}
		cloud := ExtractSurfaces(points, 0)
		var support []Point
		seen := map[Cell]bool{}
		for _, surface := range cloud.Surfaces {
			if surface.Horizontal {
				continue
			}
			p := Point{surface.Position.X, surface.Position.Y}
			if !seen[cell(p)] {
				support = append(support, p)
				seen[cell(p)] = true
			}
		}
		if _, ok := planarWallDirection(support); !ok {
			t.Fatalf("wall support was lost at angle %v: %v", angle, support)
		}
		// Reordering and duplicating returns must not change the retained wall.
		reversed := append([]Vertex(nil), points...)
		for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
			reversed[i], reversed[j] = reversed[j], reversed[i]
		}
		got := ExtractSurfaces(append(reversed, points...), 0)
		if !reflect.DeepEqual(cloud.Surfaces, got.Surfaces) {
			t.Fatal("return ordering or repetition changed surface evidence")
		}
	}
}

func TestExtractSurfacesDoesNotGrowEvidenceIntoOccupiedVolume(t *testing.T) {
	var points []Vertex
	for x := 0; x < 20; x++ {
		for y := 0; y < 16; y++ {
			for _, z := range []float64{.15, .20, .25} {
				points = append(points, Vertex{float64(x) * .05, float64(y) * .05, z})
			}
		}
	}
	for _, surface := range ExtractSurfaces(points, 0).Surfaces {
		if !surface.Horizontal {
			t.Fatal("a filled volume supplied wall direction evidence")
		}
	}
}
