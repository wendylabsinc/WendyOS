package meshcatalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func TestServiceHaveWantDiamondOneFullTransferIncludingWithdrawal(t *testing.T) {
	n := newNeighbourNetwork(t, 4)
	var mu sync.Mutex
	partial := map[[2]int][]byte{}
	full := map[int]map[netgossip.ID]int{}
	legacy := 0
	n.observe = func(from, to int, fragment []byte) {
		mu.Lock()
		defer mu.Unlock()
		pair := [2]int{from, to}
		total, offset := int(binary.BigEndian.Uint32(fragment)), int(binary.BigEndian.Uint32(fragment[4:]))
		if offset == 0 {
			partial[pair] = nil
		}
		partial[pair] = append(partial[pair], fragment[8:]...)
		if len(partial[pair]) != total {
			return
		}
		var m inventoryMessage
		if err := json.Unmarshal(partial[pair], &m); err != nil {
			t.Error(err)
			return
		}
		delete(partial, pair)
		if m.Kind == "record" {
			legacy++
		}
		if m.Gossip != nil && m.Gossip.Op == "data" && m.Gossip.Offset+len(m.Gossip.Data) == m.Gossip.Total {
			if full[to] == nil {
				full[to] = make(map[netgossip.ID]int)
			}
			full[to][m.Gossip.ID()]++
		}
	}
	n.connect(0, 1)
	n.connect(0, 2)
	n.connect(1, 3)
	n.connect(2, 3)
	n.connect(1, 2)
	for step := 0; step < 3; step++ {
		var w SignedRecord
		var err error
		if step == 2 {
			w, err = n.catalogs[0].Remove(testSpec().AppID, testSpec().ServiceID, time.Now())
		} else {
			w, err = n.catalogs[0].Publish(testSpec(), time.Now())
		}
		if err != nil {
			t.Fatal(err)
		}
		n.runtimes[0].Broadcast(w)
		body, _ := json.Marshal(w)
		id := netgossip.Identify(netgossip.Service, body)
		eventually(t, func() bool {
			for node := 1; node < 4; node++ {
				found := false
				for _, record := range n.catalogs[node].Records(time.Now()) {
					b, _ := json.Marshal(record)
					if netgossip.Identify(netgossip.Service, b) == id {
						found = true
					}
				}
				if !found {
					return false
				}
			}
			return true
		})
		// Force an explicit duplicate advertisement from every origin/relay;
		// the periodic repair path uses this same HAVE-only send operation.
		for node := 0; node < 4; node++ {
			n.runtimes[node].Broadcast(w)
		}
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		for node := 1; node < 4; node++ {
			if full[node][id] != 1 {
				t.Errorf("step %d node %d received %d full objects", step, node, full[node][id])
			}
		}
		if legacy != 0 {
			t.Error("unsolicited full service records", legacy)
		}
		mu.Unlock()
	}
	if len(n.catalogs[3].Snapshot(time.Now())) != 0 {
		t.Fatal("withdrawal did not remove service")
	}
}

