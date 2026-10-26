package meshcatalog

import (
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestRenewedIdentityDoesNotProjectOrRelayPreviousCertificateRecord(t *testing.T) {
	old, renewed := newFixture(t), newFixture(t)
	// Both caches use the same observation time even if certificate generation
	// crosses the fixture clock's one-second boundary.
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
	relay, err := NewCatalog("default", old.creds[p534].Device, 32, old.creds[p534], cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, old.now)
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
	if len(relay.Snapshot(old.now)) != 1 {
		t.Fatal("initial origin was not admitted")
	}
	// The authoritative principal cache now contains a different trusted
	// certificate/key. Retiring the old issuer must not let the new chain lend
	// current-trust eligibility to a record signed with the previous key.
	allowOld = false
	if _, err := cache.Put(renewed.creds[p533].Certificate.Certificate, old.now); err != nil {
		t.Fatal(err)
	}
	if got := relay.Snapshot(old.now); len(got) != 0 {
		t.Error("renewed identity authorized projection of a previous-certificate record")
	}
	if got := relay.Records(old.now); len(got) != 0 {
		t.Error("previous-certificate record remained in reconciliation")
	}
	// Also protect a record snapshot/Broadcast already queued before renewal.
	// Otherwise Record pairs a new certificate descriptor with the old
	// fingerprint and makes a cold receiver reject the healthy neighbour.
	sender := NewSynchronizer(relay, cache)
	defer sender.Close()
	if got := sender.Record(wire, old.now); len(got) != 0 {
		t.Error("queued old record paired with the renewed certificate descriptor")
	}
	key := Key{"default", p533, testSpec().AppID, testSpec().ServiceID}
	currentOrigin, _ := renewed.newCatalog(t, p533, "default", []Receipt{relay.history[key]}, nil)
	current, err := currentOrigin.Publish(testSpec(), old.now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := relay.Accept(current, old.now); err != nil || !changed {
		t.Fatalf("current generation did not recover: changed=%v err=%v", changed, err)
	}
	if got := relay.Snapshot(old.now); len(got) != 1 || got[0].Generation != 2 {
		t.Fatalf("renewal lost generation safety: %+v", got)
	}
	messages := sender.Record(current, old.now)
	if len(messages) != 2 || messages[0].Certificate == nil || messages[0].Certificate.Chain == nil || messages[0].Certificate.Chain.Hashes[0] != current.Fingerprint || messages[1].Record == nil || messages[1].Record.Fingerprint != current.Fingerprint {
		t.Fatal("current certificate and record were not transmitted together")
	}
	cold, coldCache := renewed.newCatalog(t, p535, "default", nil, nil)
	receiver := NewSynchronizer(cold, coldCache)
	defer receiver.Close()
	changed := exchangeServiceMessages(t, sender, receiver, messages, old.now)
	if len(changed) != 1 || len(cold.Snapshot(old.now)) != 1 {
		t.Fatal("renewed record did not reach a cold receiver through certificate exchange")
	}
	if _, err := relay.Accept(wire, old.now); err == nil {
		t.Fatal("renewed cache accepted old certificate fingerprint")
	}
	expired := old.now.Add(testSpec().Lease + time.Second)
	if len(relay.Snapshot(expired)) != 0 || len(relay.Records(expired)) != 0 || relay.history[key].Generation != 2 {
		t.Fatal("renewal extended the signed lease or lost its generation receipt")
	}
	if _, err := relay.Accept(current, expired); !errors.Is(err, ErrExpired) {
		t.Fatal("expired renewed record was resurrected", err)
	}
}
