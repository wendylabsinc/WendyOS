package sitenav

import (
	"reflect"
	"testing"
)

func TestHazardFootprintsMatchCollisionGeometry(t *testing.T) {
	g := navigationGrid([]Vertex{{.1, .1, .2}, {.1, .1, .3}, {1, 1, 2}, {2, 2, 0}}, 0)
	g.Obstacles[Cell{-2, -3}] = true
	hazards := g.HazardFootprints()
	if len(hazards) != 2 {
		t.Fatalf("expected one deduplicated voxel and one full cell, got %+v", hazards)
	}
	if hazards[0].Position.Distance(Point{-.15, -.25}) > 1e-9 || !hazards[0].Drop || hazards[0].Size != CellSize {
		t.Fatalf("drop footprint: %+v", hazards[0])
	}
	if hazards[1] != (HazardFootprint{Point{.1, .1}, .05, false}) {
		t.Fatalf("voxel was inflated, lost or duplicated: %+v", hazards[1])
	}
	for i := 0; i < 10; i++ {
		if !reflect.DeepEqual(hazards, g.WithUnobservedFloor(Point{}).HazardFootprints()) {
			t.Fatal("ordering or supervised policy changed measured hazards")
		}
	}
}
