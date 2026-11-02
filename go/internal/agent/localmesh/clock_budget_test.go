package localmesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

func TestClockRecoveryAttemptWindowCannotBeExtendedByMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadlines := make(chan time.Time, 2)
	p := &nodePeer{clockRelay: true}
	n := &Node{ctx: ctx, events: make(chan nodeEvent, 2), peers: map[babel.LinkID]*nodePeer{1: p}, clockSync: func(ctx context.Context, _ roughtime.RelayQuery) error {
		deadline, _ := ctx.Deadline()
		deadlines <- deadline
		return errors.New("temporary authority outage")
	}}
	finishRound := func() {
		t.Helper()
		select {
		case event := <-n.events:
			if err := n.handleClockEvent(event.clock); err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("authority round did not stop")
		}
	}
	n.clockBlocked(p)
	finishRound()
	window, peerDeadline := n.clockWindowEnd, p.clockWaitingUntil
	if n.clockAttempts != 1 || n.clockNext.Before(window.Add(-20*time.Second)) {
		t.Fatal("first retry was not spaced by10s")
	}
	n.clockBlocked(p) // Repeated authenticated input cannot reset either window.
	if n.clockAttempts != 1 || n.clockWindowEnd != window || p.clockWaitingUntil != peerDeadline {
		t.Fatal("input extended clock budget")
	}
	// Make only the retry eligibility instant due, without changing clocks,
	// attempt windows or peer deadlines; the second round shares the first.
	n.clockNext = time.Now().Add(-time.Second)
	n.startClockRecovery()
	finishRound()
	if n.clockAttempts != 2 || n.clockWindowEnd != window || n.clockNext != window.Add(time.Minute) {
		t.Fatal("second round extended window or omitted cooldown")
	}
	n.clockNext = time.Now().Add(-time.Second)
	n.clockBlocked(p)
	if n.clockAttempts != 2 || n.clockRunning || n.clockWindowEnd != window || p.clockWaitingUntil != peerDeadline {
		t.Fatal("third round bypassed fixed attempt cap")
	}
	for range 2 {
		if deadline := <-deadlines; deadline != window {
			t.Fatal("round escaped fixed monotonic recovery window")
		}
	}
}
