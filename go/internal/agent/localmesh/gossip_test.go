package localmesh

import (
	"bytes"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func TestManifestHaveWantDiamondTransfersEachRevisionOnce(t *testing.T) {
	source, chain, key, device, now := directoryFixture(t)
	dirs := []*Directory{source}
	pools := []*netgossip.Pool{netgossip.NewPool()}
	for range 3 {
		cache, err := OpenIdentityCache("", DefaultCacheLimits(), source.cache.verify, now)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := NewDirectory(testTenant(t), 10, cache)
		dirs = append(dirs, d)
		pools = append(pools, netgossip.NewPool())
	}
	edges := [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {1, 2}}
	sessions := make(map[[2]int]*Synchronizer)
	for _, e := range edges {
		for _, ends := range [][2]int{e, {e[1], e[0]}} {
			s := NewSynchronizer(dirs[ends[0]], dirs[ends[0]].cache, pools[ends[0]])
			sessions[ends] = s
			defer s.Close()
		}
	}
	type delivery struct {
		from, to int
		m        ControlMessage
	}
	for revision := uint64(1); revision <= 3; revision++ {
		m := routeManifest(device, now)
		m.Revision = revision
		m.Withdraw = revision == 3
		w, err := SignManifest(m, chain, key, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = source.Accept(w, now); err != nil {
			t.Fatal(err)
		}
		var queue []delivery
		for edge, s := range sessions {
			if edge[0] == 0 {
				for _, m := range s.Record(w, now) {
					if m.Kind != "gossip" || m.Gossip.Op != "have" {
						t.Fatal("publication pushed a full manifest")
					}
					queue = append(queue, delivery{edge[0], edge[1], m})
				}
			}
		}
		full := make(map[int]int)
		for round := 0; round < 50; round++ {
			for len(queue) > 0 {
				d := queue[0]
				queue = queue[1:]
				var wire bytes.Buffer
				if err := WriteControl(&wire, d.m); err != nil {
					t.Fatal(err)
				}
				decoded, err := ReadControl(&wire)
				if err != nil {
					t.Fatal(err)
				}
				if decoded.Kind == "gossip" && decoded.Gossip.Op == "data" && decoded.Gossip.Offset+len(decoded.Gossip.Data) == decoded.Gossip.Total {
					full[d.to]++
				}
				replies, changed, err := sessions[[2]int{d.to, d.from}].Receive(decoded, now)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range replies {
					queue = append(queue, delivery{d.to, d.from, m})
				}
				for _, w := range changed {
					for edge, s := range sessions {
						if edge[0] == d.to && edge[1] != d.from {
							for _, m := range s.Record(w, now) {
								queue = append(queue, delivery{edge[0], edge[1], m})
							}
						}
					}
				}
			}
			for edge, s := range sessions {
				replies, changed, err := s.ContinueCertificates(now)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range replies {
					queue = append(queue, delivery{edge[0], edge[1], m})
				}
				for _, w := range changed {
					for other, forward := range sessions {
						if other[0] == edge[0] && other[1] != edge[1] {
							for _, m := range forward.Record(w, now) {
								queue = append(queue, delivery{other[0], other[1], m})
							}
						}
					}
				}
			}
			if len(queue) == 0 {
				break
			}
			if round == 49 {
				t.Fatal("protocol did not quiesce")
			}
		}
		for node := 1; node < 4; node++ {
			if full[node] != 1 {
				t.Fatalf("revision %d node %d received %d full manifests", revision, node, full[node])
			}
			records := dirs[node].Records(now)
			if len(records) != 1 {
				t.Fatal("manifest missing", node)
			}
			got, err := DecodeManifest(records[0].Body)
			if err != nil {
				t.Fatal(err)
			}
			if got.Revision != revision || got.Withdraw != m.Withdraw {
				t.Fatal("wrong revision/withdrawal", got)
			}
		}
		// Reconnect/periodic repair sends HAVE again, not duplicate DATA.
		for _, edge := range [][2]int{{1, 3}, {2, 3}} {
			for _, m := range sessions[edge].Reconcile(now) {
				replies, _, err := sessions[[2]int{edge[1], edge[0]}].Receive(m, now)
				if err != nil || len(replies) > 0 {
					t.Fatal("known revision redownloaded", replies, err)
				}
			}
		}
	}
}

func TestManifestGossipQueueBudgetDoesNotConsumeAnnouncement(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, _ := SignManifest(routeManifest(device, now), chain, key, now)
	d.Accept(w, now)
	s := NewSynchronizer(d, d.cache, netgossip.NewPool())
	defer s.Close()
	if got := s.ReconcileBounded(now, 0); len(got) != 0 {
		t.Fatal(got)
	}
	got := s.ContinueReconcile(now, 1)
	if len(got) != 1 || got[0].Gossip == nil || got[0].Gossip.Op != "have" {
		t.Fatal("dropped unqueued announcement", got)
	}
}

func TestManifestGossipExpiredUnverifiedBodyDoesNotCreateReceipt(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	// Expiry is rejected before authentication. Even this bad signature must not
	// become a shared receipt or prevent an independent supplier from being tried.
	w.Signature[0] ^= 1
	body, _ := netwire.Marshal(w)
	later := now.Add(MaxLease)
	source := netgossip.NewPool().NewSession(netgossip.Manifest)
	have := source.Offer(body, later.Add(MaxLease), later)
	pool := netgossip.NewPool()
	receiver := NewSynchronizer(d, d.cache, pool)
	defer receiver.Close()
	want, _, err := receiver.Receive(ControlMessage{Kind: "gossip", Gossip: have}, later)
	if err != nil || len(want) != 1 {
		t.Fatal(want, err)
	}
	data, _, err := source.Receive(*want[0].Gossip, later)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := receiver.Receive(ControlMessage{Kind: "gossip", Gossip: data}, later); err != nil || len(changed) != 0 {
		t.Fatal(changed, err)
	}
	alternate := pool.NewSession(netgossip.Manifest)
	defer alternate.Close()
	if next, _, err := alternate.Receive(*have, later); err != nil || next == nil {
		t.Fatal("unverified expired manifest became a shared receipt", next, err)
	}
	if len(d.Records(later)) != 0 {
		t.Fatal("unverified artifact entered the directory")
	}
}

func TestManifestGossipRecoversEvictedPayloadAndRechecksTrust(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	pool := netgossip.NewPool()
	source := NewSynchronizer(d, d.cache, pool)
	defer source.Close()
	have := source.Record(w, now)[0]
	// Fill the shared byte cache with newer objects, preserving the old receipt.
	filler := pool.NewSession(netgossip.Service)
	defer filler.Close()
	for i := range 220 {
		b := bytes.Repeat([]byte{byte(i)}, netgossip.MaxObject)
		filler.Offer(b, now.Add(time.Minute), now.Add(time.Duration(i+1)*time.Millisecond))
	}
	request, _, err := netgossip.NewPool().NewSession(netgossip.Manifest).Receive(*have.Gossip, now)
	if err != nil || request == nil {
		t.Fatal(request, err)
	}
	replies, _, err := source.Receive(ControlMessage{Kind: "gossip", Gossip: request}, now)
	if err != nil || len(replies) != 2 || replies[0].Certificate == nil || replies[1].Gossip == nil || replies[1].Gossip.Op != "data" {
		t.Fatal("evicted body was not recovered from its directory key", replies, err)
	}
	body, _ := netwire.Marshal(w)
	if !bytes.Equal(replies[1].Gossip.Data, body) {
		t.Fatal("recovery changed the signed envelope")
	}
	d.cache.verify = func([][]byte, time.Time) (Identity, error) { return Identity{}, errors.New("trust removed") }
	request.Request++
	replies, _, err = source.Receive(ControlMessage{Kind: "gossip", Gossip: request}, now)
	if err != nil || len(replies) != 1 || replies[0].Gossip.Op != "unavailable" {
		t.Fatal("recovered bytes bypassed current trust", replies, err)
	}
}
