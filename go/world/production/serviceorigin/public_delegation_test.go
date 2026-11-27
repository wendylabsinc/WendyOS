package serviceorigin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestPublicDelegationOwnsOriginalBytesWithoutClockOrSigning(t *testing.T) {
	e := setup(t)
	e.clock.mu.Lock()
	before := e.clock.calls
	e.clock.mu.Unlock()
	a, err := e.publisher.PublicDelegation()
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.publisher.PublicDelegation()
	if err != nil {
		t.Fatal(err)
	}
	e.clock.mu.Lock()
	after := e.clock.calls
	e.clock.mu.Unlock()
	if before != after {
		t.Fatal("metadata accessor acquired time")
	}
	for _, part := range [][]byte{a.Claims, a.Signature, a.ChainDER[0]} {
		part[0] ^= 1
	}
	c, err := e.publisher.PublicDelegation()
	if err != nil {
		t.Fatal(err)
	}
	bRaw, _ := json.Marshal(b)
	cRaw, _ := json.Marshal(c)
	if !bytes.Equal(bRaw, cRaw) {
		t.Fatal("caller mutated pinned delegation")
	}
	p, err := e.publisher.Publish(context.Background(), e.request)
	if err != nil {
		t.Fatal(err)
	}
	pRaw, _ := json.Marshal(p.Delegation)
	if !bytes.Equal(pRaw, bRaw) {
		t.Fatal("metadata differs from actual signed record delegation")
	}
	var record Record
	if json.Unmarshal(p.Record, &record) != nil || record.Sequence != 1 {
		t.Fatal("metadata accessor consumed a publication sequence")
	}
	e.publisher.Close()
	if _, err = e.publisher.PublicDelegation(); !errors.Is(err, ErrClosed) {
		t.Fatal("closed publisher exposed authority metadata", err)
	}
}
