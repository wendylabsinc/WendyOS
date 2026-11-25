package main

import (
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
)

func TestFixtureAdmitsOneSignedSimulationMission(t *testing.T) {
	rooms := []patrol.Room{{ID: "office", Polygon: [][2]float64{{0, 0}, {1, 0}, {1, 1}}}}
	store, _, _, spec, err := fixture(t.TempDir(), "run", world.FrameRef{ID: "office-floor", Revision: "r1"}, rooms, 3600)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].Spec.ID != spec.ID || records[0].Status.Phase != mission.Accepted {
		t.Fatal(records, err)
	}
}
