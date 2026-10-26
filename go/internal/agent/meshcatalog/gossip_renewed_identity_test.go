package meshcatalog

import (
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func TestServiceGossipRenewalRejectsPreviousCertificateOffersAndCachedBodies(t *testing.T) {
	old, renewed := newFixture(t), newFixture(t)
	old.now = renewed.now
	allowOld := true
	verifyCurrent := func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
		if id, err := renewed.creds[p534].Verify(chain, now); err == nil {
			return id, nil
		}
		if allowOld {
			return old.creds[p534].Verify(chain, now)
		}
		return localmesh.Identity{}, errors.New("old issuer retired")
	}
	cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), verifyCurrent, old.now)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := NewCatalog("default", old.creds[p534].Device, 32, old.creds[p534], cache,
		func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, old.now)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := old.newCatalog(t, p533, "default", nil, nil)
	wire, err := origin.Publish(testSpec(), old.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(old.creds[p533].Certificate.Certificate, old.now); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Accept(wire, old.now); err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(relay, cache, netgossip.NewPool())
	defer sender.Close()
	have := sender.Record(wire, old.now)
	if len(have) != 1 || have[0].Gossip == nil || have[0].Gossip.Op != "have" {
		t.Fatal("initial current artifact was not advertised")
	}
	requester := netgossip.NewPool().NewSession(netgossip.Service)
	defer requester.Close()
	want, _, err := requester.Receive(*have[0].Gossip, old.now)
	if err != nil || want == nil {
		t.Fatal(want, err)
	}

	// HAVE may already be queued when the principal receives a new trusted leaf.
	// Neither an old Broadcast nor a delayed WANT may borrow that new identity's
	// eligibility to advertise or serve bytes signed by the retired certificate.
	allowOld = false
	if _, err := cache.Put(renewed.creds[p533].Certificate.Certificate, old.now); err != nil {
		t.Fatal(err)
	}
	if got := sender.Record(wire, old.now); len(got) != 0 {
		t.Error("queued old artifact was advertised under renewed trust")
	}
	replies, _, err := sender.Receive(Message{Kind: "gossip", Gossip: want}, old.now)
	if err != nil || len(replies) != 1 || replies[0].Gossip == nil || replies[0].Gossip.Op != "unavailable" {
		t.Errorf("cached old body remained downloadable after renewal: replies=%v err=%v", replies, err)
	}
	key := Key{"default", p533, testSpec().AppID, testSpec().ServiceID}
	if got := relay.servicePayload(key, old.now); len(got) != 0 {
		t.Error("authoritative payload loader recovered an old-certificate artifact")
	}

	currentOrigin, _ := renewed.newCatalog(t, p533, "default", []Receipt{relay.history[key]}, nil)
	current, err := currentOrigin.Publish(testSpec(), old.now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := relay.Accept(current, old.now); err != nil || !changed {
		t.Fatalf("renewed generation did not recover: changed=%v err=%v", changed, err)
	}
	cold, coldCache := renewed.newCatalog(t, p535, "default", nil, nil)
	receiver := NewSynchronizer(cold, coldCache, netgossip.NewPool())
	defer receiver.Close()
	changed := exchangeServiceMessages(t, sender, receiver, sender.Record(current, old.now), old.now)
	if len(changed) != 1 || len(cold.Snapshot(old.now)) != 1 || cold.Snapshot(old.now)[0].Generation != 2 {
		t.Fatal("renewed artifact failed pooled download and cold certificate repair")
	}
	if _, err := relay.Accept(wire, old.now); err == nil {
		t.Fatal("renewed cache accepted an old leaf fingerprint")
	}
	expired := old.now.Add(testSpec().Lease + time.Second)
	if got := sender.Record(current, expired); len(got) != 0 || relay.history[key].Generation != 2 {
		t.Fatal("gossip renewed a signed lease or lost its generation floor")
	}
}

func TestServiceGossipDescriptorRefreshUsesCertificateFetchLease(t *testing.T) {
	f := newFixture(t)
	catalog, cache := f.newCatalog(t, p533, "default", nil, nil)
	w, err := catalog.PublishGatewayOffer(f.now)
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(catalog, cache, netgossip.NewPool())
	defer sender.Close()
	have := sender.Record(w, f.now)
	if len(have) != 1 {
		t.Fatal("HAVE needs exactly one slot")
	}
	requester := netgossip.NewPool().NewSession(netgossip.Service)
	defer requester.Close()
	want, _, err := requester.Receive(*have[0].Gossip, f.now)
	if err != nil || want == nil {
		t.Fatal(want, err)
	}
	for i, offset := range []time.Duration{0, time.Second, localmesh.MaxLease + time.Second} {
		want.Request++
		out, _, err := sender.Receive(Message{Kind: "gossip", Gossip: want}, f.now.Add(offset), 0)
		need := 2
		if i == 1 {
			need = 1
		}
		if err != nil || len(out) != need || out[len(out)-1].Gossip == nil || out[len(out)-1].Gossip.Op != "data" {
			t.Fatal("bad descriptor refresh or extra bulk progress", offset, out, err)
		}
		if need == 2 && out[0].Certificate == nil {
			t.Fatal("timed-out fetch cannot restart from requested DATA")
		}
	}
}

func TestServiceGossipUnavailableDoesNotConsumeDescriptorHint(t *testing.T) {
	f := newFixture(t)
	catalog, cache := f.newCatalog(t, p533, "default", nil, nil)
	w, err := catalog.PublishGatewayOffer(f.now)
	if err != nil {
		t.Fatal(err)
	}
	pool := netgossip.NewPool()
	sender := NewSynchronizer(catalog, cache, pool)
	defer sender.Close()
	have := sender.Record(w, f.now)
	requester := netgossip.NewPool().NewSession(netgossip.Service)
	defer requester.Close()
	want, _, err := requester.Receive(*have[0].Gossip, f.now)
	if err != nil || want == nil {
		t.Fatal(want, err)
	}
	// Occupy every shared outbound transfer with a partially sent body. A
	// failed WANT must leave the descriptor available for the later retry.
	var blockers []*netgossip.Session
	for i := range 16 {
		blocker := pool.NewSession(netgossip.Service)
		blockers = append(blockers, blocker)
		defer blocker.Close()
		body := make([]byte, 32<<10)
		body[0] = byte(i)
		hint := blocker.Offer(body, f.now.Add(time.Minute), f.now)
		peer := netgossip.NewPool().NewSession(netgossip.Service)
		request, _, err := peer.Receive(*hint, f.now)
		if err != nil || request == nil {
			t.Fatal(request, err)
		}
		if first, _, err := blocker.Receive(*request, f.now); err != nil || first == nil || first.Op != "data" {
			t.Fatal(first, err)
		}
		peer.Close()
	}
	out, _, err := sender.Receive(Message{Kind: "gossip", Gossip: want}, f.now, 0)
	if err != nil || len(out) != 1 || out[0].Gossip.Op != "unavailable" || len(sender.sent) != 0 {
		t.Fatal("unavailable WANT consumed current descriptor hint", out, err)
	}
	for _, blocker := range blockers {
		blocker.Close()
	}
	want.Request++
	out, _, err = sender.Receive(Message{Kind: "gossip", Gossip: want}, f.now, 0)
	if err != nil || len(out) != 2 || out[0].Certificate == nil || out[1].Gossip.Op != "data" {
		t.Fatal("retry lost descriptor or body progress", out, err)
	}
}

func TestServiceGossipWarmRenewalKeepsOneVerifiedDownload(t *testing.T) {
	for _, mode := range []string{"complete", "replacement", "deadline", "mismatched-completion", "capacity", "close"} {
		t.Run(mode, func(t *testing.T) {
			old, current := newFixture(t), newFixture(t)
			old.now = current.now
			verify := func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
				if id, err := current.creds[p534].Verify(chain, now); err == nil {
					return id, nil
				}
				return old.creds[p534].Verify(chain, now)
			}
			cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), verify, old.now)
			if err != nil {
				t.Fatal(err)
			}
			dest, err := NewCatalog("default", old.creds[p534].Device, 32, old.creds[p534], cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, old.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = cache.Put(old.creds[p533].Certificate.Certificate, old.now); err != nil {
				t.Fatal(err)
			}
			source, sourceCache := current.newCatalog(t, p533, "default", nil, nil)
			w, err := source.Publish(testSpec(), old.now)
			if err != nil {
				t.Fatal(err)
			}
			pool := netgossip.NewPool()
			sender := NewSynchronizer(source, sourceCache, netgossip.NewPool())
			receiver, alternate := NewSynchronizer(dest, cache, pool), NewSynchronizer(dest, cache, pool)
			defer sender.Close()
			defer receiver.Close()
			defer alternate.Close()
			have := sender.Record(w, old.now)
			if len(have) != 1 {
				t.Fatal("publication needs more than one slot")
			}
			want, _, err := receiver.Receive(have[0], old.now)
			if err != nil || len(want) != 1 {
				t.Fatal(want, err)
			}
			if out, _, err := alternate.Receive(have[0], old.now); err != nil || len(out) != 0 {
				t.Fatal("alternate duplicated warm body download", out, err)
			}
			out, _, err := sender.Receive(want[0], old.now, 0)
			if err != nil || len(out) != 2 || out[0].Certificate == nil || out[1].Gossip == nil {
				t.Fatal("missing descriptor before DATA", out, err)
			}
			requests, _, err := receiver.Receive(out[0], old.now)
			if err != nil || len(requests) != 1 {
				t.Fatal(requests, err)
			}
			if mode == "capacity" {
				for range maxPendingRecords {
					receiver.pending[p534] = append(receiver.pending[p534], pendingServiceRecord{wire: w})
				}
			}
			_, changed, err := receiver.Receive(out[1], old.now)
			if mode == "capacity" {
				if !errors.Is(err, errPendingIdentities) {
					t.Fatal("pending limit was bypassed", err)
				}
			} else if err != nil || len(changed) != 0 || !receiver.PendingIdentity() || len(dest.Snapshot(old.now)) != 0 {
				t.Fatal("warm body gained authority before current chain completed", changed, err)
			}
			at := old.now
			switch mode {
			case "complete":
				var chunks []Message
				for _, request := range requests {
					next, _, err := sender.Receive(request, at)
					if err != nil {
						t.Fatal(err)
					}
					chunks = append(chunks, next...)
				}
				changed, err := exchangeWarmServiceMessages(sender, receiver, chunks, at)
				if err != nil || len(changed) != 1 || receiver.PendingIdentity() || alternate.PendingAnnouncements(at) || len(dest.Snapshot(at)) != 1 {
					t.Fatal("verified pooled renewal did not resolve shared receipt", changed, err)
				}
				return
			case "replacement":
				_, _, err = receiver.Receive(Message{Kind: "certificate", Certificate: localmesh.DescribeCertificates(p533, old.creds[p533].Certificate.Certificate)}, at)
			case "deadline":
				at = at.Add(testSpec().Lease + time.Second)
				_, _, err = receiver.ContinueCertificates(at, 0)
			case "mismatched-completion":
				_, _, err = receiver.certificateResult(nil, []string{p533}, nil, at)
			case "close":
				receiver.Close()
			}
			if err != nil && mode != "capacity" {
				t.Fatal(err)
			}
			if next := alternate.gossip.Poll(at, 1); len(next) != 1 || next[0].Op != "want" {
				t.Fatal("dropped service candidate stranded shared request", next)
			}
		})
	}
}

