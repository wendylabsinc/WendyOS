package meshcatalog

import (
	"context"
	"fmt"
	"github.com/wendylabsinc/WendyOS/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// Version 1 exists at the services owner. The optional interface keeps these
// same regressions buildable there; version 2 cases activate on descendants.
func legacySession(t *testing.T, r *Runtime, marker bool) func(context.Context, int32, net.Conn) {
	t.Helper()
	if versioned, ok := any(r).(interface {
		sessionWithProtocol(context.Context, int32, net.Conn, bool)
	}); ok {
		return func(ctx context.Context, asset int32, conn net.Conn) {
			versioned.sessionWithProtocol(ctx, asset, conn, marker)
		}
	}
	if marker {
		t.Skip("snapshot marker is introduced by a later feature layer")
	}
	return r.session
}

type legacyBlockedConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *legacyBlockedConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}
func legacyRuntime(t *testing.T, c *Catalog) *Runtime {
	t.Helper()
	r, err := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func legacyPeer(t *testing.T, r *Runtime, asset int32) *catalogPeer {
	t.Helper()
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		r.mu.Lock()
		p := r.peers[asset]
		r.mu.Unlock()
		if p != nil {
			return p
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("peer not registered")
	return nil
}
func TestLegacyLargeInitialSnapshot(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(fmt.Sprint(marker), func(t *testing.T) {
			legacySession(t, new(Runtime), marker)
			f := newFixture(t)
			c, _ := f.newCatalog(t, 533, "default", nil, nil)
			receiver, _ := f.newCatalog(t, 534, "default", nil, nil)
			for i := 0; i < 24; i++ {
				s := testSpec()
				s.ServiceID = fmt.Sprintf("service-%d", i)
				if _, err := c.Publish(s, f.now); err != nil {
					t.Fatal(err)
				}
			}
			r := legacyRuntime(t, c)
			left, right := net.Pipe()
			defer right.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); legacySession(t, r, marker)(ctx, 534, left) }()
			defer func() {
				cancel()
				right.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("session goroutines did not join")
				}
			}()
			right.SetReadDeadline(time.Now().Add(4 * time.Second))
			syncer := NewSynchronizer(receiver, receiver.cache)
			count := 0
			for {
				m, err := ReadMessage(right)
				if err != nil {
					t.Fatal(err)
				}
				if m.Kind == "snapshot-done" {
					if !marker || count != 24 {
						t.Fatalf("premature marker after %d records", count)
					}
					break
				}
				if m.Kind == "record" {
					count++
				}
				if _, _, err := syncer.Receive(m, time.Now()); err != nil {
					t.Fatal(err)
				}
				if !marker && count == 24 {
					break
				}
			}
			if count != 24 || len(receiver.Snapshot(time.Now())) != 24 {
				t.Fatal("large snapshot truncated")
			}
		})
	}
}
func TestLegacySlowWriterOverloadAndWithdrawalReconnect(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(fmt.Sprint(marker), func(t *testing.T) {
			legacySession(t, new(Runtime), marker)
			f := newFixture(t)
			origin, _ := f.newCatalog(t, 533, "default", nil, nil)
			remote, _ := f.newCatalog(t, 534, "default", nil, nil)
			first, err := origin.Publish(testSpec(), f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = remote.cache.Put(origin.creds.Certificate.Certificate, f.now); err != nil {
				t.Fatal(err)
			}
			if _, err = remote.Accept(first, f.now); err != nil {
				t.Fatal(err)
			}
			r := legacyRuntime(t, origin)
			left, right := net.Pipe()
			defer right.Close()
			conn := &legacyBlockedConn{Conn: left, started: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); legacySession(t, r, marker)(ctx, 534, conn) }()
			defer func() {
				cancel()
				right.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("overloaded reader/writer leaked")
				}
			}()
			peer := legacyPeer(t, r, 534)
			select {
			case <-conn.started:
			case <-time.After(time.Second):
				t.Fatal("writer not blocked")
			}
			start := time.Now()
			for i := 0; i < 64; i++ {
				s := testSpec()
				s.TXT = []string{fmt.Sprintf("generation=%d", i)}
				w, err := origin.Publish(s, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				peer.out <- w
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("slow writer queue did not overload before eight-second write timeout")
			}
			if time.Since(start) >= 8*time.Second {
				t.Fatal("write deadline masked overload")
			}
			if ctx.Err() != nil {
				t.Fatal("runtime context canceled by failed peer")
			}
			waitPeerGone(t, r, 534)
			if _, err := origin.Remove(testSpec().AppID, testSpec().ServiceID, time.Now()); err != nil {
				t.Fatal(err)
			}
			rr := legacyRuntime(t, remote)
			a, b := net.Pipe()
			reconnected := make(chan struct{}, 2)
			go func() { legacySession(t, r, marker)(ctx, 534, a); reconnected <- struct{}{} }()
			go func() { legacySession(t, rr, marker)(ctx, 533, b); reconnected <- struct{}{} }()
			waitCatalog(t, remote, 0)
			if len(remote.Records(time.Now())) != 1 {
				t.Fatal("withdrawal tombstone missing after reconnect")
			}
			cancel()
			a.Close()
			b.Close()
			for i := 0; i < 2; i++ {
				select {
				case <-reconnected:
				case <-time.After(time.Second):
					t.Fatal("reconnect goroutines leaked")
				}
			}
		})
	}
}

