package meshcatalog

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestRuntimePinnedTLS13PeerOrder(t *testing.T) {
	f := newFixture(t)
	serverCatalog, _ := f.newCatalog(t, 534, "default", nil, nil)
	server, _ := NewRuntime(serverCatalog, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	for _, clientAsset := range []int32{533, 535} {
		t.Run(itoa(clientAsset), func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			_ = left.SetDeadline(time.Now().Add(5 * time.Second))
			_ = right.SetDeadline(time.Now().Add(5 * time.Second))
			serverResult := make(chan error, 1)
			go func() {
				err := tls.Server(right, server.serverTLS()).Handshake()
				if err != nil {
					_ = right.Close()
				}
				serverResult <- err
			}()
			config, err := f.creds[clientAsset].PeerTLS(534)
			if err != nil {
				t.Fatal(err)
			}
			config.NextProtos = []string{syncALPN}
			client := tls.Client(left, config)
			clientErr := client.Handshake()
			serverErr := <-serverResult
			if clientAsset == 533 {
				if clientErr != nil || serverErr != nil {
					t.Fatalf("valid mTLS: client=%v server=%v", clientErr, serverErr)
				}
				if state := client.ConnectionState(); state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != syncALPN {
					t.Fatalf("TLS version/ALPN: %d %q", state.Version, state.NegotiatedProtocol)
				}
			} else if serverErr == nil {
				t.Fatal("wrong connector asset admitted")
			}
		})
	}
}

func TestRuntimeCatalogALPNRollingUpgrade(t *testing.T) {
	f := newFixture(t)
	serverCatalog, _ := f.newCatalog(t, 534, "default", nil, nil)
	server, _ := NewRuntime(serverCatalog, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	for _, tc := range []struct {
		name, want string
		serverALPN []string
	}{
		{name: "new-to-new", want: syncALPNv2, serverALPN: []string{syncALPNv2, syncALPN}},
		{name: "new-to-old", want: syncALPN, serverALPN: []string{syncALPN}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			_ = left.SetDeadline(time.Now().Add(5 * time.Second))
			_ = right.SetDeadline(time.Now().Add(5 * time.Second))
			serverConfig := server.serverTLS()
			serverConfig.NextProtos = tc.serverALPN
			serverResult := make(chan error, 1)
			go func() { serverResult <- tls.Server(right, serverConfig).Handshake() }()
			clientConfig, err := f.creds[533].PeerTLS(534)
			if err != nil {
				t.Fatal(err)
			}
			clientConfig.NextProtos = []string{syncALPNv2, syncALPN}
			client := tls.Client(left, clientConfig)
			clientErr := client.Handshake()
			serverErr := <-serverResult
			if clientErr != nil || serverErr != nil {
				t.Fatalf("catalog mTLS: client=%v server=%v", clientErr, serverErr)
			}
			if got := client.ConnectionState().NegotiatedProtocol; got != tc.want {
				t.Fatalf("negotiated %q, want %q", got, tc.want)
			}
		})
	}
}

func waitCatalog(t *testing.T, c *Catalog, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot(time.Now())) == count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("catalog asset %d has %d records, want %d", c.asset, len(c.Snapshot(time.Now())), count)
}

func waitPeerGone(t *testing.T, r *Runtime, asset int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		gone := r.peers[asset] == nil
		r.mu.Unlock()
		if gone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("peer %d still registered", asset)
}

