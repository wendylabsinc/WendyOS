package main

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"reflect"
	"testing"
)

func TestPublishedOriginalSnapshotReplacementDoesNotRetainOmittedFields(t *testing.T) {
	simulation := &world.SimulationContext{RunID: "run", ClockDomain: "simulation:run", ScenarioVersion: "fixture", Seed: 1}
	raw := world.Component{Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "sensor", Simulation: simulation}, Payload: json.RawMessage(`{"original":1}`)}
	pair := world.Component{Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "pair", ModelID: "intact", ModelVersion: "1", Inputs: []world.Input{{ID: "original", Origin: world.Simulated, Simulation: simulation}}}, Payload: json.RawMessage(`{"pair":2}`)}
	r := new(runtime)
	if err := r.retainPublishedRecords([]world.Component{raw}); err != nil {
		t.Fatal(err)
	}
	if err := r.retainPublishedRecords([]world.Component{pair}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.publishedRecords[0], pair) || gasreplica.ComponentHash(r.publishedRecords[0]) != gasreplica.ComponentHash(pair) {
		t.Fatal("replacement retained omitted sensor provenance in exact pair snapshot")
	}
	pair.Payload[0] = '!'
	simulation.RunID = "mutated"
	if string(r.publishedRecords[0].Payload) != `{"pair":2}` || r.publishedRecords[0].Provenance.Inputs[0].Simulation.RunID != "run" {
		t.Fatal("retained evidence aliases caller")
	}
	if err := r.retainPublishedRecords(nil); err != nil || r.publishedRecords != nil {
		t.Fatal("withdrawn original snapshot retained")
	}
}
