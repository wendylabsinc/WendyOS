package meshcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type testEdge struct {
	a, b int
	id   babel.LinkID
}
type neighbourNetwork struct {
	observe  func(from, to int, fragment []byte)
	mu       sync.RWMutex
	t        *testing.T
	catalogs []*Catalog
	runtimes []*Runtime
	hubs     []*localmesh.ControlHub
	edges    map[babel.LinkID]testEdge
	next     babel.LinkID
	done     chan error
	cancel   context.CancelFunc
}

func newNeighbourNetwork(t *testing.T, count int) *neighbourNetwork {
	t.Helper()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	n := &neighbourNetwork{t: t, edges: make(map[babel.LinkID]testEdge), cancel: cancel, done: make(chan error, count)}
	for i, p := range []string{p533, p534, p535, p536}[:count] {
		c, _ := f.newCatalog(t, p, "default", nil, nil)
		n.catalogs = append(n.catalogs, c)
		hub := localmesh.NewControlHub()
		n.hubs = append(n.hubs, hub)
		r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return n.view(i) })
		n.runtimes = append(n.runtimes, r)
	}
	for i, r := range n.runtimes {
		go func() { n.done <- r.Run(ctx, n.hubs[i]) }()
	}
	t.Cleanup(func() {
		cancel()
		for range count {
			select {
			case err := <-n.done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("catalogue worker did not stop")
			}
		}
	})
	return n
}
func (n *neighbourNetwork) view(self int) localmesh.NodeSnapshot {
	n.mu.RLock()
	defer n.mu.RUnlock()
	v := localmesh.NodeSnapshot{}
	type visit struct {
		node  int
		first babel.LinkID
	}
	seen := map[int]bool{self: true}
	q := []visit{{self, 0}}
	for len(q) > 0 {
		x := q[0]
		q = q[1:]
		for id, e := range n.edges {
			other := -1
			if e.a == x.node {
				other = e.b
			}
			if e.b == x.node {
				other = e.a
			}
			if other < 0 {
				continue
			}
			if x.node == self {
				v.Links = append(v.Links, localmesh.PeerLink{Device: n.catalogs[other].device})
			}
			if seen[other] {
				continue
			}
			seen[other] = true
			first := x.first
			if first == 0 {
				first = id
			}
			q = append(q, visit{other, first})
			d := n.catalogs[other].device
			v.Devices = append(v.Devices, localmesh.Manifest{Device: d.UUID(), Expires: time.Now().Add(time.Minute).UnixMilli()})
			v.Routes = append(v.Routes, babel.Route{Prefix: netip.PrefixFrom(d.MeshAddress(), 128), Link: first})
		}
	}
	return v
}
func (n *neighbourNetwork) connect(a, b int) babel.LinkID {
	n.mu.Lock()
	n.next++
	id := n.next
	n.edges[id] = testEdge{a, b, id}
	n.mu.Unlock()
	// A real reliable transport buffers the first write while its remote reader
	// starts. Do not deliver through this in-memory link until both ends exist.
	ready := make(chan struct{})
	for _, ends := range [][2]int{{a, b}, {b, a}} {
		from, to := ends[0], ends[1]
		n.hubs[from].Add(localmesh.ControlPeer{ID: id, Device: n.catalogs[to].device}, func(ctx context.Context, data []byte) error {
			select {
			case <-ready:
			case <-ctx.Done():
				return ctx.Err()
			}
			n.mu.RLock()
			_, live := n.edges[id]
			n.mu.RUnlock()
			if !live {
				return errors.New("link down")
			}
			if n.observe != nil {
				n.observe(from, to, data)
			}
			// Exercise actual multiplexed control framing, not a record shortcut.
			var wire bytes.Buffer
			if err := localmesh.WriteControl(&wire, localmesh.ControlMessage{Kind: "catalog", Catalog: data}); err != nil {
				return err
			}
			m, err := localmesh.ReadControl(&wire)
			if err != nil {
				return err
			}
			return n.hubs[to].Deliver(id, m.Catalog)
		}, func() { n.disconnect(id) })
	}
	close(ready)
	return id
}
func (n *neighbourNetwork) disconnect(id babel.LinkID) {
	n.mu.Lock()
	e, ok := n.edges[id]
	delete(n.edges, id)
	n.mu.Unlock()
	if ok {
		n.hubs[e.a].Remove(id)
		n.hubs[e.b].Remove(id)
	}
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not converge")
}
func TestNeighbourCatalogueFourNodeChain(t *testing.T) {
	n := newNeighbourNetwork(t, 4)
	n.connect(0, 1)
	n.connect(1, 2)
	n.connect(2, 3)
	w, err := n.catalogs[0].Publish(testSpec(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.runtimes[0].Broadcast(w)
	eventually(t, func() bool {
		return len(n.catalogs[3].Snapshot(time.Now())) == 1 && n.runtimes[3].ProjectionReady(n.catalogs[0].device)
	})
	if peers := n.hubs[3].Peers(); len(peers) != 1 || peers[0].Device != n.catalogs[2].device {
		t.Fatal("catalogue opened a non-neighbour connection", peers)
	}
	tombstone, err := n.catalogs[0].Remove(testSpec().AppID, testSpec().ServiceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.runtimes[0].Broadcast(tombstone)
	eventually(t, func() bool { return len(n.catalogs[3].Snapshot(time.Now())) == 0 })
}
func TestNeighbourPartitionFreshnessAndWithdrawal(t *testing.T) {
	n := newNeighbourNetwork(t, 3)
	n.connect(0, 1)
	cut := n.connect(1, 2)
	w, err := n.catalogs[0].Publish(testSpec(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.runtimes[0].Broadcast(w)
	eventually(t, func() bool {
		return n.runtimes[2].ProjectionReady(n.catalogs[0].device) && len(n.catalogs[2].Snapshot(time.Now())) == 1
	})
	n.disconnect(cut)
	if n.runtimes[2].ProjectionReady(n.catalogs[0].device) {
		t.Fatal("partition retained projection readiness")
	}
	tombstone, err := n.catalogs[0].Remove(testSpec().AppID, testSpec().ServiceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.runtimes[0].Broadcast(tombstone)
	eventually(t, func() bool { return len(n.catalogs[1].Snapshot(time.Now())) == 0 })
	n.connect(1, 2)
	if n.runtimes[2].ProjectionReady(n.catalogs[0].device) {
		t.Fatal("route heal alone restored readiness")
	}
	eventually(t, func() bool {
		return n.runtimes[2].ProjectionReady(n.catalogs[0].device) && len(n.catalogs[2].Snapshot(time.Now())) == 0
	})
}
func TestNeighbourTriangleConvergesAndStops(t *testing.T) {
	n := newNeighbourNetwork(t, 3)
	n.connect(0, 1)
	n.connect(1, 2)
	n.connect(0, 2)
	w, err := n.catalogs[0].Publish(testSpec(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.runtimes[0].Broadcast(w)
	eventually(t, func() bool {
		return len(n.catalogs[1].Snapshot(time.Now())) == 1 && len(n.catalogs[2].Snapshot(time.Now())) == 1
	})
	// Each synchronizer suppresses unchanged repair passes after initial delivery.
	s := NewSynchronizer(n.catalogs[1], n.catalogs[1].cache)
	s.Reconcile(time.Now())
	if len(s.Reconcile(time.Now())) != 0 {
		t.Fatal("unchanged record resent")
	}
}
func TestNeighbourQueuePressureDoesNotCommitSendStamp(t *testing.T) {
	f := newFixture(t)
	c, cache := f.newCatalog(t, p533, "default", nil, nil)
	r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	w, err := c.Publish(testSpec(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := &catalogPeer{syncer: NewSynchronizer(c, cache), out: make(chan []byte, 2)}
	p.out <- []byte("full")
	p.out <- []byte("full")
	if r.sendRecord(p, w) {
		t.Fatal("full queue accepted record")
	}
	if len(p.syncer.sentRecords) != 0 || len(p.syncer.sent) != 0 {
		t.Fatal("failed enqueue consumed record or certificate")
	}
	<-p.out
	<-p.out
	if !r.sendRecord(p, w) || len(p.out) != 2 {
		t.Fatal("repair did not queue certificate and record")
	}
	if !r.sendRecord(p, w) || len(p.out) != 2 {
		t.Fatal("duplicate transmission queued")
	}
}
func TestOriginInventoryBindsNonceAndFencesWithdrawnGeneration(t *testing.T) {
	f := newFixture(t)
	a, _ := f.newCatalog(t, p533, "default", nil, nil)
	b, _ := f.newCatalog(t, p535, "default", nil, nil)
	w, err := a.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	q := FreshRequest{Origin: p533, Requester: p535, Nonce: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Hops: 32}
	if _, err := a.Remove(testSpec().AppID, testSpec().ServiceID, f.now); err != nil {
		t.Fatal(err)
	}
	data, err := a.signInventory(q, f.now)
	if err != nil {
		t.Fatal(err)
	}
	changed := q
	changed.Nonce = "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if b.acceptInventory(data, changed, f.now) == nil {
		t.Fatal("replayed response accepted for new challenge")
	}
	if err := b.acceptInventory(data, q, f.now); !errors.Is(err, ErrIdentityNeeded) {
		t.Fatal("uncached proof must request certificate elements", err)
	}
	synchronizeServiceIdentity(t, a, b, f.now)
	if err := b.acceptInventory(data, q, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Accept(w, f.now); !errors.Is(err, ErrStale) {
		t.Fatal("old service resurrected", err)
	}
	var signed signedInventory
	if err := json.Unmarshal(data, &signed); err != nil {
		t.Fatal(err)
	}
	signed.Signature[0] ^= 1
	tampered, _ := json.Marshal(signed)
	if b.acceptInventory(tampered, q, f.now) == nil {
		t.Fatal("tampered inventory accepted")
	}
}

func TestOriginInventoryPersistenceFailureAndConcurrentPublication(t *testing.T) {
	f := newFixture(t)
	a, _ := f.newCatalog(t, p533, "default", nil, nil)
	b, _ := f.newCatalog(t, p535, "default", nil, nil)
	q := FreshRequest{Origin: p533, Requester: p535, Nonce: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Hops: 32}
	old, err := a.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Remove(testSpec().AppID, testSpec().ServiceID, f.now); err != nil {
		t.Fatal(err)
	}
	data, err := a.signInventory(q, f.now)
	if err != nil {
		t.Fatal(err)
	}
	synchronizeServiceIdentity(t, a, b, f.now)
	b.persist = func([]Receipt) error { return errors.New("disk failed") }
	if b.acceptInventory(data, q, f.now) == nil {
		t.Fatal("persistence failure accepted")
	}
	if len(b.history) != 0 || len(b.unavailable) != 0 {
		t.Fatal("failed persistence changed safety state")
	}
	b.persist = func([]Receipt) error { return nil }
	if _, err := b.cache.Put(f.creds[p533].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	// A new key published while the snapshot is in transit must survive it.
	spec := testSpec()
	spec.ServiceID = "new-service"
	newer, err := a.Publish(spec, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Accept(newer, f.now); err != nil {
		t.Fatal(err)
	}
	if err := b.acceptInventory(data, q, f.now); err != nil {
		t.Fatal(err)
	}
	if len(b.Snapshot(f.now)) != 1 {
		t.Fatal("snapshot erased concurrent new service")
	}
	if _, err := b.Accept(old, f.now); !errors.Is(err, ErrStale) {
		t.Fatal("withdrawn generation resurrected", err)
	}
	// The actual signed withdrawal must remain gossipable even if its fence arrived first.
	for _, record := range a.Records(f.now) {
		var body Record
		_ = json.Unmarshal(record.Body, &body)
		if body.Withdraw {
			changed, err := b.Accept(record, f.now)
			if err != nil || !changed {
				t.Fatal("withdrawal body blocked by inventory fence", changed, err)
			}
		}
	}
	if b.acceptInventory(data, q, f.now.Add(freshLifetime)) == nil {
		t.Fatal("expired freshness evidence accepted")
	}
}

func TestIdentityRepairFromAlternateNeighbour(t *testing.T) {
	f := newFixture(t)
	origin, _ := f.newCatalog(t, p533, "default", nil, nil)
	relay, cache := f.newCatalog(t, p534, "default", nil, nil)
	w, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewRuntime(relay, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	for id, principal := range map[babel.LinkID]string{1: p533, 2: p535} {
		r.peers[id] = &catalogPeer{id: id, device: principalOf(t, principal), syncer: NewSynchronizer(relay, cache), out: make(chan []byte, 16), inventory: receivedInventory{done: true}}
	}
	data, _ := inventoryWire(inventoryMessage{Message: Message{Kind: "record", Record: &w}})
	if err := r.receive(r.peers[2], data); err != nil {
		t.Fatal(err)
	}
	if len(relay.Snapshot(time.Now())) != 0 {
		t.Fatal("unverified service admitted")
	}
	if len(r.peers[1].out) == 0 {
		t.Fatal("missing certificate was not requested from alternate neighbour")
	}
	source := NewSynchronizer(origin, origin.cache)
	for steps := 0; len(r.peers[1].out) > 0; steps++ {
		if steps > 1000 {
			t.Fatal("alternate neighbour repair did not finish")
		}
		var request inventoryMessage
		if err := json.Unmarshal(<-r.peers[1].out, &request); err != nil {
			t.Fatal(err)
		}
		replies, _, err := source.Receive(request.Message, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, reply := range replies {
			wire, err := inventoryWire(inventoryMessage{Message: reply})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.receive(r.peers[1], wire); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(relay.Snapshot(time.Now())) != 1 || r.peers[2].syncer.PendingIdentity() {
		t.Fatalf("alternate neighbour bundle failed: records=%v pending=%v cache=%v", relay.Snapshot(time.Now()), r.peers[2].syncer.pending, cache.Fingerprints(time.Now(), 10))
	}
}