// The periodic reconciliation batch is generated after the initial record.
// Delay the blocked write until just before the real 15s ticker, ensuring its
// 8s deadline cannot be the reason the session exits.
func TestLegacyPeriodicOversizedReconcileClosesSession(t *testing.T) {
	f := newFixture(t)
	origin, _ := f.newCatalog(t, 533, "default", nil, nil)
	if _, err := origin.Publish(testSpec(), f.now); err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(origin, func() localmesh.NodeSnapshot { return legacyRouteView(t, time.Now(), 534) })
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer right.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); legacySession(t, r, false)(ctx, 534, left) }()
	defer func() {
		cancel()
		right.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("periodic overload did not join")
		}
	}()
	right.SetReadDeadline(time.Now().Add(time.Second))
	for {
		m, err := ReadMessage(right)
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind == "record" {
			break
		}
	}
	peer := legacyPeer(t, r, 534)
	time.Sleep(12 * time.Second)
	var first SignedRecord
	for i := 0; i < 24; i++ {
		s := testSpec()
		s.ServiceID = fmt.Sprintf("service-%d", i)
		w, err := origin.Publish(s, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = w
		}
	}
	start := time.Now()
	peer.out <- first
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("oversized periodic batch did not close session before write timeout")
	}
	if time.Since(start) >= 8*time.Second {
		t.Fatal("write timeout masked reconciliation overload")
	}
	waitPeerGone(t, r, 534)
	// Session-local stamps from both the partial write and rejected reconciliation
	// disappear on reconnect; every signed record must be delivered anew.
	receiver, _ := f.newCatalog(t, 534, "default", nil, nil)
	rr := legacyRuntime(t, receiver)
	a, b := net.Pipe()
	joined := make(chan struct{}, 2)
	go func() { legacySession(t, r, false)(ctx, 534, a); joined <- struct{}{} }()
	go func() { legacySession(t, rr, false)(ctx, 533, b); joined <- struct{}{} }()
	waitCatalog(t, receiver, 25)
	cancel()
	a.Close()
	b.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Fatal("reconnected session leaked")
		}
	}
}

func legacyRouteView(t *testing.T, now time.Time, asset int32) localmesh.NodeSnapshot {
	t.Helper()
	addr, _, err := localmesh.Addresses(64, asset)
	if err != nil {
		t.Fatal(err)
	}
	router, err := localmesh.RouterID(64, asset)
	if err != nil {
		t.Fatal(err)
	}
	return localmesh.NodeSnapshot{Devices: []localmesh.Manifest{{Org: 64, Asset: asset, Expires: now.Add(time.Minute).UnixMilli()}}, Routes: []babel.Route{{Prefix: netip.PrefixFrom(addr, 32), RouterID: router, Link: 2}}}
}
