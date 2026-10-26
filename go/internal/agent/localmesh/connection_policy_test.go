package localmesh

import (
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestConnectionPolicyIndependentLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits ConnectionLimits
	}{
		{"BLE", ConnectionLimits{BLEMaxOutgoing, BLEMaxOutgoingAttempts, BLEMaxConnections}},
		{"NAN", ConnectionLimits{NANMaxOutgoing, NANMaxOutgoingAttempts, NANMaxConnections}},
		{"LAN", ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewConnectionPolicy(selectionDevice(1), true, tc.limits)
			var pending []*ConnectionLease
			for i := 0; i < tc.limits.Attempts; i++ {
				l := p.Reserve(fmt.Sprint(i), selectionDevice(i+10), true)
				if l == nil {
					t.Fatal("outgoing attempt denied too early")
				}
				pending = append(pending, l)
			}
			if p.Reserve("excess-attempt", selectionDevice(90), true) != nil {
				t.Fatal("parallel attempt budget exceeded")
			}
			for i, l := range pending {
				if !l.Ready(selectionDevice(i+10), func() {}) {
					t.Fatal("ready failed")
				}
			}
			for p.Counts().Outgoing < tc.limits.Outgoing {
				i := p.Counts().Outgoing + 20
				l := p.Reserve(fmt.Sprint(i), selectionDevice(i), true)
				if l == nil || !l.Ready(selectionDevice(i), func() {}) {
					t.Fatal("established connection consumed setup budget")
				}
			}
			if p.Reserve("excess-outgoing", selectionDevice(91), true) != nil {
				t.Fatal("outgoing connection budget exceeded")
			}
			for p.Counts().Total < tc.limits.Total {
				i := p.Counts().Total + 100
				l := p.Reserve(fmt.Sprint(i), selectionDevice(i), false)
				if l == nil || !l.Ready(selectionDevice(i), func() {}) {
					t.Fatal("outgoing target blocked incoming connection")
				}
			}
			if p.Reserve("excess-incoming", selectionDevice(999), false) != nil {
				t.Fatal("total connection budget exceeded")
			}
		})
	}
}

func TestConnectionPolicyConcurrentReservations(t *testing.T) {
	p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{2, 2, 4})
	var wg sync.WaitGroup
	var held sync.Map
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := p.Reserve(fmt.Sprint(i), selectionDevice(i+10), i%2 == 0)
			if l != nil {
				held.Store(l, true)
			}
		}()
	}
	wg.Wait()
	c := p.Counts()
	if c.Total > 4 || c.Outgoing > 2 || c.Attempts > 2 {
		t.Fatalf("oversubscribed: %+v", c)
	}
	held.Range(func(k, v any) bool { k.(*ConnectionLease).Release(); return true })
	if p.Counts() != (ConnectionCounts{}) {
		t.Fatal("reservation leaked")
	}
}

func TestCrossedConnectionsKeepSameWinnerAtBothEndpoints(t *testing.T) {
	for _, preferLower := range []bool{true, false} {
		for _, preferredFirst := range []bool{true, false} {
			for _, selfNumber := range []int{1, 2} {
				self, peer := selectionDevice(selfNumber), selectionDevice(3-selfNumber)
				p := NewConnectionPolicy(self, preferLower, ConnectionLimits{2, 2, 4})
				out := p.Reserve("out", peer, true)
				in := p.Reserve("in", peer, false)
				var outClosed, inClosed atomic.Int32
				preferred, reverse := out, in
				preferredClose := func() { outClosed.Add(1) }
				reverseClose := func() { inClosed.Add(1) }
				if !p.preferred(out) {
					preferred, reverse = in, out
					preferredClose, reverseClose = reverseClose, preferredClose
				}
				if preferredFirst {
					if !preferred.Ready(peer, preferredClose) {
						t.Fatal("preferred rejected")
					}
					if reverse.Ready(peer, reverseClose) {
						t.Fatal("reverse survived")
					}
				} else {
					if !reverse.Ready(peer, reverseClose) {
						t.Fatal("sole reverse connection rejected")
					}
					if outClosed.Load()+inClosed.Load() != 0 {
						t.Fatal("closed sole connection before replacement was stable")
					}
					if !preferred.Ready(peer, preferredClose) {
						t.Fatal("replacement rejected")
					}
				}
				if outClosed.Load()+inClosed.Load() != 1 || preferred.retired || !reverse.retired {
					t.Fatal("endpoints did not keep exactly the canonical connection")
				}
				if p.preferred(out) && outClosed.Load() != 0 || !p.preferred(out) && inClosed.Load() != 0 {
					t.Fatal("closed preferred connection")
				}
				out.Release()
				in.Release()
			}
		}
	}
}

