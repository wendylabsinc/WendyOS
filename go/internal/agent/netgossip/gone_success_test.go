package netgossip

import (
	"bytes"
	"testing"
	"time"
)

func TestGoneSwitchesSupplierWithoutWaiting(t *testing.T) {
	now := time.Now()
	data := []byte("ordinary verified service fixture bytes")
	source := NewPool().NewSession(Service)
	have := source.Offer(data, now.Add(time.Minute), now)
	pool := NewPool()
	first, other := pool.NewSession(Service), pool.NewSession(Service)
	want, _, err := first.Receive(*have, now)
	if err != nil || want == nil {
		t.Fatalf("first supplier: %v %v", want, err)
	}
	if reply, _, err := other.Receive(*have, now); err != nil || reply != nil {
		t.Fatalf("alternate hint: %v %v", reply, err)
	}
	if _, _, err := first.Receive(*Gone(have.ID()), now); err != nil {
		t.Fatal(err)
	}
	if first.Pending(now) || len(first.Poll(now)) != 0 {
		t.Fatal("withdrawn supplier still pending")
	}
	next := other.Poll(now)
	if len(next) != 1 || next[0].Op != "want" {
		t.Fatalf("immediate alternate claim: %v", next)
	}
	chunk, _, err := source.Receive(*next[0], now)
	if err != nil || chunk == nil {
		t.Fatalf("alternate delivery: %v %v", chunk, err)
	}
	_, received, err := other.Receive(*chunk, now)
	if err != nil || !bytes.Equal(received, data) {
		t.Fatalf("delivered bytes: %q %v", received, err)
	}
	other.Resolve(received, now.Add(time.Minute), now, true)
	// A supplier's cache eviction does not revoke our independently held bytes.
	if _, _, err := other.Receive(*Gone(have.ID()), now); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(other.Lookup(have.ID(), now), data) {
		t.Fatal("local object lost on remote cache eviction")
	}
	first.Close()
	other.Close()
	source.Close()
}
