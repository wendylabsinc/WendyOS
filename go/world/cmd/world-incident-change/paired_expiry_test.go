package main

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
	"time"
)

func TestPairExpiresBeforeCarrierWithoutDowngradingVerification(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	members := make([][]byte, 2)
	for i, ttl := range []time.Duration{time.Second, 2 * time.Second} {
		record := authority.SignedRecord{Component: world.Component{Timing: world.Timing{SampleTime: now}, TTL: ttl}}
		members[i], _ = json.Marshal(record)
	}
	calls := 0
	verify := func(records []authority.SignedRecord) error {
		calls++
		if len(records) != 2 {
			t.Fatal("incomplete member verification")
		}
		return nil
	}
	if err := pairMembersCurrent(members, now.Add(time.Second-time.Nanosecond), verify); err != nil {
		t.Fatal(err)
	}
	if err := pairMembersCurrent(members, now.Add(time.Second), verify); !errors.Is(err, errOriginalExpired) {
		t.Fatalf("oldest member deadline ignored: %v", err)
	}
	if calls != 2 {
		t.Fatalf("verification skipped: %d", calls)
	}
	denied := errors.New("member signature or grant denied")
	if err := pairMembersCurrent(members, now.Add(3*time.Second), func([]authority.SignedRecord) error { return denied }); !errors.Is(err, denied) || errors.Is(err, errOriginalExpired) {
		t.Fatalf("verification error downgraded: %v", err)
	}
	malformed := append([][]byte(nil), members...)
	malformed[0] = []byte(`{"unexpected":true}`)
	if err := pairMembersCurrent(malformed, now.Add(3*time.Second), verify); err == nil || errors.Is(err, errOriginalExpired) {
		t.Fatalf("wire schema error downgraded: %v", err)
	}
	if err := pairMembersCurrent(members, now, nil); err == nil {
		t.Fatal("missing verifier allowed")
	}
}
