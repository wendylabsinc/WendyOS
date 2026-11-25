//go:build linux

package bleprovider

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

func TestBLEProviderReclaimsOnlyRunnableOwnedStandby(t *testing.T) {
	for _, blocked := range []string{"none", "pacing", "retry", "failure-penalty", "unready-alternate", "retiring-alternate"} {
		t.Run(blocked, func(t *testing.T) {
			peer, next := numberedBLEDevice(10), numberedBLEDevice(11)
			node := &countSnapshotNode{links: []localmesh.PeerLink{{ID: 1, Device: peer, Cost: LinkCost}, {ID: 2, Device: peer, Cost: 128, ForwardingReady: true}}}
			p := localmesh.NewConnectionPolicy(selfFixture, false, localmesh.ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 2})
			r := &runtime{cfg: Config{Credentials: &localmesh.Credentials{Device: selfFixture}, Node: node, Policy: p, Logger: zap.NewNop()}}
			p = r.connectionPolicy()
			old := p.Reserve("old", peer, true)
			if old == nil || !old.Ready(peer, func() { t.Fatal("reclamation called blocking transport Close") }) {
				t.Fatal("could not own original BLE")
			}
			defer old.Release()
			var signals atomic.Int32
			localmesh.BindConnectionLink(localmesh.WithConnectionLease(context.Background(), old), 1, LinkCost, func() { signals.Add(1) })
			c := candidate{address: "00:11:22:33:44:55", addressType: "public", psm: DefaultPSM, hint: deviceHint(next.UUID())}
			r.known = map[string]meshid.Device{c.key(): next}
			switch blocked {
			case "pacing":
				r.nextDial = time.Now().Add(globalDialPace)
			case "retry":
				r.radioRetry = map[string]time.Time{c.key(): time.Now().Add(time.Second)}
			case "failure-penalty":
				r.candidateQueue().Failed(c.key(), time.Now())
			case "unready-alternate":
				node.links[1].ForwardingReady = false
			case "retiring-alternate":
				node.links[1].Retiring = true
			}
			if got := r.reserveCandidate(c); got != nil {
				t.Fatal("standby reclamation borrowed an extra slot")
			}
			want := int32(0)
			if blocked == "none" {
				want = 1
			}
			if signals.Load() != want || old.Retired() != (want == 1) {
				t.Fatalf("retirement=%d retired=%v blocked=%s", signals.Load(), old.Retired(), blocked)
			}
			if blocked == "none" {
				old.Release()
				if got := r.reserveCandidate(c); got == nil {
					t.Fatal("new peer could not use released BLE slot")
				} else {
					got.Release()
				}
			}
		})
	}
}
