//go:build darwin

package bleprovider

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestDarwinKnownRadioIdentityRequiresFreshMatchingAdvertisement(t *testing.T) {
	now := time.Now()
	adv, _ := NewAdvertisement(peerDevice, "test", DefaultPSM)
	var cache darwinPeerCache
	if cache.known("radio", adv, now) != (meshid.Device{}) {
		t.Fatal("unverified radio has an identity")
	}
	cache.remember("radio", adv, peerFixture, now)
	if cache.known("radio", adv, now) != peerFixture {
		t.Fatal("verified radio identity missing")
	}
	if cache.known("radio", adv, now.Add(time.Minute)) != (meshid.Device{}) {
		t.Fatal("stale radio identity retained")
	}
	adv.PSM++
	if cache.known("radio", adv, now) != (meshid.Device{}) {
		t.Fatal("changed advertisement retained identity")
	}
}

func TestDarwinStandbyYieldsOnlyToRunnableNewPeer(t *testing.T) {
	for _, mode := range []string{"new-peer", "known-upgrade", "failed-candidate", "no-working-alternate"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			adv, _ := NewAdvertisement(peerDevice, "test", DefaultPSM)
			view := localmesh.NodeSnapshot{Links: []localmesh.PeerLink{
				{ID: 1, Device: peerFixture, Cost: LinkCost},
				{ID: 2, Device: peerFixture, Cost: 64, ForwardingReady: true},
			}}
			policy := newDarwinConnectionPolicy(selfFixture, func() localmesh.NodeSnapshot { return view })
			var cache darwinPeerCache
			var candidates localmesh.CandidateQueue
			old := cache.reserve(policy, &candidates, "old", adv, now)
			if old == nil || !old.Ready(peerFixture, func() { t.Fatal("capacity used physical closer") }) {
				t.Fatal("standby not admitted")
			}
			defer old.Release()
			signals := 0
			localmesh.BindConnectionLink(localmesh.WithConnectionLease(context.Background(), old), 1, LinkCost, func() { signals++ })
			// Fill the second slot with a unique carrier, which must not be retired.
			other := cache.reserve(policy, &candidates, "other", adv, now)
			if other == nil || !other.Ready(lowFixture, func() { t.Fatal("unique carrier closed") }) {
				t.Fatal("second slot not admitted")
			}
			defer other.Release()
			if mode == "known-upgrade" {
				cache.remember("next", adv, peerFixture, now)
			}
			if mode == "failed-candidate" {
				candidates.Failed("next", now)
			}
			if mode == "no-working-alternate" {
				view.Links[1].ForwardingReady = false
			}
			if cache.reserve(policy, &candidates, "next", adv, now) != nil {
				t.Fatal("slot allocated before owner released")
			}
			want := mode == "new-peer"
			if old.Retired() != want || (signals == 1) != want {
				t.Fatalf("retired=%v signals=%d", old.Retired(), signals)
			}
			if !want {
				return
			}
			old.Release()
			view.Links = view.Links[1:]
			next := cache.reserve(policy, &candidates, "next", adv, now)
			if next == nil {
				t.Fatal("released standby slot not available to new peer")
			}
			next.Release()
		})
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

func TestDarwinNewPeerGetsSetupSlotBeforeKnownStandby(t *testing.T) {
	now := time.Now()
	adv, _ := NewAdvertisement(peerDevice, "test", DefaultPSM)
	advertisements := map[string]Advertisement{"standby": adv, "new": adv}
	view := localmesh.NodeSnapshot{Links: []localmesh.PeerLink{{ID: 1, Device: peerFixture, Cost: 64, ForwardingReady: true}}}
	var cache darwinPeerCache
	cache.remember("standby", adv, peerFixture, now)
	var candidates localmesh.CandidateQueue
	// Repeated planning must not let shuffled discovery order give the one
	// allowed setup attempt to a redundant peer ahead of an unconnected peer.
	for range 20 {
		policy := newDarwinConnectionPolicy(selfFixture, func() localmesh.NodeSnapshot { return view })
		keys := cache.order(&candidates, []string{"standby", "new"}, advertisements, now, view)
		if keys[0] != "new" {
			t.Fatalf("new peer was not prioritized: %v", keys)
		}
		lease := cache.reserve(policy, &candidates, keys[0], advertisements[keys[0]], now)
		if lease == nil {
			t.Fatal("new peer did not receive setup slot")
		}
		if other := cache.reserve(policy, &candidates, keys[1], advertisements[keys[1]], now); other != nil {
			other.Release()
			t.Fatal("second attempt exceeded native setup budget")
		}
		lease.Release()
	}
	// A failing new candidate must still respect the common retry penalty.
	candidates.Failed("new", now)
	if keys := cache.order(&candidates, []string{"new", "standby"}, advertisements, now, view); keys[0] != "standby" {
		t.Fatalf("failed candidate bypassed cooldown: %v", keys)
	}
}