func TestCandidateFailureMovesToTailAndHealthyCandidatesShuffle(t *testing.T) {
	now := time.Unix(100, 0)
	rng := rand.New(rand.NewPCG(1, 2))
	q := CandidateQueue{shuffle: rng.Shuffle}
	q.Failed("A", now)
	q.Failed("B", now.Add(time.Second))
	firsts := make(map[string]bool)
	for i := 0; i < 30; i++ {
		order := q.Order([]string{"A", "B", "C", "D"}, now.Add(2*time.Second))
		firsts[order[0]] = true
		if order[2] != "A" || order[3] != "B" {
			t.Fatalf("failed peers did not move to tail: %v", order)
		}
	}
	if !firsts["C"] || !firsts["D"] {
		t.Fatal("healthy candidates retain discovery-order bias")
	}
	q.Failed("A", now.Add(3*time.Second))
	if order := q.Order([]string{"A", "B"}, now.Add(4*time.Second)); order[0] != "B" {
		t.Fatal("repeated failure did not move to bottom")
	}
	q.Succeeded("A")
	if q.Order([]string{"B", "A"}, now.Add(5*time.Second))[0] != "A" {
		t.Fatal("successful candidate retained penalty")
	}
	q.Order(nil, now.Add(FailedConnectPenalty+5*time.Second))
	if len(q.failed) != 0 {
		t.Fatal("failure penalty never expires")
	}
}

func TestIncomingProgressSuppressesOnlyMatchingOutboundUntilIdle(t *testing.T) {
	for _, knownIdentity := range []bool{false, true} {
		p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{2, 2, 4})
		now := time.Unix(100, 0)
		p.now = func() time.Time { return now }
		peer := selectionDevice(2)
		in := p.Reserve("radio-address", meshid.Device{}, false)
		if knownIdentity {
			in.Identify(peer)
		}
		key := "radio-address"
		if knownIdentity {
			key = "identity-from-discovery"
		}
		if p.Reserve(key, peer, true) != nil {
			t.Fatal("raced active incoming attempt")
		}
		other := p.Reserve("other", selectionDevice(3), true)
		if other == nil {
			t.Fatal("incoming attempt suppressed unrelated peer")
		}
		other.Release()
		now = now.Add(IncomingAttemptIdle - time.Second)
		in.Progress()
		now = now.Add(2 * time.Second)
		if p.Reserve(key, peer, true) != nil {
			t.Fatal("progressing incoming stopped suppressing")
		}
		now = now.Add(IncomingAttemptIdle)
		out := p.Reserve(key, peer, true)
		if out == nil {
			t.Fatal("stalled incoming suppressed outgoing forever")
		}
		out.Release()
		in.Release()
		if out = p.Reserve(key, peer, true); out == nil {
			t.Fatal("ended incoming attempt retained suppression")
		} else {
			out.Release()
		}
	}
}

func TestIncomingSocketProgressStopsTrackingAfterReady(t *testing.T) {
	p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{2, 2, 4})
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	l := p.Reserve("address", selectionDevice(2), false)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	tracked := l.TrackIncoming(a).(*incomingProgressConn)
	now = now.Add(4 * time.Second)
	read := make(chan struct{})
	go func() { var data [1]byte; _, _ = tracked.Read(data[:]); close(read) }()
	if _, err := b.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	<-read
	if !l.progress.Equal(now) {
		t.Fatal("socket read did not report incoming progress")
	}
	if !l.Ready(selectionDevice(2), func() {}) {
		t.Fatal("ready failed")
	}
	if tracked.tracking.Load() {
		t.Fatal("established traffic still incurs handshake progress tracking")
	}
	l.Release()
}