func TestServiceSnapshotBarrierWaitsForSharedVerification(t *testing.T) {
	f := newFixture(t)
	source, cache := f.newCatalog(t, p533, "default", nil, nil)
	dest, destCache := f.newCatalog(t, p534, "default", nil, nil)
	w, err := source.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(source, cache, netgossip.NewPool())
	pool := netgossip.NewPool()
	a, b := NewSynchronizer(dest, destCache, pool), NewSynchronizer(dest, destCache, pool)
	defer sender.Close()
	defer a.Close()
	defer b.Close()
	have := sender.Record(w, f.now)[0]
	request, _, err := a.Receive(have, f.now)
	if err != nil || len(request) != 1 {
		t.Fatal(request, err)
	}
	if duplicate, _, err := b.Receive(have, f.now); err != nil || len(duplicate) != 0 {
		t.Fatal("duplicate service fetch", duplicate, err)
	}
	if !b.PendingAnnouncements(f.now) {
		t.Fatal("snapshot complete before DATA")
	}
	data, _, err := sender.Receive(request[0], f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 2 || data[0].Certificate == nil || data[1].Gossip == nil {
		t.Fatal("missing descriptor before DATA", data)
	}
	identity, changed, err := a.Receive(data[0], f.now)
	if err != nil || len(changed) != 0 || len(identity) != 1 {
		t.Fatal(identity, changed, err)
	}
	if out, changed, err := a.Receive(data[1], f.now); err != nil || len(out) != 0 || len(changed) != 0 {
		t.Fatal("DATA bypassed pending identity", out, changed, err)
	}
	if !b.PendingAnnouncements(f.now) {
		t.Fatal("snapshot complete before signature verification")
	}
	description, _, err := sender.Receive(identity[0], f.now)
	if err != nil {
		t.Fatal(err)
	}
	changed = exchangeServiceMessages(t, sender, a, description, f.now)
	if len(changed) != 1 || b.PendingAnnouncements(f.now) {
		t.Fatal("shared verification did not finish alternate snapshot")
	}
	if _, _, err := b.Receive(Message{Kind: "record", Record: &w}, f.now); err == nil {
		t.Fatal("accepted unsolicited full service record")
	}
}

func TestServiceGossipExpiredUnverifiedBodyDoesNotCreateReceipt(t *testing.T) {
	f := newFixture(t)
	origin, _ := f.newCatalog(t, p533, "default", nil, nil)
	dest, cache := f.newCatalog(t, p534, "default", nil, nil)
	w, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	w.Signature[0] ^= 1
	body, _ := json.Marshal(w)
	now := f.now.Add(MaxLease)
	source := netgossip.NewPool().NewSession(netgossip.Service)
	have := source.Offer(body, now.Add(MaxLease), now)
	pool := netgossip.NewPool()
	receiver := NewSynchronizer(dest, cache, pool)
	defer receiver.Close()
	want, _, err := receiver.Receive(Message{Kind: "gossip", Gossip: have}, now)
	if err != nil || len(want) != 1 {
		t.Fatal(want, err)
	}
	data, _, err := source.Receive(*want[0].Gossip, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := receiver.Receive(Message{Kind: "gossip", Gossip: data}, now); err != nil || len(changed) != 0 {
		t.Fatal(changed, err)
	}
	alternate := pool.NewSession(netgossip.Service)
	defer alternate.Close()
	if next, _, err := alternate.Receive(*have, now); err != nil || next == nil {
		t.Fatal("unverified expired service became a shared receipt", next, err)
	}
	if len(dest.Records(now)) != 0 {
		t.Fatal("unverified artifact entered the catalogue")
	}
}

func TestFreshServiceRemainsReadyWhileNewNeighbourStalls(t *testing.T) {
	n := newNeighbourNetwork(t, 3)
	n.connect(0, 1)
	w, err := n.catalogs[0].Publish(testSpec(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.runtimes[0].Broadcast(w)
	eventually(t, func() bool {
		return len(n.catalogs[1].Snapshot(time.Now())) == 1 && n.runtimes[1].ProjectionReady(n.catalogs[0].device)
	})
	// This authenticated newcomer accepts writes but never supplies an inventory,
	// snapshot or freshness response. Established origins must remain available.
	started := make(chan struct{}, 1)
	n.hubs[1].Add(localmesh.ControlPeer{ID: 99, Device: n.catalogs[2].device}, func(context.Context, []byte) error {
		select {
		case started <- struct{}{}:
		default:
		}
		return nil
	}, func() { n.hubs[1].Remove(99) })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("newcomer did not start synchronization")
	}
	until := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(until) {
		if !n.runtimes[1].ProjectionReady(n.catalogs[0].device) {
			t.Fatal("stalled newcomer hid a freshness-proven origin")
		}
		if n.runtimes[1].ProjectionReady(n.catalogs[2].device) {
			t.Fatal("newcomer became ready without origin proof")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOriginProofBeforeGossipBodyPreservesWithdrawalAndNewPublication(t *testing.T) {
	f := newFixture(t)
	origin, originCache := f.newCatalog(t, p533, "default", nil, nil)
	dest, cache := f.newCatalog(t, p535, "default", nil, nil)
	old, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := origin.Remove(testSpec().AppID, testSpec().ServiceID, f.now); err != nil {
		t.Fatal(err)
	}
	q := FreshRequest{Origin: p533, Requester: p535, Nonce: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Hops: 32}
	proof, err := origin.signInventory(q, f.now)
	if err != nil {
		t.Fatal(err)
	}
	synchronizeServiceIdentity(t, origin, dest, f.now)
	if err := dest.acceptInventory(proof, q, f.now); err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(origin, originCache, netgossip.NewPool())
	receiver := NewSynchronizer(dest, cache, netgossip.NewPool())
	defer sender.Close()
	defer receiver.Close()
	// Deliver the old publish after the origin proof has fenced its generation.
	changed := exchangeServiceMessages(t, sender, receiver, sender.Record(old, f.now), f.now)
	if len(changed) != 0 || len(dest.Snapshot(f.now)) != 0 {
		t.Fatal("late gossip resurrected withdrawn generation")
	}
	spec := testSpec()
	spec.ServiceID = "new-service"
	newer, err := origin.Publish(spec, f.now)
	if err != nil {
		t.Fatal(err)
	}
	changed = exchangeServiceMessages(t, sender, receiver, sender.Record(newer, f.now), f.now)
	if len(changed) != 1 || len(dest.Snapshot(f.now)) != 1 {
		t.Fatal("freshness proof blocked a later publication", len(changed))
	}
}

func TestServiceGossipRecoversEvictedPayloadAndRejectsReplacement(t *testing.T) {
	f := newFixture(t)
	catalog, cache := f.newCatalog(t, p533, "default", nil, nil)
	w, err := catalog.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	pool := netgossip.NewPool()
	source := NewSynchronizer(catalog, cache, pool)
	defer source.Close()
	have := source.Record(w, f.now)[0]
	filler := pool.NewSession(netgossip.Manifest)
	defer filler.Close()
	evict := func(round int) {
		for i := range 220 {
			b := bytes.Repeat([]byte{byte(i)}, netgossip.MaxObject)
			b[0] = byte(round)
			filler.Offer(b, f.now.Add(time.Minute), f.now.Add(time.Duration(round*220+i+1)*time.Millisecond))
		}
	}
	evict(0)
	request, _, err := netgossip.NewPool().NewSession(netgossip.Service).Receive(*have.Gossip, f.now)
	if err != nil || request == nil {
		t.Fatal(request, err)
	}
	replies, _, err := source.Receive(Message{Kind: "gossip", Gossip: request}, f.now)
	if err != nil || len(replies) != 2 || replies[0].Certificate == nil || replies[1].Gossip == nil || replies[1].Gossip.Op != "data" {
		t.Fatal("evicted body was not recovered from its service key", replies, err)
	}
	body, _ := json.Marshal(w)
	if !bytes.Equal(replies[1].Gossip.Data, body) {
		t.Fatal("recovery changed the signed envelope")
	}
	if _, err := catalog.Publish(testSpec(), f.now); err != nil {
		t.Fatal(err)
	}
	evict(1)
	request.Request++
	replies, _, err = source.Receive(Message{Kind: "gossip", Gossip: request}, f.now)
	if err != nil || len(replies) != 1 || replies[0].Gossip.Op != "unavailable" {
		t.Fatal("replacement was served under the old digest", replies, err)
	}
}
