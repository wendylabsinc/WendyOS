package rtps

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPool(t *testing.T) (*Pool, Config) {
	t.Helper()
	p := NewPool()
	t.Cleanup(func() { _ = p.Close() })
	return p, Config{DomainID: 200, Interface: loopbackInterface(t)}
}

func acquire(t *testing.T, p *Pool, cfg Config) *Lease {
	t.Helper()
	l, err := p.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestPoolConcurrentAcquisitionAndFinalCleanup(t *testing.T) {
	pool, cfg := testPool(t)
	var created atomic.Int32
	pool.create = func(cfg Config) (*Participant, error) { created.Add(1); return NewParticipant(cfg) }
	leases := make([]*Lease, 32)
	var wg sync.WaitGroup
	for i := range leases {
		wg.Go(func() { leases[i] = acquire(t, pool, cfg) })
	}
	wg.Wait()
	p := leases[0].entry.participant
	if created.Load() != 1 || pool.Participants() != 1 {
		t.Fatalf("created %d physical participants", created.Load())
	}
	for _, l := range leases {
		if l.entry.participant != p {
			t.Fatal("concurrent acquisition not shared")
		}
		_ = l.Close()
		_ = l.Close()
	}
	if pool.Participants() != 0 {
		t.Fatal("last lease retained participant")
	}
	select {
	case <-leases[0].entry.done:
	default:
		t.Fatal("Close did not join goroutines")
	}
	if _, err := p.ucast.WriteToUDP([]byte("closed"), p.ucast.LocalAddr().(*net.UDPAddr)); err == nil {
		t.Fatal("unicast socket remained open")
	}
	if _, ok := localParticipants.Load(p.prefix); ok {
		t.Fatal("local GUID leaked")
	}
}

func TestPoolLateSnapshotCoalescingAndFilteredQueues(t *testing.T) {
	pool, cfg := testPool(t)
	camera := acquire(t, pool, cfg)
	p := camera.entry.participant
	camEP := Endpoint{GUID: GUID{Prefix: GUIDPrefix{9}, EntityID: 0x102}, Topic: "rt/camera", Type: "Image"}
	batEP := Endpoint{GUID: GUID{Prefix: GUIDPrefix{9}, EntityID: 0x202}, Topic: "rt/battery", Type: "BatteryState"}
	p.handleSEDPPublication(publicationData(camEP, 1))
	p.handleSEDPPublication(publicationData(batEP, 2))
	battery := acquire(t, pool, cfg)
	for i := 3; i < 100; i++ {
		ep := camEP
		ep.GUID.EntityID = uint32(i<<8 | 2)
		p.handleSEDPPublication(publicationData(ep, SequenceNumber(i)))
	}
	<-battery.Changed()
	if got := len(battery.Endpoints()); got != 99 {
		t.Fatalf("coalesced snapshot lost endpoints: %d", got)
	}
	if err := camera.Subscribe(camEP); err != nil {
		t.Fatal(err)
	}
	if err := battery.Subscribe(batEP); err != nil {
		t.Fatal(err)
	}
	p.deliverUserData(batEP.GUID, 1, []byte("battery"))
	for i := 1; i <= 100; i++ {
		p.deliverUserData(camEP.GUID, SequenceNumber(i), []byte("camera"))
	}
	if got := <-battery.Samples(); got.Writer != batEP.GUID || string(got.Payload) != "battery" {
		t.Fatalf("battery queue contaminated: %+v", got)
	}
	if len(camera.samples) != 4 {
		t.Fatalf("camera queue size %d", len(camera.samples))
	}
	if got := <-camera.Samples(); got.SN != 97 {
		t.Fatalf("overflow retained old sample: %+v", got)
	}
	_ = battery.Close()
	if pool.Participants() != 1 {
		t.Fatal("battery scan interrupted camera discovery")
	}
}

func TestPoolSubscriptionReferenceCountingAndDisposal(t *testing.T) {
	pool, cfg := testPool(t)
	a, b := acquire(t, pool, cfg), acquire(t, pool, cfg)
	p := a.entry.participant
	peer, loc := listener(t)
	prefix := GUIDPrefix{9}
	p.mu.Lock()
	p.peers[prefix] = []Locator{loc}
	p.mu.Unlock()
	ep := Endpoint{GUID: GUID{Prefix: prefix, EntityID: 0x102}, Topic: "rt/camera", Type: "Image"}
	for _, l := range []*Lease{a, a, b, b} {
		if err := l.Subscribe(ep); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	readers, announces := len(p.subs), len(p.subAnnounce)
	p.mu.Unlock()
	if readers != 1 || announces != 1 {
		t.Fatalf("readers=%d announces=%d", readers, announces)
	}
	buf := make([]byte, 2048)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := peer.ReadFromUDP(buf); err != nil {
		t.Fatal(err)
	}
	a.Unsubscribe(ep.GUID)
	p.deliverUserData(ep.GUID, 1, []byte("frame"))
	if len(a.samples) != 0 || len(b.samples) != 1 {
		t.Fatal("unsubscribe removed another consumer or retained delivery")
	}
	b.Unsubscribe(ep.GUID)
	p.mu.Lock()
	readers, announces = len(p.subs), len(p.subAnnounce)
	p.mu.Unlock()
	if readers != 0 || announces != 0 {
		t.Fatal("last unsubscribe retained reader announcement")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ParseMessage(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseData(msg.Submessages[0])
	if err != nil {
		t.Fatal(err)
	}
	if disposed, guid := discoveryDisposal(d); !disposed || guid.Prefix != p.prefix {
		t.Fatal("missing SEDP reader disposal")
	}
	if len(b.samples) != 0 {
		t.Fatal("unsubscribed sample remained queued")
	}
}

func TestPoolCancellationFailureAndShutdownDuringCreation(t *testing.T) {
	pool, cfg := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.Acquire(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}
	pool.create = func(Config) (*Participant, error) { return nil, errors.New("creation failed") }
	if _, err := pool.Acquire(context.Background(), cfg); err == nil {
		t.Fatal("failed creation succeeded")
	}
	if len(pool.entries) != 0 {
		t.Fatal("failed creation poisoned entry")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	pool.create = func(cfg Config) (*Participant, error) { close(entered); <-release; return NewParticipant(cfg) }
	result := make(chan error, 1)
	go func() { _, err := pool.Acquire(context.Background(), cfg); result <- err }()
	<-entered
	waitCtx, stop := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := pool.Acquire(waitCtx, cfg); waiting <- err }()
	stop()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation: %v", err)
	}
	closed := make(chan struct{})
	go func() { _ = pool.Close(); close(closed) }()
	for {
		pool.mu.Lock()
		closing := pool.closed
		pool.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("acquisition survived pool shutdown")
	}
	<-closed
	if len(pool.entries) != 0 {
		t.Fatal("creation leaked after shutdown")
	}
}

func TestPoolLeaseCancellationKeepsOtherConsumer(t *testing.T) {
	pool, cfg := testPool(t)
	a := acquire(t, pool, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	b, err := pool.Acquire(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("lease cancellation did not close")
	}
	if pool.Participants() != 1 {
		t.Fatal("cancellation closed another lease")
	}
	if err := a.Subscribe(Endpoint{GUID: GUID{EntityID: 2}, Topic: "rt/camera", Type: "Image"}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolDistinctDomainsAndResolvedInterfaceAliases(t *testing.T) {
	pool, cfg := testPool(t)
	resolve := pool.resolve
	pool.resolve = func(c Config) (*resolvedTarget, error) {
		if c.Interface == "alias" {
			c.Interface = cfg.Interface
		}
		return resolve(c)
	}
	a := acquire(t, pool, cfg)
	alias := cfg
	alias.Interface = "alias"
	b := acquire(t, pool, alias)
	other := cfg
	other.DomainID++
	c := acquire(t, pool, other)
	if a.entry != b.entry || a.entry == c.entry || pool.Participants() != 2 {
		t.Fatal("pool key did not use resolved interface and domain")
	}
}

func TestPoolCancelledCreatorReleasesSocketsAndAllowsRetry(t *testing.T) {
	pool, cfg := testPool(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var created *Participant
	pool.create = func(cfg Config) (*Participant, error) {
		var err error
		created, err = NewParticipant(cfg)
		close(entered)
		<-release
		return created, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := pool.Acquire(ctx, cfg); result <- err }()
	<-entered
	cancel()
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled creator: %v", err)
	}
	if _, ok := localParticipants.Load(created.prefix); ok {
		t.Fatal("cancelled creator leaked socket registry")
	}
	pool.create = NewParticipant
	acquire(t, pool, cfg)
	if pool.Participants() != 1 {
		t.Fatal("cancelled creation prevented retry")
	}
}

func TestPoolSharedAcquireVerifiesNamespaceOutsideTheLock(t *testing.T) {
	// Namespace ownership checks call back into containerd with no timeout.
	// A consumer holding the pool lock across one would stall every other
	// consumer's subscribe, release and listing for as long as containerd does.
	pool, cfg := testPool(t)
	acquire(t, pool, cfg)
	entered, release := make(chan struct{}), make(chan struct{})
	shared := cfg
	shared.VerifyNetworkNamespace = func() bool {
		entered <- struct{}{}
		<-release
		return true
	}
	done := make(chan error, 1)
	go func() {
		l, err := pool.Acquire(context.Background(), shared)
		if err == nil {
			_ = l.Close()
		}
		done <- err
	}()
	for {
		select {
		case <-entered:
			unlocked := make(chan struct{})
			go func() { pool.Participants(); close(unlocked) }()
			select {
			case <-unlocked:
			case <-time.After(time.Second):
				t.Error("namespace verification ran while holding the pool lock")
			}
			release <- struct{}{}
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}
