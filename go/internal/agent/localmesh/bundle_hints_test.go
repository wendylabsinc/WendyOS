package localmesh

import (
	"fmt"
	"testing"
	"time"
)

func TestBundleHintsPeerScopeExpiryAndBounds(t *testing.T) {
	now := time.Unix(1800000000, 0)
	h := newBundleHintCache()
	chain := [][]byte{[]byte("device-0")}
	h.Written(445, chain, now)
	fp := Fingerprint(chain)
	if got := h.Known(445, now.Add(time.Second)); len(got) != 1 || got[0] != fp {
		t.Fatalf("peer hint=%v", got)
	}
	if got := h.Known(446, now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("hint leaked to another peer: %v", got)
	}
	if got := h.Known(445, now.Add(directoryBundleHintLifetime)); len(got) != 0 {
		t.Fatalf("expired hint=%v", got)
	}
	h.Written(445, chain, now)
	if got := h.Known(445, now.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("future hint=%v", got)
	}
	for i := 0; i < maxDirectoryHintsPerPeer+1; i++ {
		h.Written(445, [][]byte{[]byte(fmt.Sprintf("identity-%d", i))}, now.Add(time.Duration(i)*time.Millisecond))
	}
	if got := len(h.Known(445, now.Add(time.Minute))); got != maxDirectoryHintsPerPeer {
		t.Fatalf("peer hint count=%d", got)
	}
	if got := h.Known(445, now.Add(time.Minute)); containsHint(got, Fingerprint([][]byte{[]byte("identity-0")})) {
		t.Fatal("oldest identity was not evicted")
	}
	for i := int32(1); i <= maxDirectoryHintPeers+1; i++ {
		h.Written(i, chain, now.Add(time.Second+time.Duration(i)*time.Millisecond))
	}
	if got := len(h.peers); got != maxDirectoryHintPeers {
		t.Fatalf("peer cache count=%d", got)
	}
	if got := h.Known(1, now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("oldest peer not evicted: %v", got)
	}
}

func containsHint(fps []string, needle string) bool {
	for _, fp := range fps {
		if fp == needle {
			return true
		}
	}
	return false
}