func TestServiceGossipRequiresExactCurrentArtifactBeforeExport(t *testing.T) {
	f := newFixture(t)
	catalog, cache := f.newCatalog(t, p533, "default", nil, nil)
	first, err := catalog.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(catalog, cache, netgossip.NewPool())
	defer sender.Close()
	have := sender.Record(first, f.now)
	peer := netgossip.NewPool().NewSession(netgossip.Service)
	defer peer.Close()
	want, _, err := peer.Receive(*have[0].Gossip, f.now)
	if err != nil || want == nil {
		t.Fatal(want, err)
	}
	current, err := catalog.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if out := sender.Record(first, f.now); len(out) != 0 {
		t.Fatal("queued superseded artifact was re-advertised")
	}
	out, _, err := sender.Receive(Message{Kind: "gossip", Gossip: want}, f.now)
	if err != nil || len(out) != 1 || out[0].Gossip == nil || out[0].Gossip.Op != "unavailable" {
		t.Fatal("cached old body bypassed exact current artifact gate", out, err)
	}
	if out := sender.Record(current, f.now); len(out) != 1 {
		t.Fatal("replacement lost publication progress")
	}
}

func TestServiceGossipSameLeafIssuerChangeRefreshesDescriptorImmediately(t *testing.T) {
	now, old, current, verify := serviceIssuerRenewalFixture(t, time.Hour)
	newSide := func() (*Catalog, *localmesh.IdentityCache) {
		cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), verify, now)
		if err != nil {
			t.Fatal(err)
		}
		c, err := NewCatalog("default", old.Device, 32, old, cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, now)
		if err != nil {
			t.Fatal(err)
		}
		return c, cache
	}
	source, sourceCache := newSide()
	dest, cache := newSide()
	sender := NewSynchronizer(source, sourceCache, netgossip.NewPool())
	receiver := NewSynchronizer(dest, cache, netgossip.NewPool())
	defer sender.Close()
	defer receiver.Close()
	spec := testSpec()
	spec.Lease = 10 * time.Second
	first, err := source.Publish(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	exchangeServiceMessages(t, sender, receiver, sender.Record(first, now), now)
	if _, err = sourceCache.Put(current.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	source.creds = current
	wire, err := source.Publish(testSpec(), now)
	if err != nil {
		t.Fatal(err)
	}
	have := sender.Record(wire, now)
	want, _, err := receiver.Receive(have[0], now)
	if err != nil || len(want) != 1 {
		t.Fatal(want, err)
	}
	out, _, err := sender.Receive(want[0], now, 0)
	if err != nil || len(out) != 2 || out[0].Certificate == nil {
		t.Fatal("fresh leaf hint suppressed changed issuer descriptor", out, err)
	}
	requests, _, err := receiver.Receive(out[0], now)
	if err != nil || len(requests) != 1 || requests[0].Certificate.Request.Hash == wire.Fingerprint {
		t.Fatal("renewed issuer behind known leaf was not fetched", requests, err)
	}
	if _, changed, err := receiver.Receive(out[1], now); err != nil || len(changed) != 0 {
		t.Fatal("issuer lifetime candidate gained authority before chain completion", changed, err)
	}
	var chunks []Message
	for _, request := range requests {
		next, _, err := sender.Receive(request, now)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, next...)
	}
	changed, err := exchangeWarmServiceMessages(sender, receiver, chunks, now)
	if err != nil || len(changed) != 1 || len(dest.Snapshot(now)) != 1 || dest.Snapshot(now)[0].Generation != 2 {
		t.Fatal("same-leaf pooled service issuer renewal failed", changed, err)
	}
}
