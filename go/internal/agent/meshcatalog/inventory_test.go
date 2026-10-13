package meshcatalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestInventoryFramingAndBounds(t *testing.T) {
	fps := make([]string, maxInventoryEntries)
	for i := range fps {
		fps[i] = fmt.Sprintf("%064x", i)
	}
	var received receivedInventory
	var wire bytes.Buffer
	for _, m := range inventoryMessages(fps) {
		if err := writeInventoryMessage(&wire, m); err != nil {
			t.Fatal(err)
		}
		got, err := readInventoryMessage(&wire)
		if err != nil {
			t.Fatal(err)
		}
		if err = received.receive(got); err != nil {
			t.Fatal(err)
		}
	}
	if !received.done || len(received.fingerprints) != maxInventoryEntries {
		t.Fatal("inventory incomplete")
	}
	if err := received.receive(inventoryMessage{Message: Message{Kind: "identity-cache-done"}}); err == nil {
		t.Fatal("duplicate completion accepted")
	}
	var duplicate receivedInventory
	m := inventoryMessages(fps[:1])[0]
	if duplicate.receive(m) != nil || duplicate.receive(m) == nil {
		t.Fatal("duplicate inventory accepted")
	}
	var oversized receivedInventory
	for _, m := range inventoryMessages(append(fps, fmt.Sprintf("%064x", 1024)))[:8] {
		if err := oversized.receive(m); err != nil {
			t.Fatal(err)
		}
	}
	if oversized.receive(inventoryMessages([]string{fmt.Sprintf("%064x", 1024)})[0]) == nil {
		t.Fatal("oversized inventory accepted")
	}
	invalid := []inventoryMessage{
		{Message: Message{Kind: "identity-cache"}},
		{Message: Message{Kind: "identity-cache"}, Fingerprints: make([]byte, 33)},
		{Message: Message{Kind: "identity-cache"}, Fingerprints: make([]byte, 32*(inventoryChunkEntries+1))},
		{Message: Message{Kind: "identity-cache-done"}, Fingerprints: make([]byte, 32)},
		{Message: Message{Kind: "snapshot-done"}, Fingerprints: make([]byte, 32)},
	}
	for _, m := range invalid {
		if writeInventoryMessage(new(bytes.Buffer), m) == nil {
			t.Fatalf("invalid frame accepted: %s", m.Kind)
		}
	}
	var early receivedInventory
	if early.receive(inventoryMessage{Message: Message{Kind: "snapshot-done"}}) == nil {
		t.Fatal("data before inventory accepted")
	}
	var old bytes.Buffer
	if err := writeInventoryMessage(&old, inventoryMessages(fps[:1])[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMessage(&old); err == nil {
		t.Fatal("legacy parser accepted inventory")
	}
	for _, raw := range []string{`{"kind":"identity-cache-done","unknown":1}`, `{"kind":"identity-cache-done"} {}`, `{"kind":"identity-cache","fingerprints":"%%%"}`} {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(len(raw)))
		b.WriteString(raw)
		if _, err := readInventoryMessage(&b); err == nil {
			t.Fatalf("malformed wire accepted: %s", raw)
		}
	}
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(MaxMessageBytes+1))
	if _, err := readInventoryMessage(&b); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestInventoryQueueBoundsAndRelease(t *testing.T) {
	var q inventoryQueue
	m := inventoryMessage{Message: Message{Kind: "snapshot-done"}}
	for range maxInventoryQueueFrames {
		if err := q.add(m); err != nil {
			t.Fatal(err)
		}
	}
	if q.add(m) == nil {
		t.Fatal("unbounded frame queue")
	}
	for range maxInventoryQueueFrames {
		q.pop()
	}
	if q.bytes != 0 || len(q.messages) != 0 {
		t.Fatal("queue retained bytes")
	}
	large := inventoryMessage{Message: Message{Kind: "bundle", Bundle: [][]byte{make([]byte, 40000)}}}
	if q.add(large, large) != nil || q.add(large) == nil {
		t.Fatal("byte queue bound not enforced")
	}
}

func TestInventoryOneColdRetryThenCooldown(t *testing.T) {
	r := &Runtime{}
	now := time.Now()
	if cold, ok := r.inventoryAttempt(535, now); cold || !ok {
		t.Fatal("first attempt")
	}
	r.inventoryFailed(535, false, now)
	if cold, ok := r.inventoryAttempt(535, now.Add(time.Second)); !cold || !ok {
		t.Fatal("cold retry missing")
	}
	r.inventoryFailed(535, true, now.Add(time.Second))
	if _, ok := r.inventoryAttempt(535, now.Add(2*time.Second)); ok {
		t.Fatal("unbounded cold retry")
	}
	if cold, ok := r.inventoryAttempt(535, now.Add(inventoryRetryCooldown+2*time.Second)); cold || !ok {
		t.Fatal("cooldown did not expire")
	}
	r.inventoryFailed(535, false, now)
	r.inventoryReady(535)
	if cold, ok := r.inventoryAttempt(535, now); cold || !ok {
		t.Fatal("successful repair retained retry")
	}
	for i := range maxInventoryRetryPeers {
		r.inventoryFailed(int32(i+1), false, now)
	}
	r.inventoryFailed(2000, false, now)
	if len(r.inventoryRetries) > maxInventoryRetryPeers {
		t.Fatal("unbounded retry map")
	}
	if _, ok := r.inventoryAttempt(2000, now); ok {
		t.Fatal("overflow did not back off")
	}
}

func TestInventoryDoesNotGrantTrustOrBypassSignatures(t *testing.T) {
	f := newFixture(t)
	origin, oc := f.newCatalog(t, 535, "default", nil, nil)
	w, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	allowed := true
	cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
		if !allowed {
			return localmesh.Identity{}, errors.New("revoked")
		}
		return f.creds[533].Verify(chain, now)
	}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := NewCatalog("default", 64, 533, 32, f.creds[533], cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(f.creds[535].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	fps := cache.Fingerprints(f.now, 1024)
	if len(fps) != 2 {
		t.Fatal("valid identity enumeration")
	}
	var inv receivedInventory
	for _, m := range inventoryMessages(fps) {
		if err = inv.receive(m); err != nil {
			t.Fatal(err)
		}
	}
	sender := NewSynchronizer(origin, oc)
	sender.useCurrentInventory(inv)
	frames := sender.Record(w, f.now)
	if len(frames) != 1 || frames[0].Kind != "record" {
		t.Fatal("inventory did not omit bundle")
	}
	receiver := NewSynchronizer(viewer, cache)
	tampered := cloneWire(w)
	tampered.Signature[0] ^= 0xff
	if _, _, err = receiver.Receive(Message{Kind: "record", Record: &tampered}, f.now); err == nil {
		t.Fatal("cached inventory bypassed signature")
	}
	allowed = false
	if got := cache.Fingerprints(f.now, 1024); len(got) != 0 {
		t.Fatal("revoked identities advertised")
	}
	if _, changed, err := receiver.Receive(frames[0], f.now); err != nil || len(changed) != 0 || !receiver.PendingIdentity() {
		t.Fatalf("revoked identity admitted: %v", err)
	}
	if _, _, err = receiver.Receive(Message{Kind: "bundle", Bundle: f.creds[535].Certificate.Certificate}, f.now); err == nil {
		t.Fatal("repair bypassed revoked trust")
	}
	if len(viewer.Snapshot(f.now)) != 0 {
		t.Fatal("untrusted service visible")
	}
	allowed = true
	if len(cache.Fingerprints(f.now.Add(25*time.Hour), 1024)) != 0 {
		t.Fatal("expired identities advertised")
	}
}

func TestInventoryStaleAdvertisementCannotExceedPendingBound(t *testing.T) {
	f := newFixture(t)
	origin, oc := f.newCatalog(t, 535, "default", nil, nil)
	viewer, cache := f.newCatalog(t, 533, "default", nil, nil)
	fp := localmesh.Fingerprint(f.creds[535].Certificate.Certificate)
	sender := NewSynchronizer(origin, oc)
	sender.useCurrentInventory(receivedInventory{fingerprints: map[string]bool{fp: true}, done: true})
	receiver := NewSynchronizer(viewer, cache)
	for i := range 9 {
		spec := testSpec()
		spec.ServiceID = fmt.Sprintf("http-%d", i)
		w, err := origin.Publish(spec, f.now)
		if err != nil {
			t.Fatal(err)
		}
		frames := sender.Record(w, f.now)
		if len(frames) != 1 {
			t.Fatal("current inventory unexpectedly reverted to four hints")
		}
		_, _, err = receiver.Receive(frames[0], f.now)
		if i < 8 && err != nil {
			t.Fatal(err)
		}
		if i == 8 && !errors.Is(err, errPendingIdentities) {
			t.Fatal("ninth pending identity did not fail closed")
		}
	}
	if n := len(receiver.pending[fp]); n != 8 {
		t.Fatalf("pending=%d", n)
	}
	if len(viewer.Snapshot(f.now)) != 0 {
		t.Fatal("unverified snapshot projected")
	}
	// Empty inventory cold retry carries its bundle first and admits all records.
	retry := NewSynchronizer(origin, oc)
	fresh := NewSynchronizer(viewer, cache)
	for _, m := range retry.Reconcile(f.now) {
		if _, _, err := fresh.Receive(m, f.now); err != nil {
			t.Fatal(err)
		}
	}
	if fresh.PendingIdentity() || len(viewer.Snapshot(f.now)) != 9 {
		t.Fatal("cold retry failed")
	}
}

func TestInventoryRuntimeOriginWithdrawalBarrier(t *testing.T) {
	f := newFixture(t)
	viewer, cache := f.newCatalog(t, 533, "default", nil, nil)
	origin, _ := f.newCatalog(t, 535, "default", nil, nil)
	_, _ = cache.Put(f.creds[535].Certificate.Certificate, f.now)
	w, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = viewer.Accept(w, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err = origin.Remove(testSpec().AppID, testSpec().ServiceID, f.now); err != nil {
		t.Fatal(err)
	}
	view := bridgeRouteView(t, f.now, 535)
	a, _ := NewRuntime(viewer, func() localmesh.NodeSnapshot { return view })
	b, _ := NewRuntime(origin, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, r := net.Pipe()
	defer l.Close()
	defer r.Close()
	done := make(chan struct{}, 2)
	go func() { a.sessionInventory(ctx, 535, l); done <- struct{}{} }()
	go func() { b.sessionInventory(ctx, 533, r); done <- struct{}{} }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a.ProjectionReady(535) {
			if len(viewer.Snapshot(time.Now())) != 0 {
				t.Fatal("warm inventory bypassed tombstone snapshot")
			}
			a.InvalidateProjection(535)
			if a.ProjectionReady(535) {
				t.Fatal("route invalidation retained readiness")
			}
			for range 2 {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("session goroutine leaked")
				}
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("inventory snapshot never ready")
}

func TestInventoryFingerprintPackingMatchesLeafDigest(t *testing.T) {
	f := newFixture(t)
	fp := localmesh.Fingerprint(f.creds[535].Certificate.Certificate)
	m := inventoryMessages([]string{fp})[0]
	if len(m.Fingerprints) != 32 || hex.EncodeToString(m.Fingerprints) != fp {
		t.Fatal("fingerprint encoding changed")
	}
}

func TestInventoryRuntimeEvictionColdFallback(t *testing.T) {
	for _, succeed := range []bool{false, true} {
		t.Run(fmt.Sprint(succeed), func(t *testing.T) {
			f := newFixture(t)
			viewer, cache := f.newCatalog(t, 533, "default", nil, nil)
			origin, _ := f.newCatalog(t, 535, "default", nil, nil)
			_, _ = cache.Put(f.creds[535].Certificate.Certificate, f.now)
			var records []SignedRecord
			for i := range 9 {
				spec := testSpec()
				spec.ServiceID = fmt.Sprintf("http-%d", i)
				w, err := origin.Publish(spec, f.now)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, w)
			}
			fp := records[0].Fingerprint
			r, _ := NewRuntime(viewer, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
			run := func(first bool) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				left, right := net.Pipe()
				defer left.Close()
				defer right.Close()
				_ = right.SetDeadline(time.Now().Add(5 * time.Second))
				done := make(chan struct{})
				go func() { defer close(done); r.sessionInventory(ctx, 535, left) }()
				inventoryDone := make(chan []string, 1)
				readerDone := make(chan struct{})
				go func() {
					defer close(readerDone)
					var fps []string
					for {
						m, err := readInventoryMessage(right)
						if err != nil {
							return
						}
						if m.Kind == "identity-cache" {
							for i := 0; i < len(m.Fingerprints); i += 32 {
								fps = append(fps, hex.EncodeToString(m.Fingerprints[i:i+32]))
							}
						}
						if m.Kind == "identity-cache-done" {
							inventoryDone <- fps
						}
					}
				}()
				var fps []string
				select {
				case fps = <-inventoryDone:
				case <-time.After(3 * time.Second):
					t.Fatal("inventory stalled")
				}
				if first {
					found := false
					for _, p := range fps {
						found = found || p == fp
					}
					if !found {
						t.Fatal("initial inventory missing seeded identity")
					}
					_, _, _ = cache.Get(fp, f.now.Add(25*time.Hour))
				} else if len(fps) != 0 {
					t.Fatal("cold fallback advertised identities")
				}
				if err := writeInventoryMessage(right, inventoryMessage{Message: Message{Kind: "identity-cache-done"}}); err != nil {
					t.Fatal(err)
				}
				if !first && succeed {
					if err := writeInventoryMessage(right, inventoryMessage{Message: Message{Kind: "bundle", Bundle: f.creds[535].Certificate.Certificate}}); err != nil {
						t.Fatal(err)
					}
				}
				for _, w := range records {
					w := w
					if err := writeInventoryMessage(right, inventoryMessage{Message: Message{Kind: "record", Record: &w}}); err != nil {
						t.Fatal(err)
					}
				}
				if !first && succeed {
					if err := writeInventoryMessage(right, inventoryMessage{Message: Message{Kind: "snapshot-done"}}); err != nil {
						t.Fatal(err)
					}
					waitCatalog(t, viewer, 9)
					deadline := time.Now().Add(time.Second)
					for time.Now().Before(deadline) {
						r.mu.Lock()
						ready := r.peers[535] != nil && r.peers[535].ready
						r.mu.Unlock()
						if ready {
							break
						}
						time.Sleep(time.Millisecond)
					}
					if cold, ok := r.inventoryAttempt(535, time.Now()); cold || !ok {
						t.Fatal("successful cold repair retained fallback")
					}
					cancel()
				}
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("failed repair session did not exit")
				}
				_ = right.Close()
				select {
				case <-readerDone:
				case <-time.After(time.Second):
					t.Fatal("reader leaked")
				}
			}
			run(true)
			if cold, ok := r.inventoryAttempt(535, time.Now()); !cold || !ok {
				t.Fatal("eviction did not schedule cold fallback")
			}
			run(false)
			if !succeed {
				if _, ok := r.inventoryAttempt(535, time.Now()); ok {
					t.Fatal("second failed snapshot allowed another immediate retry")
				}
				if len(viewer.Snapshot(time.Now())) != 0 {
					t.Fatal("failed identities projected")
				}
			}
		})
	}
}

func TestInventoryCompletionWaitsForIdentityRepair(t *testing.T) {
	f := newFixture(t)
	viewer, _ := f.newCatalog(t, 533, "default", nil, nil)
	origin, _ := f.newCatalog(t, 535, "default", nil, nil)
	w, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewRuntime(viewer, func() localmesh.NodeSnapshot { return bridgeRouteView(t, f.now, 535) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	_ = right.SetDeadline(time.Now().Add(4 * time.Second))
	done := make(chan struct{})
	go func() { defer close(done); r.sessionInventory(ctx, 535, left) }()
	inventoryDone, request := make(chan struct{}, 1), make(chan struct{}, 1)
	go func() {
		for {
			m, e := readInventoryMessage(right)
			if e != nil {
				return
			}
			if m.Kind == "identity-cache-done" {
				inventoryDone <- struct{}{}
			}
			if m.Kind == "identity-request" {
				request <- struct{}{}
			}
		}
	}()
	select {
	case <-inventoryDone:
	case <-time.After(time.Second):
		t.Fatal("inventory not sent")
	}
	write := func(m Message) {
		if err := writeInventoryMessage(right, inventoryMessage{Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	write(Message{Kind: "identity-cache-done"})
	if r.ProjectionReady(535) {
		t.Fatal("inventory completion marked route ready")
	}
	write(Message{Kind: "record", Record: &w})
	write(Message{Kind: "snapshot-done"})
	select {
	case <-request:
	case <-time.After(time.Second):
		t.Fatal("identity repair not requested")
	}
	if r.ProjectionReady(535) {
		t.Fatal("unverified completed snapshot marked ready")
	}
	write(Message{Kind: "bundle", Bundle: f.creds[535].Certificate.Certificate})
	waitCatalog(t, viewer, 1)
	deadline := time.Now().Add(time.Second)
	for !r.ProjectionReady(535) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !r.ProjectionReady(535) {
		t.Fatal("verified repair did not complete snapshot")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session leaked")
	}
}

func TestInventoryRuntimeExpiredServiceDoesNotReappearOnHeal(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(-3 * time.Second)
	viewer, cache := f.newCatalog(t, 533, "default", nil, nil)
	origin, _ := f.newCatalog(t, 535, "default", nil, nil)
	_, _ = cache.Put(f.creds[535].Certificate.Certificate, f.now)
	spec := testSpec()
	spec.Lease = time.Second
	w, err := origin.Publish(spec, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = viewer.Accept(w, f.now); err != nil {
		t.Fatal(err)
	}
	view := bridgeRouteView(t, time.Now(), 535)
	a, _ := NewRuntime(viewer, func() localmesh.NodeSnapshot { return view })
	b, _ := NewRuntime(origin, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, r := net.Pipe()
	defer l.Close()
	defer r.Close()
	done := make(chan struct{}, 2)
	go func() { a.sessionInventory(ctx, 535, l); done <- struct{}{} }()
	go func() { b.sessionInventory(ctx, 533, r); done <- struct{}{} }()
	deadline := time.Now().Add(3 * time.Second)
	for !a.ProjectionReady(535) && time.Now().Before(deadline) {
		if len(viewer.Snapshot(time.Now())) != 0 {
			t.Fatal("expired origin visible during heal")
		}
		time.Sleep(time.Millisecond)
	}
	if !a.ProjectionReady(535) || len(viewer.Snapshot(time.Now())) != 0 {
		t.Fatal("expired origin snapshot incorrectly healed")
	}
	cancel()
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("session leaked")
		}
	}
}

func TestInventoryFailedColdHandshakeConsumesRetry(t *testing.T) {
	f := newFixture(t)
	c, _ := f.newCatalog(t, 533, "default", nil, nil)
	r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	r.inventoryFailed(535, false, time.Now())
	l, remote := net.Pipe()
	defer l.Close()
	defer remote.Close()
	done := make(chan struct{})
	go func() { defer close(done); r.sessionInventory(context.Background(), 535, l) }()
	// Read the empty retry inventory, then disconnect before any peer inventory.
	m, err := readInventoryMessage(remote)
	if err != nil || m.Kind != "identity-cache-done" {
		t.Fatal("cold inventory", m.Kind, err)
	}
	_ = remote.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cold session leaked")
	}
	if _, allowed := r.inventoryAttempt(535, time.Now()); allowed {
		t.Fatal("failed cold handshake retained another immediate retry")
	}
}

type inventoryCountConn struct {
	net.Conn
	bytes atomic.Int64
}

func (c *inventoryCountConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.bytes.Add(int64(n))
	return n, err
}
func TestInventoryRuntimeIdleReconcileEmitsNoWire(t *testing.T) {
	f := newFixture(t)
	a, _ := f.newCatalog(t, 533, "default", nil, nil)
	b, _ := f.newCatalog(t, 535, "default", nil, nil)
	if _, err := a.Publish(testSpec(), f.now); err != nil {
		t.Fatal(err)
	}
	av, bv := bridgeRouteView(t, f.now, 535), bridgeRouteView(t, f.now, 533)
	ar, _ := NewRuntime(a, func() localmesh.NodeSnapshot { return av })
	br, _ := NewRuntime(b, func() localmesh.NodeSnapshot { return bv })
	left, right := net.Pipe()
	l, r := &inventoryCountConn{Conn: left}, &inventoryCountConn{Conn: right}
	defer l.Close()
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 2)
	go func() { ar.sessionInventory(ctx, 535, l); done <- struct{}{} }()
	go func() { br.sessionInventory(ctx, 533, r); done <- struct{}{} }()
	deadline := time.Now().Add(3 * time.Second)
	for (!ar.ProjectionReady(535) || !br.ProjectionReady(533)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !ar.ProjectionReady(535) || !br.ProjectionReady(533) {
		t.Fatal("initial snapshot not ready")
	}
	time.Sleep(100 * time.Millisecond)
	before := l.bytes.Load() + r.bytes.Load()
	time.Sleep(17 * time.Second)
	if after := l.bytes.Load() + r.bytes.Load(); after != before {
		t.Fatalf("idle reconciliation emitted %d bytes", after-before)
	}
	cancel()
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("idle session leaked")
		}
	}
}
