package localmesh

import (
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestBundleHintsPeerScopeExpiryAndBounds(t *testing.T) {
	now := time.Unix(1800000000, 0)
	h := newBundleHintCache()
	peer := testDevice(t, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	other := testDevice(t, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/33333333-3333-4333-8333-333333333333")
	chain := [][]byte{[]byte("device-0")}
	h.Written(peer, chain, now)
	fp := Fingerprint(chain)
	if got := h.Known(peer, now.Add(time.Second)); len(got) != 1 || got[0] != fp {
		t.Fatalf("peer hint=%v", got)
	}
	if got := h.Known(other, now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("hint leaked to another peer: %v", got)
	}
	if got := h.Known(peer, now.Add(directoryBundleHintLifetime)); len(got) != 0 {
		t.Fatalf("expired hint=%v", got)
	}
	h.Written(peer, chain, now)
	if got := h.Known(peer, now.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("future hint=%v", got)
	}
	for i := range maxDirectoryHintsPerPeer + 1 {
		h.Written(peer, [][]byte{[]byte(fmt.Sprintf("identity-%d", i))}, now.Add(time.Duration(i)*time.Millisecond))
	}
	if got := len(h.Known(peer, now.Add(time.Minute))); got != maxDirectoryHintsPerPeer {
		t.Fatalf("peer hint count=%d", got)
	}
	if got := h.Known(peer, now.Add(time.Minute)); containsHint(got, Fingerprint([][]byte{[]byte("identity-0")})) {
		t.Fatal("oldest identity was not evicted")
	}
	peers := make([]meshid.Device, 0, maxDirectoryHintPeers+2)
	for i := range maxDirectoryHintPeers + 2 {
		peers = append(peers, testDevice(t, fmt.Sprintf("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/%08x-%04x-4000-8000-%012x", i, i, i)))
	}
	for i, device := range peers {
		h.Written(device, chain, now.Add(time.Second+time.Duration(i)*time.Millisecond))
	}
	if got := len(h.peers); got != maxDirectoryHintPeers {
		t.Fatalf("peer cache count=%d", got)
	}
	if got := h.Known(peers[0], now.Add(time.Minute)); len(got) != 0 {
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
