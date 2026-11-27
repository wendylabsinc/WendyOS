package node

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestAnnotationLocalRecordOwnerMatchesRequestOwner(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	r := annotationRecord(t, f, annotationEpoch(t, f))
	// A root may have issued or renewed the same source/principal with a
	// different owning gateway. The local request cannot launder that record.
	g := f.credentials.Grant.Grant
	g.ID = "foreign-owner-grant"
	g.PublishAudience = "other-owner"
	foreign, err := authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	foreignRecord, err := authority.SignAnnotation(foreign, f.credentials.PrivateKey, r.Component)
	if err != nil {
		t.Fatal(err)
	}
	if status, response := postAnnotation(t, f, foreignRecord); status != http.StatusForbidden {
		t.Fatalf("local request laundered foreign record owner: %d %s", status, response)
	}
	if records, err := f.s.Records(); err != nil || len(records) != 0 {
		t.Fatalf("foreign local publication changed replica: %d records, %v", len(records), err)
	}
	// Replicas still admit origin-authorized records from another owner.
	if _, err = f.n.apply(foreignRecord, false); err != nil {
		t.Fatalf("relay rejected foreign origin: %v", err)
	}
	// Renewal can use a different grant ID while keeping the owning gateway.
	g.ID = "renewed-local-grant"
	g.PublishAudience = f.audience
	renewed, err := authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	renewedRecord, err := authority.SignAnnotation(renewed, f.credentials.PrivateKey, r.Component)
	if err != nil {
		t.Fatal(err)
	}
	if status, response := postAnnotation(t, f, renewedRecord); status != http.StatusOK {
		t.Fatalf("same-owner grant renewal rejected: %d %s", status, response)
	}
}

func TestAnnotationProofFailureRetainsRetryablePendingRecord(t *testing.T) {
	f := newFixture(t, nil, "owner")
	annotationOperator(t, f)
	r := annotationRecord(t, f, annotationEpoch(t, f))
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	// Fail the origin-proof rename before replica persistence. A client must
	// retain the exact pending bytes rather than treat this as a refusal.
	if err = os.Mkdir(f.cfg.ProofPath, 0700); err != nil {
		t.Fatal(err)
	}
	status, response := call(t, f, f.credentials, http.MethodPost, "/v1/annotations", "annotation-write", body)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("proof failure must remain retryable: %d %s", status, response)
	}
	records, err := f.s.Records()
	if err != nil || len(records) != 0 {
		t.Fatalf("uncommitted note entered replica: %d records, %v", len(records), err)
	}
	if err = os.Remove(f.cfg.ProofPath); err != nil {
		t.Fatal(err)
	}
	for _, want := range []world.Result{world.Applied, world.Duplicate} {
		// call signs a new transport nonce; the record body remains unchanged.
		status, response = call(t, f, f.credentials, http.MethodPost, "/v1/annotations", "annotation-write", body)
		if status != http.StatusOK {
			t.Fatalf("retry failed: %d %s", status, response)
		}
		var ack struct {
			Result    world.Result    `json:"result"`
			Component world.Component `json:"component"`
		}
		if err = json.Unmarshal(response, &ack); err != nil {
			t.Fatal(err)
		}
		wantDigest, _ := recordDigest(r.Component)
		gotDigest, _ := recordDigest(ack.Component)
		if ack.Result != want || gotDigest != wantDigest {
			t.Fatalf("retry acknowledged a different record or result: %s", ack.Result)
		}
	}
	if _, err = New(f.cfg, f.s, f.v); err != nil {
		t.Fatalf("acknowledged note has no durable origin proof: %v", err)
	}
}
