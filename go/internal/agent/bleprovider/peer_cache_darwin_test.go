//go:build darwin

package bleprovider

import (
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestDarwinKnownRadioAllowsImmediateLANFailover(t *testing.T) {
	now := time.Now()
	adv, _ := NewAdvertisement(peerDevice, "test", DefaultPSM)
	view := localmesh.NodeSnapshot{Links: []localmesh.PeerLink{{Device: peerFixture, Cost: 64}}}
	selection := localmesh.NewPeerSelection(func() localmesh.NodeSnapshot { return view })
	var cache darwinPeerCache
	if cache.suppressed("radio", adv, selection, now) {
		t.Fatal("unverified radio skipped authentication")
	}
	cache.remember("radio", adv, peerFixture, now)
	if !cache.suppressed("radio", adv, selection, now) {
		t.Fatal("healthy LAN peer dialed BLE again")
	}
	view.Links = nil
	if cache.suppressed("radio", adv, selection, now) {
		t.Fatal("LAN loss did not enable immediate BLE fallback")
	}
	view.Links = []localmesh.PeerLink{{Device: selfFixture, Cost: 64}}
	if cache.suppressed("radio", adv, selection, now) {
		t.Fatal("another peer's LAN path suppressed fallback")
	}
	view.Links = []localmesh.PeerLink{{Device: peerFixture, Cost: 64}}
	if cache.suppressed("radio", adv, selection, now.Add(time.Minute)) {
		t.Fatal("stale radio identity did not reauthenticate")
	}
	adv.PSM++
	if cache.suppressed("radio", adv, selection, now) {
		t.Fatal("changed advertisement did not reauthenticate")
	}
}

func TestDarwinKnownRadiosStayBounded(t *testing.T) {
	now := time.Now()
	adv, _ := NewAdvertisement(peerDevice, "test", DefaultPSM)
	var cache darwinPeerCache
	for i := range 300 {
		cache.remember(fmt.Sprint(i), adv, peerFixture, now)
	}
	if len(cache.peers) != 256 {
		t.Fatalf("cache size=%d", len(cache.peers))
	}
	cache.remember("fresh", adv, peerFixture, now.Add(time.Minute))
	if len(cache.peers) != 1 {
		t.Fatalf("expired entries retained: %d", len(cache.peers))
	}
}
