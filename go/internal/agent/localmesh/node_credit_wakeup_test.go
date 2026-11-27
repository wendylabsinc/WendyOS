package localmesh

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

type creditBlockedLink struct {
	creditLatencyLink
	entered chan struct{}
	started atomic.Bool
}

func (l *creditBlockedLink) WriteControl(m ControlMessage) error {
	if l.started.CompareAndSwap(false, true) {
		close(l.entered)
		<-l.closed
		return io.ErrClosedPipe
	}
	return l.creditLatencyLink.WriteControl(m)
}

func TestControlCreditRefillsPartialDrainBeforeBlockedWriteCompletes(t *testing.T) {
	self, remote := testCredentials(t)
	n := newActorTestNode(t, self, &handoffActorNetwork{})
	link := &creditBlockedLink{creditLatencyLink: creditLatencyLink{writes: make(chan ControlMessage, 256), closed: make(chan struct{})}, entered: make(chan struct{})}
	p := &nodePeer{device: remote.Device, cost: 256, transport: "test", link: link, control: make(chan ControlMessage, 128), done: make(chan struct{})}
	for range cap(p.control) {
		p.control <- ControlMessage{Kind: "identity-request", Principal: self.Device.Principal()}
	}
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
				t.Error("blocked writer did not stop")
			}
		}
	})
	ctx, cancel := context.WithTimeout(n.ctx, 500*time.Millisecond)
	defer cancel()
	if err := n.request(ctx, nodeEvent{add: p}); err != nil {
		t.Fatal(err)
	}
	writerStarted = true
	go func() { writerDone <- n.writeControls(n.ctx, p) }()
	select {
	case <-link.entered:
	case <-ctx.Done():
		t.Fatal("writer never entered blocked transport")
	}
	// The writer queued its credit event before entering the transport; this
	// barrier waits for that real actor event without advancing maintenance.
	if err := n.request(ctx, nodeEvent{}); err != nil {
		t.Fatal(err)
	}
	if got := len(p.control); got != cap(p.control) {
		t.Fatalf("one freed slot remained empty during blocked write: %d/%d", got, cap(p.control))
	}
	if err := n.request(ctx, nodeEvent{id: p.id, remove: true}); err != nil {
		t.Fatal(err)
	}
	// A final writer notification can race removal. The exact old incarnation
	// must not revive the peer or fail the actor after its synchronizer closes.
	p.controlCreditWanted.Store(true)
	n.signalControlCredit(p)
	if err := n.request(ctx, nodeEvent{}); err != nil {
		t.Fatal(err)
	}
}

func TestAnnouncementCreditWakesPromptly(t *testing.T) {
	t.Setenv("WENDY_EXPECT_FAST_CREDIT", "1")
	TestAnnouncementCreditLatency(t)
}

func TestControlCreditNotificationsCoalesceAndSurviveFullEventQueue(t *testing.T) {
	n := &Node{events: make(chan nodeEvent, 1)}
	p := &nodePeer{id: 1}
	n.events <- nodeEvent{}
	p.controlCreditWanted.Store(true)
	n.signalControlCredit(p)
	if !p.controlCreditWanted.Load() || p.controlCreditPending.Load() {
		t.Fatal("full event queue consumed notification")
	}
	<-n.events
	n.signalControlCredit(p)
	if p.controlCreditWanted.Load() || !p.controlCreditPending.Load() {
		t.Fatal("deferred notification was not delivered")
	}
	for range 100 {
		p.controlCreditWanted.Store(true)
		n.signalControlCredit(p)
	}
	if len(n.events) != 1 {
		t.Fatal("credit events were not coalesced")
	}
	e := <-n.events
	if e.id != p.id || e.controlCredit != p {
		t.Fatal("notification did not preserve peer incarnation")
	}
}

func TestControlCreditIgnoresRemovedAndReplacedPeer(t *testing.T) {
	old := &nodePeer{id: 1}
	n := &Node{peers: map[babel.LinkID]*nodePeer{}}
	if err := n.handle(nodeEvent{id: 1, controlCredit: old}); err != nil {
		t.Fatal(err)
	}
	current := &nodePeer{id: 1}
	n.peers[1] = current
	if err := n.handle(nodeEvent{id: 1, controlCredit: old}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityDeferredAnnouncementDoesNotSelfWake(t *testing.T) {
	self, _ := testCredentials(t)
	n := newActorTestNode(t, self, &handoffActorNetwork{})
	t.Cleanup(func() { n.cancel(); _ = n.Stop() })
	now := time.Now()
	w, err := SignManifest(routeManifest(self.Device, now), self.Certificate.Certificate, self.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := n.directory.Accept(w, now); err != nil || !changed {
		t.Fatalf("accept: %t %v", changed, err)
	}
	p := &nodePeer{id: 1, sync: NewSynchronizer(n.directory, n.cache, n.controlHub.Announcements()), control: make(chan ControlMessage, 128)}
	defer p.sync.Close()
	p.sync.markAnnouncement(w, now)
	n.cache.mu.Lock()
	delete(n.cache.entries, self.Device.Principal())
	n.cache.mu.Unlock()
	for range 100 {
		n.flushAnnouncements(p, now)
	}
	if len(n.events) != 0 || p.controlCreditWanted.Load() || p.controlCreditPending.Load() {
		t.Fatal("identity repair wait self-woke without output progress")
	}
	if len(p.sync.pendingAnnouncements) != 1 {
		t.Fatal("identity loss discarded pending origin")
	}
	// A partially occupied channel is not evidence of new capacity. Repeated
	// actor attempts must wait for actual writer progress, not self-notify.
	for range 10 {
		p.control <- ControlMessage{Kind: "identity-missing", Principal: self.Device.Principal()}
	}
	for range 100 {
		n.flushAnnouncements(p, now)
	}
	if len(n.events) != 0 || p.controlCreditPending.Load() {
		t.Fatal("partial channel caused identity retry spin")
	}
}

func BenchmarkControlCreditNotification(b *testing.B) {
	for _, pending := range []bool{false, true} {
		name := "idle"
		if pending {
			name = "pending"
		}
		b.Run(name, func(b *testing.B) {
			n := &Node{events: make(chan nodeEvent, 1)}
			p := &nodePeer{id: 1}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if pending {
					p.controlCreditWanted.Store(true)
				}
				n.signalControlCredit(p)
				if pending {
					<-n.events
					p.controlCreditPending.Store(false)
				}
			}
		})
	}
}
