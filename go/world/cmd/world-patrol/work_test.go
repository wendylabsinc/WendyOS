package main

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkPolicyInSignedPayloadAndTamperRejected(t *testing.T) {
	policy := &patrolwork.Policy{LimitJ: 5000, ReturnReserveJ: 3000}
	store, _, auth, spec, err := fixture(t.TempDir(), "work-test", world.FrameRef{ID: "office", Revision: "1"}, []patrol.Room{{ID: "lounge"}}, 3600, scheduleDocument{Work: policy, Return: &tasking.PatrolReturn{Position: []float64{-6, .3, 0}}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, _ := store.Get(spec.ID)
	var proof authority.SignedInstruction
	_ = json.Unmarshal(record.Proof, &proof)
	var bound tasking.Spec
	_ = json.Unmarshal(proof.Payload, &bound)
	if bound.Patrol.Work == nil || *bound.Patrol.Work != *policy {
		t.Fatal("work allowance absent from signed instruction")
	}
	for _, changed := range []*patrolwork.Policy{{LimitJ: 6000, ReturnReserveJ: 3000}, {LimitJ: 5000, ReturnReserveJ: 0}, nil} {
		copy := spec
		patrolCopy := *spec.Patrol
		copy.Patrol = &patrolCopy
		copy.Patrol.Work = changed
		if _, err = store.Accept(copy, auth); err == nil {
			t.Fatal("work policy tamper accepted", changed)
		}
	}
}
func TestWorkConfigRefusesOmittedOrNullNumbersAndReserveWithoutReturn(t *testing.T) {
	for _, fragment := range []string{`{}`, `{"limit_j":null,"return_reserve_j":0}`, `{"limit_j":5000,"return_reserve_j":null}`} {
		path := filepath.Join(t.TempDir(), "policy.json")
		_ = os.WriteFile(path, []byte(`{"revisit_seconds":{"lounge":30},"work":`+fragment+`}`), 0600)
		if _, err := readPatrolConfig(path, []patrol.Room{{ID: "lounge"}}); err == nil {
			t.Fatal(fragment)
		}
	}
	store, _, _, _, err := fixture(t.TempDir(), "work-invalid", world.FrameRef{ID: "office", Revision: "1"}, []patrol.Room{{ID: "lounge"}}, 3600, scheduleDocument{Work: &patrolwork.Policy{LimitJ: 100, ReturnReserveJ: 30}})
	if store != nil {
		store.Close()
	}
	if err == nil {
		t.Fatal("reserve signed without a return")
	}
}
