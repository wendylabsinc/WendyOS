package scenario

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func find(t *testing.T, values []world.Component, kind string) world.Component {
	t.Helper()
	for _, c := range values {
		if c.Key.Type == kind && c.Key.Entity != "person-short-lived" {
			return c
		}
	}
	t.Fatalf("missing %s component", kind)
	return world.Component{}
}

func TestPartitionSeparatesReplicasAndReconciliationKeepsLatest(t *testing.T) {
	r, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Steps) != 4 {
		t.Fatalf("got %d steps", len(r.Steps))
	}
	initial, partition, loss, reconnected := r.Steps[0], r.Steps[1], r.Steps[2], r.Steps[3]
	for _, node := range []string{"robot", "gateway", "operator"} {
		if len(initial.Replicas[node]) != 3 {
			t.Fatalf("%s did not receive initial world", node)
		}
	}
	if partition.Connected || loss.Connected || !reconnected.Connected {
		t.Fatal("partition timeline incorrect")
	}
	for _, kind := range []string{"wendy.pose.v0", "wendy.gas.v0"} {
		local := find(t, partition.Replicas["robot"], kind)
		if local.Version.Sequence != 2 {
			t.Fatal("local sensing did not progress")
		}
		for _, node := range []string{"gateway", "operator"} {
			remote := find(t, partition.Replicas[node], kind)
			if remote.Version.Sequence != 1 || bytes.Equal(remote.Payload, local.Payload) {
				t.Fatalf("%s received isolated robot update", node)
			}
		}
		for _, node := range []string{"robot", "gateway", "operator"} {
			got := find(t, reconnected.Replicas[node], kind)
			if got.Version.Sequence != 2 || !bytes.Equal(got.Payload, local.Payload) {
				t.Fatalf("%s failed to converge despite delayed old delivery", node)
			}
		}
	}
	for _, s := range []Step{loss, reconnected} {
		for node, values := range s.Replicas {
			for _, c := range values {
				if c.Key.Entity == "person-short-lived" {
					t.Fatalf("expired track visible on %s at %s", node, s.Name)
				}
			}
		}
	}
}

func TestAutonomyDependsOnLocalRequirementsAndHoldDoesNotResume(t *testing.T) {
	r, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if r.Steps[0].Action != mission.Continue || r.Steps[1].Action != mission.Continue {
		t.Fatal("autonomous mission failed before local condition loss")
	}
	for _, s := range r.Steps[2:] {
		if s.Action != mission.Hold || s.Phase != mission.Held {
			t.Fatalf("unsafe action at %s: %s", s.Name, s.Action)
		}
	}
	if r.Steps[2].Reason != "required sensor unavailable: gas" {
		t.Fatalf("unexpected fallback cause: %s", r.Steps[2].Reason)
	}
	if len(r.Checks) != 6 {
		t.Fatal("missing scenario check")
	}
	for name, passed := range r.Checks {
		if !passed {
			t.Errorf("failed check %s", name)
		}
	}
}

func TestScenarioIsRepeatableAndKeepsSyntheticProvenance(t *testing.T) {
	a, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same fixture produced different report")
	}
	for _, s := range a.Steps {
		for _, values := range s.Replicas {
			for _, c := range values {
				if !c.Provenance.Synthetic() || c.Provenance.Simulation == nil || c.Provenance.Simulation.RunID != "partition-demo" || c.Timing.ClockDomain != a.ClockDomain {
					t.Fatal("synthetic ancestry or clock domain lost")
				}
				if c.Key.Type == "wendy.gas.v0" {
					var observation world.Observation
					if err := json.Unmarshal(c.Payload, &observation); err != nil {
						t.Fatal(err)
					}
					if err := observation.Validate(); err != nil {
						t.Fatal(err)
					}
					if observation.Scalar.Unit != "ppm" {
						t.Fatal("gas units lost")
					}
				}
			}
		}
	}
}