func TestRuntimeThreeHopPartitionAndReconcile(t *testing.T) {
	f := newFixture(t)
	a, _ := f.newCatalog(t, 533, "default", nil, nil)
	b, _ := f.newCatalog(t, 534, "default", nil, nil)
	c, _ := f.newCatalog(t, 535, "default", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	view := func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} }
	ra, _ := NewRuntime(a, view)
	rb, _ := NewRuntime(b, view)
	rc, _ := NewRuntime(c, view)
	connect := func(left *Runtime, leftPeer int32, right *Runtime, rightPeer int32) (net.Conn, net.Conn) {
		l, r := net.Pipe()
		go left.session(ctx, leftPeer, l)
		go right.session(ctx, rightPeer, r)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			left.mu.Lock()
			leftReady := left.peers[leftPeer] != nil
			left.mu.Unlock()
			right.mu.Lock()
			rightReady := right.peers[rightPeer] != nil
			right.mu.Unlock()
			if leftReady && rightReady {
				return l, r
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("test peer session did not start")
		return nil, nil
	}
	ab1, ab2 := connect(ra, 534, rb, 533)
	defer ab1.Close()
	defer ab2.Close()
	bc1, bc2 := connect(rb, 535, rc, 534)
	defer bc1.Close()
	defer bc2.Close()
	w, err := a.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	ra.Broadcast(w)
	waitCatalog(t, c, 1)
	_ = bc1.Close()
	_ = bc2.Close()
	waitPeerGone(t, rb, 535)
	waitPeerGone(t, rc, 534)
	tombstone, err := a.Remove("com.wendy.test", "http", f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ra.Broadcast(tombstone)
	waitCatalog(t, b, 0)
	if len(c.Snapshot(time.Now())) != 1 {
		t.Fatal("partitioned third agent lost service before reconciliation")
	}
	bc3, bc4 := connect(rb, 535, rc, 534)
	defer bc3.Close()
	defer bc4.Close()
	waitCatalog(t, c, 0)
}

func TestProjectionReadyWaitsForOriginTombstoneSnapshot(t *testing.T) {
	f := newFixture(t)
	viewer, cache := f.newCatalog(t, 533, "default", nil, nil)
	origin, _ := f.newCatalog(t, 535, "default", nil, nil)
	if _, err := cache.Put(f.creds[535].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	wire, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Accept(wire, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := origin.Remove(testSpec().AppID, testSpec().ServiceID, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	view := bridgeRouteView(t, f.now, 535)
	ra, _ := NewRuntime(viewer, func() localmesh.NodeSnapshot { return view })
	rc, _ := NewRuntime(origin, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	go ra.session(ctx, 535, left)
	go rc.session(ctx, 533, right)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ra.ProjectionReady(535) {
			if len(viewer.Snapshot(time.Now())) != 0 {
				t.Fatal("origin snapshot marked ready before its tombstone was admitted")
			}
			ra.InvalidateProjection(535)
			if ra.ProjectionReady(535) {
				t.Fatal("invalidated route retained catalog projection readiness")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("catalog origin snapshot never completed")
}

// Force a write failure while the reader is blocked handing off decoded
// records. The runtime itself stays alive, as it would across peer reconnects.
type failedCatalogWriter struct {
	net.Conn
	fail    <-chan struct{}
	started chan struct{}
	once    sync.Once
}

func (c *failedCatalogWriter) Write([]byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	<-c.fail
	return 0, errors.New("test peer write failed")
}
func TestRuntimeFailedSessionJoinsBackloggedReader(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	receiver, cache := f.newCatalog(t, 533, "default", nil, nil)
	// V1 sends records without an initial manifest. Seed a real outbound
	// record before blocking incoming persistence so both protocol versions
	// start the failing writer independently of reader progress.
	if _, err := receiver.Publish(testSpec(), f.now); err != nil {
		t.Fatal(err)
	}
	receiver.persist = func([]Receipt) error {
		once.Do(func() { close(entered); <-release })
		return nil
	}
	origin, _ := f.newCatalog(t, 535, "default", nil, nil)
	if _, err := cache.Put(f.creds[535].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	record, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := NewRuntime(receiver, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fail := make(chan struct{})
	started := make(chan struct{})
	var releaseOnce, failOnce sync.Once
	unblock := func() { failOnce.Do(func() { close(fail) }); releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	sessionDone := make(chan struct{})
	go func() {
		defer close(sessionDone)
		runtime.session(ctx, 535, &failedCatalogWriter{Conn: left, fail: fail, started: started})
	}()
	// Do not let a racing incoming Accept block the session select before
	// it queues that first write; then deliberately fill its read backlog.
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("writer started")
	}
	sent := make(chan struct{})
	go func() {
		// One record is in Accept, sixteen are queued and the eighteenth is
		// decoded by the reader, which cannot enqueue it until Accept returns.
		for i := 0; ; i++ {
			if WriteMessage(right, Message{Kind: "record", Record: &record}) != nil {
				return
			}
			if i == 17 {
				close(sent)
			}
		}
	}()
	for _, phase := range []struct {
		name string
		done <-chan struct{}
	}{
		{"writer started", started}, {"accept blocked", entered}, {"reader queue filled", sent},
	} {
		select {
		case <-phase.done:
		case <-time.After(3 * time.Second):
			t.Fatal(phase.name)
		}
	}
	failOnce.Do(func() { close(fail) })
	// Keep Accept blocked while the writer reports its failure.
	time.Sleep(40 * time.Millisecond)
	releaseOnce.Do(func() { close(release) })
	select {
	case <-sessionDone:
	case <-time.After(3 * time.Second):
		t.Fatal("failed session retained its reader while runtime stayed alive")
	}
	if ctx.Err() != nil {
		t.Fatal("session cancelled the runtime context")
	}
}
