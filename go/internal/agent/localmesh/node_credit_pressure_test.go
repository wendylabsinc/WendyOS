package localmesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

type creditProgressNetwork struct {
	handoffActorNetwork
	maintenance atomic.Int32
}

func (n *creditProgressNetwork) ReconcileBase() error { n.maintenance.Add(1); return nil }

type creditPressureLink struct {
	creditLatencyLink
	writes            atomic.Int32
	replies           atomic.Int32
	haves             atomic.Int32
	priorityViolation atomic.Bool
	maxQueuedEvents   atomic.Int32
	events            chan nodeEvent
	delay             time.Duration
	mu                sync.Mutex
	seen              map[netgossip.ID]int
}

func (l *creditPressureLink) WriteControl(m ControlMessage) error {
	// Keep the real writer occupied past a maintenance interval, with bounded
	// output and no artificial consumer of the actor's control channel.
	if l.delay > 0 {
		timer := time.NewTimer(l.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-l.closed:
			return io.ErrClosedPipe
		}
	}
	l.writes.Add(1)
	for queued := int32(len(l.events)); ; {
		old := l.maxQueuedEvents.Load()
		if old >= queued || l.maxQueuedEvents.CompareAndSwap(old, queued) {
			break
		}
	}
	if m.Kind == "identity-request" {
		l.replies.Add(1)
	}
	if m.Gossip != nil && m.Gossip.Op == "have" {
		if l.replies.Load() < 8 {
			l.priorityViolation.Store(true)
		}
		l.mu.Lock()
		l.seen[m.Gossip.ID()]++
		l.mu.Unlock()
		l.haves.Add(1)
	}
	return nil
}

// Real Run, maintenance, actor events and writer operate together under a
// directory larger than the production 128-frame queue. Independent actor
// requests and the one-second maintenance tick must still make progress.
func TestControlCreditSustainedPressurePreservesActorProgress(t *testing.T) {
	testControlCreditPressure(t, 3*time.Millisecond)
}

func TestControlCreditFastDrainDoesNotLoseWake(t *testing.T) {
	testControlCreditPressure(t, 0)
}

func testControlCreditPressure(t *testing.T, delay time.Duration) {
	origins := 256
	if delay == 0 {
		// Just over one production queue exercises the refill race without
		// making the deadline mostly a certificate-processing benchmark
		// under the race detector.
		origins = 129
	}
	self, remote := testCredentials(t)
	network := &creditProgressNetwork{}
	n := newActorTestNode(t, self, network)
	now := time.Now()
	for i := range origins {
		c := testCredentialsFor(t, testPrincipal(testTenantUUID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)))
		if _, err := n.cache.Put(c.Certificate.Certificate, now); err != nil {
			t.Fatal(err)
		}
		w, err := SignManifest(routeManifest(c.Device, now), c.Certificate.Certificate, c.Signer, now)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := n.directory.Accept(w, now); err != nil || !changed {
			t.Fatalf("accept %d: %t %v", i, changed, err)
		}
	}
	link := &creditPressureLink{creditLatencyLink: creditLatencyLink{closed: make(chan struct{})}, events: n.events, seen: make(map[netgossip.ID]int), delay: delay}
	p := &nodePeer{id: 1, device: remote.Device, link: link, control: make(chan ControlMessage, 128), done: make(chan struct{}), sync: NewSynchronizer(n.directory, n.cache, n.controlHub.Announcements())}
	n.peers[1] = p
	for range cap(p.control) {
		p.control <- ControlMessage{Kind: "identity-missing", Principal: self.Device.Principal()}
	}
	p.sync.seedAnnouncements("", now)
	replies := make([]ControlMessage, 8)
	for i := range replies {
		replies[i] = ControlMessage{Kind: "identity-request", Principal: remote.Device.Principal()}
	}
	if !n.queueReplies(p, replies) || len(p.replies) != len(replies) {
		t.Fatal("failed to retain replies")
	}
	n.flushAnnouncements(p, now)
	runDone, writerDone := make(chan error, 1), make(chan error, 1)
	go func() { runDone <- n.Run() }()
	writerStarted := false
	t.Cleanup(func() {
		n.cancel()
		_ = link.Close()
		select {
		case err := <-runDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("actor exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("actor did not stop")
		}
		if writerStarted {
			select {
			case <-writerDone:
			case <-time.After(3 * time.Second):
				t.Error("writer did not stop")
			}
		}
	})
	ctx, cancel := context.WithTimeout(n.ctx, 8*time.Second)
	defer cancel()
	if err := n.request(ctx, nodeEvent{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	writerStarted = true
	go func() { writerDone <- n.writeControls(n.ctx, p) }()
	requests := 0
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for link.haves.Load() < int32(origins+1) {
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("sustained inventory did not finish")
		}
		requestCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		err := n.request(requestCtx, nodeEvent{})
		stop()
		if err != nil {
			t.Fatalf("other actor work starved: %v", err)
		}
		requests++
	}
	if delay > 0 && network.maintenance.Load() < 2 {
		t.Fatal("maintenance did not run during sustained output")
	}
	if link.priorityViolation.Load() {
		t.Fatal("HAVE overtook retained reply")
	}
	if got := link.writes.Load(); got != int32(128+8+origins+1) {
		t.Fatalf("unexpected control work: %d frames", got)
	}
	link.mu.Lock()
	for id, count := range link.seen {
		if count != 1 {
			t.Errorf("repeated HAVE %v count%d", id, count)
		}
	}
	link.mu.Unlock()
	if got := link.maxQueuedEvents.Load(); got > 2 {
		t.Fatalf("credit notifications filled actor queue: max%d", got)
	}
	if delay == 0 && time.Since(start) >= 500*time.Millisecond {
		t.Fatalf("fast drain lost wake and waited for maintenance: %v", time.Since(start))
	}
	t.Logf("pressure_elapsed_ms=%d frames=%d actor_requests=%d maintenance_runs=%d max_actor_queue=%d", time.Since(start).Milliseconds(), link.writes.Load(), requests, network.maintenance.Load(), link.maxQueuedEvents.Load())
}
