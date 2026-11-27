package main

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRecallControlAuthentication(t *testing.T) {
	q := make(chan command, 1)
	interrupts := 0
	h := controlHandler("test-control", q, &controlFence{blocked: new(atomic.Bool)}, func() { interrupts++ })
	for _, test := range []struct{ auth, body string }{{"", `{"action":"recall"}`}, {"Bearer wrong", `{"action":"recall"}`}, {"Bearer test-control", `{"action":"recall","position":[1,2,3]}`}, {"Bearer test-control", `{"action":"recall"} {}`}} {
		r := httptest.NewRequest("POST", "/control", strings.NewReader(test.body))
		r.Header.Set("Authorization", test.auth)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code < 400 || len(q) != 0 || interrupts != 0 {
			t.Fatal("unauthorized or mutable recall reached executor")
		}
	}
	done := make(chan string, 1)
	go func() { c := <-q; done <- c.action; c.reply <- nil }()
	r := httptest.NewRequest("POST", "/control", strings.NewReader(`{"action":"recall"}`))
	r.Header.Set("Authorization", "Bearer test-control")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK || <-done != "recall" || interrupts != 0 {
		t.Fatal(w.Code)
	}
}
func TestRendezvousInSignedBytes(t *testing.T) {
	policy := &tasking.PatrolReturn{Position: []float64{-6, .3, 0}, Yaw: .25, BeforeExpirySeconds: 120}
	store, _, auth, spec, err := fixture(t.TempDir(), "return-test", world.FrameRef{ID: "office", Revision: "1"}, []patrol.Room{{ID: "lounge"}}, 3600, scheduleDocument{Return: policy})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, _ := store.Get(spec.ID)
	var instruction authority.SignedInstruction
	if err = json.Unmarshal(record.Proof, &instruction); err != nil {
		t.Fatal(err)
	}
	var bound tasking.Spec
	if err = json.Unmarshal(instruction.Payload, &bound); err != nil {
		t.Fatal(err)
	}
	if bound.Patrol.Return.Position[0] != -6 || bound.Patrol.Return.Yaw != .25 || bound.Patrol.Return.BeforeExpirySeconds != 120 {
		t.Fatal("rendezvous absent from signed bytes")
	}
	policy.Position[0] = 1
	if _, err = store.Accept(spec, auth); err == nil {
		t.Fatal("changed policy accepted")
	}
}

func TestRestrictedAreasAreBoundToSignedPatrol(t *testing.T) {
	frame := world.FrameRef{ID: "office", Revision: "1"}
	p := &tasking.PatrolExclusions{Frame: frame, Revision: 1, Rectangles: []tasking.PatrolRectangle{{ID: "area", MinMM: []int{0, 0}, MaxMM: []int{1000, 1000}}}}
	p.SHA256 = p.Digest()
	store, _, auth, spec, err := fixture(t.TempDir(), "restrictions-test", frame, []patrol.Room{{ID: "office"}}, 3600, scheduleDocument{Exclusions: p})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, _ := store.Get(spec.ID)
	var instruction authority.SignedInstruction
	if err = json.Unmarshal(record.Proof, &instruction); err != nil {
		t.Fatal(err)
	}
	var signed tasking.Spec
	if err = json.Unmarshal(instruction.Payload, &signed); err != nil {
		t.Fatal(err)
	}
	if signed.Patrol.Exclusions.SHA256 != p.SHA256 || len(signed.Patrol.Exclusions.Rectangles) != 1 {
		t.Fatal("restrictions missing from signed payload")
	}
	// Even a newly recomputed digest cannot authorize editing the signed geometry.
	p.Rectangles[0].MinMM[0] = -1000
	p.SHA256 = p.Digest()
	if _, err = store.Accept(spec, auth); err == nil {
		t.Fatal("changed signed restricted area accepted")
	}
	spec.Patrol.Exclusions = nil
	if _, err = store.Accept(spec, auth); err == nil {
		t.Fatal("removed signed restricted areas accepted")
	}
}
