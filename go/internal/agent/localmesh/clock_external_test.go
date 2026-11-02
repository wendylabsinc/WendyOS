package localmesh

import (
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

type clockCloseProbe struct {
	futureDatagramLink
	closes int
}

func (p *clockCloseProbe) Close() error { p.closes++; return nil }

// The hold deadline bounds retention, not a second admission rule. If another
// source has corrected UTC as maintenance runs, normal Accept must get its turn
// before deciding whether an unresolved clock problem still closes the link.
func TestClockExternalCorrectionRetriesBeforeClosingExpiredHold(t *testing.T) {
	for _, mode := range []string{"uncorrected", "corrected", "expired-signed-lease"} {
		t.Run(mode, func(t *testing.T) {
			d, chain, key, device, now := directoryFixture(t)
			behind := now.Add(-598 * time.Second)
			resetClockFixtureCache(t, d, chain, behind)
			w, err := SignManifest(routeManifest(device, now), chain, key, now)
			if err != nil {
				t.Fatal(err)
			}
			s := NewSynchronizer(d, d.cache)
			defer s.Close()
			if _, _, err = s.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, behind); !errors.Is(err, ErrManifestClockSkew) {
				t.Fatal(err)
			}
			deadline := time.Now().Add(-time.Second)
			s.pendingClock[w.ownerKey()] = deadline
			s.clockRecoveryUntil = deadline
			link := &clockCloseProbe{}
			p := &nodePeer{id: 1, device: device, sync: s, link: link, control: make(chan ControlMessage, 128), clockWaitingUntil: deadline}
			credentials, _ := testCredentials(t)
			routing, err := NewRouting(babel.Config{RouterID: 2}, nil, &routingRecorder{})
			if err != nil {
				t.Fatal(err)
			}
			utc := behind
			if mode == "corrected" {
				utc = now
			}
			if mode == "expired-signed-lease" {
				utc = now.Add(2 * MaxLease)
			}
			n := &Node{directory: d, cache: d.cache, Credentials: credentials, routing: routing, started: time.Now(), peers: map[babel.LinkID]*nodePeer{1: p}, wallNow: func() time.Time { return utc }}
			if err = n.maintainClockPeer(p); err != nil {
				t.Fatal(err)
			}
			if mode == "uncorrected" {
				if link.closes != 1 || len(d.history) != 0 || len(d.Snapshot(utc)) != 0 {
					t.Fatal("unresolved clock escaped bounded hold or gained authority")
				}
			} else {
				if link.closes != 0 || !p.clockWaitingUntil.IsZero() || len(s.pendingClock) != 0 || len(s.pending) != 0 {
					t.Fatal("obsolete hold closed corrected carrier or retained bodies")
				}
				want := 0
				if mode == "corrected" {
					want = 1
				}
				if len(d.history) != want || len(d.Snapshot(utc)) != want {
					t.Fatal("ordinary freshness/replay admission changed")
				}
			}
		})
	}
}
