package localmesh

import (
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

type gossipManifestRenewal struct {
	now              time.Time
	old, renewed     *Credentials
	allowOld         *bool
	source, dest     *Directory
	sender, receiver *Synchronizer
	wire             SignedManifest
}

func newGossipManifestRenewal(t *testing.T, sameKey bool) gossipManifestRenewal {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	var key *ecdsa.PrivateKey
	if sameKey {
		key = old.Signer.(*ecdsa.PrivateKey)
	}
	renewed := newManifestTestAuthority(t, now).issue(t, now, key, now.Add(2*time.Hour))
	allowOld := true
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
			return id, nil
		}
		if allowOld {
			return old.Verify(chain, at)
		}
		return Identity{}, errors.New("old issuer retired")
	}
	newSide := func() *Directory {
		cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = cache.Put(old.Certificate.Certificate, now); err != nil {
			t.Fatal(err)
		}
		d, err := NewDirectory(old.Device.Tenant(), 256, cache)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	source, dest := newSide(), newSide()
	w, err := SignManifest(routeManifest(old.Device, now), old.Certificate.Certificate, old.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(source, source.cache, netgossip.NewPool())
	receiver := NewSynchronizer(dest, dest.cache, netgossip.NewPool())
	t.Cleanup(sender.Close)
	t.Cleanup(receiver.Close)
	return gossipManifestRenewal{now, old, renewed, &allowOld, source, dest, sender, receiver, w}
}

func TestManifestGossipRenewalRejectsStaleOffersWantsAndLoader(t *testing.T) {
	f := newGossipManifestRenewal(t, false)
	have := f.sender.Record(f.wire, f.now)
	want, _, err := f.receiver.Receive(have[0], f.now)
	if err != nil || len(want) != 1 {
		t.Fatal(want, err)
	}
	before, receipt := f.source.entries[f.wire.Principal], f.source.history[f.wire.Principal]
	*f.allowOld = false
	if _, err = f.source.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	if got := f.sender.Record(f.wire, f.now); len(got) != 0 {
		t.Fatal("stale queued manifest was advertised")
	}
	replies, _, err := f.sender.Receive(want[0], f.now)
	if err != nil || len(replies) != 1 || replies[0].Gossip == nil || replies[0].Gossip.Op != "unavailable" {
		t.Fatal(replies, err)
	}
	if payload := f.source.manifestPayload(f.wire.Principal, f.now); len(payload) != 0 {
		t.Fatal("loader exported obsolete signature")
	}
	if f.source.entries[f.wire.Principal].activeUntil != before.activeUntil || f.source.history[f.wire.Principal] != receipt {
		t.Fatal("export check changed lease or floor")
	}
}

func TestManifestGossipWarmRenewalDescriptorPrecedesData(t *testing.T) {
	f := newGossipManifestRenewal(t, false)
	// The old key remains trusted at the receiver while the new-key artifact
	// arrives. Only this link's exact descriptor permits a bounded retry.
	if _, err := f.source.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	m := routeManifest(f.renewed.Device, f.now)
	m.Revision++
	w, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.source.Accept(w, f.now); err != nil {
		t.Fatal(err)
	}
	have := f.sender.Record(w, f.now)
	if len(have) != 1 || have[0].Gossip == nil {
		t.Fatal("HAVE publication grew beyond one frame")
	}
	want, _, err := f.receiver.Receive(have[0], f.now)
	if err != nil || len(want) != 1 {
		t.Fatal(want, err)
	}
	replies, _, err := f.sender.Receive(want[0], f.now, 10)
	if err != nil || len(replies) != 2 || replies[0].Certificate == nil || replies[1].Gossip == nil || replies[1].Gossip.Op != "data" {
		t.Fatal(replies, err)
	}
	requests, _, err := f.receiver.Receive(replies[0], f.now)
	if err != nil || len(requests) != 1 {
		t.Fatal(requests, err)
	}
	out, changed, err := f.receiver.Receive(replies[1], f.now)
	if err != nil || len(out) != 0 || len(changed) != 0 || len(f.receiver.pending) != 1 || len(f.dest.Snapshot(f.now)) != 0 {
		t.Fatal("unverified warm DATA bypassed bounded defer", out, changed, err)
	}
	changed, err = driveWarmCertificateFetch(f.sender, f.receiver, requests, f.now)
	if err != nil || len(changed) != 1 || len(f.dest.Snapshot(f.now)) != 1 {
		t.Fatal("pooled renewal did not recover", changed, err)
	}
}

func TestKnownManifestGossipReceiptRepairsIdentityWithoutBody(t *testing.T) {
	f := newGossipManifestRenewal(t, true)
	if changed := exchangeDirectoryMessages(t, f.sender, f.receiver, f.sender.Record(f.wire, f.now), f.now); len(changed) != 1 {
		t.Fatal("initial artifact absent")
	}
	before, receipt := f.dest.entries[f.wire.Principal], f.dest.history[f.wire.Principal]
	*f.allowOld = false
	if _, err := f.source.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	if len(f.dest.Snapshot(f.now)) != 0 {
		t.Fatal("retired issuer remained trusted")
	}
	have := f.sender.Record(f.wire, f.now)
	if len(have) != 1 {
		t.Fatal("same-key renewal lost exact original artifact")
	}
	repairs, _, err := f.receiver.Receive(have[0], f.now)
	if err != nil || len(repairs) != 1 || repairs[0].Kind != "identity-request" {
		t.Fatal("known receipt suppressed identity-only repair", repairs, err)
	}
	if repeated, _, err := f.receiver.Receive(have[0], f.now); err != nil || len(repeated) != 0 {
		t.Fatal("identity repair was not coalesced", repeated, err)
	}
	changed, err := driveWarmCertificateFetch(f.sender, f.receiver, repairs, f.now)
	if err != nil || len(changed) != 0 || len(f.dest.Snapshot(f.now)) != 1 {
		t.Fatal("current identity did not restore existing signed artifact", changed, err)
	}
	if f.dest.entries[f.wire.Principal].activeUntil != before.activeUntil || f.dest.history[f.wire.Principal] != receipt {
		t.Fatal("identity-only repair extended signed lease or floor")
	}
}

func TestManifestGossipLoaderExpiryCannotRollBack(t *testing.T) {
	f := newGossipManifestRenewal(t, false)
	have := f.sender.Record(f.wire, f.now)
	want, _, err := f.receiver.Receive(have[0], f.now)
	if err != nil || len(want) != 1 {
		t.Fatal(want, err)
	}
	future := f.now.Add(time.Minute + time.Second)
	if len(f.source.manifestPayload(f.wire.Principal, future)) != 0 {
		t.Fatal("expired loader returned payload")
	}
	if len(f.source.manifestPayload(f.wire.Principal, f.now)) != 0 || len(f.sender.Record(f.wire, f.now)) != 0 {
		t.Fatal("rollback revived manifest export")
	}
	replies, _, err := f.sender.Receive(want[0], f.now)
	if err != nil || len(replies) != 1 || replies[0].Gossip == nil || replies[0].Gossip.Op != "unavailable" {
		t.Fatal("cached bytes bypassed latched expiry", replies, err)
	}
	if !f.source.entries[f.wire.Principal].expired || !f.source.wallHighWater.Equal(future) {
		t.Fatal("loader overwrote expiry state")
	}
}

func TestKnownManifestReceiptDoesNotRepairObsoleteTrustedKey(t *testing.T) {
	f := newGossipManifestRenewal(t, false)
	have := f.sender.Record(f.wire, f.now)
	exchangeDirectoryMessages(t, f.sender, f.receiver, have, f.now)
	if _, err := f.dest.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	if replies, changed, err := f.receiver.Receive(have[0], f.now); err != nil || len(replies) != 0 || len(changed) != 0 || len(f.receiver.pending) != 0 {
		t.Fatal("obsolete signature triggered identity repair despite trusted current key", replies, changed, err)
	}
	if len(f.dest.Snapshot(f.now)) != 0 {
		t.Fatal("old key borrowed current trust")
	}
}

func TestManifestGossipInFlightOldDataCannotBorrowRenewedKey(t *testing.T) {
	f := newGossipManifestRenewal(t, false)
	have := f.sender.Record(f.wire, f.now)
	want, _, err := f.receiver.Receive(have[0], f.now)
	if err != nil {
		t.Fatal(err)
	}
	replies, _, err := f.sender.Receive(want[0], f.now)
	if err != nil || len(replies) != 2 {
		t.Fatal(replies, err)
	}
	// The DATA was already requested and queued before rotation. It remains
	// untrusted until actual admission, even when its digest was advertised.
	if _, err = f.dest.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := f.receiver.Receive(replies[1], f.now); !errors.Is(err, ErrInvalidManifestSignature) || len(changed) != 0 {
		t.Fatal("in-flight old signature borrowed renewed trust", changed, err)
	}
	if len(f.dest.Snapshot(f.now)) != 0 {
		t.Fatal("old DATA entered directory")
	}
	f.receiver.Close()
}

func TestManifestGossipDroppedWarmCandidateReleasesAlternate(t *testing.T) {
	for _, drop := range []string{"replacement", "deadline", "mismatched-completion", "capacity"} {
		t.Run(drop, func(t *testing.T) {
			f := newGossipManifestRenewal(t, false)
			pool := netgossip.NewPool()
			f.receiver.gossip.Close()
			f.receiver.gossip = pool.NewSession(netgossip.Manifest)
			alternate := NewSynchronizer(f.dest, f.dest.cache, pool)
			defer alternate.Close()
			if _, err := f.source.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
				t.Fatal(err)
			}
			m := routeManifest(f.renewed.Device, f.now)
			m.Revision++
			w, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.source.Accept(w, f.now); err != nil {
				t.Fatal(err)
			}
			have := f.sender.Record(w, f.now)[0]
			want, _, err := f.receiver.Receive(have, f.now)
			if err != nil || len(want) != 1 {
				t.Fatal(want, err)
			}
			if out, _, err := alternate.Receive(have, f.now); err != nil || len(out) != 0 {
				t.Fatal("duplicate shared download", out, err)
			}
			replies, _, err := f.sender.Receive(want[0], f.now)
			if err != nil || len(replies) != 2 {
				t.Fatal(replies, err)
			}
			if _, _, err = f.receiver.Receive(replies[0], f.now); err != nil {
				t.Fatal(err)
			}
			if drop == "capacity" {
				for i := range 128 {
					f.receiver.pending[string(rune(i+1))] = SignedManifest{}
				}
			}
			if _, _, err = f.receiver.Receive(replies[1], f.now); err != nil {
				t.Fatal(err)
			}
			at := f.now
			switch drop {
			case "replacement":
				_, _, err = f.receiver.Receive(warmDescriptor(f.old), f.now)
			case "deadline":
				at = f.now.Add(time.Minute + time.Second)
				_, _, err = f.receiver.ContinueCertificates(at, 0)
			case "mismatched-completion":
				_, _, err = f.receiver.certificateResult(nil, []string{w.Principal}, nil, f.now)
			}
			if err != nil {
				t.Fatal(err)
			}
			out := alternate.gossip.Poll(at, 1)
			if len(out) != 1 || out[0].Op != "want" {
				t.Fatal("discarded unverified candidate stranded pooled reservation", out)
			}
		})
	}
}

func TestClosedManifestGossipCannotRestartKnownIdentityRepair(t *testing.T) {
	f := newGossipManifestRenewal(t, true)
	exchangeDirectoryMessages(t, f.sender, f.receiver, f.sender.Record(f.wire, f.now), f.now)
	*f.allowOld = false
	if _, err := f.source.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	f.receiver.Close()
	have := f.sender.Record(f.wire, f.now)
	if out, changed, err := f.receiver.Receive(have[0], f.now); err == nil || len(out) != 0 || len(changed) != 0 || len(f.receiver.pending) != 0 {
		t.Fatal("closed session revived pending identity repair", out, changed, err)
	}
}

func TestManifestGossipSameLeafIssuerChangeRefreshesDescriptorImmediately(t *testing.T) {
	f := sameLeafIssuerRenewalFixture(t, time.Hour)
	sender := f.source(t, f.old)
	receiver, dest, _ := f.receiver(t)
	sender.gossip = netgossip.NewPool().NewSession(netgossip.Manifest)
	receiver.gossip = netgossip.NewPool().NewSession(netgossip.Manifest)
	m := routeManifest(f.old.Device, f.now)
	m.Expires = f.now.Add(10 * time.Second).UnixMilli()
	first, err := SignManifest(m, f.old.Certificate.Certificate, f.old.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sender.directory.Accept(first, f.now); err != nil {
		t.Fatal(err)
	}
	exchangeDirectoryMessages(t, sender, receiver, sender.Record(first, f.now), f.now)
	if _, err = sender.cache.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	m.Revision++
	m.Expires = f.now.Add(time.Minute).UnixMilli()
	wire, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sender.directory.Accept(wire, f.now); err != nil {
		t.Fatal(err)
	}
	have := sender.Record(wire, f.now)
	want, _, err := receiver.Receive(have[0], f.now)
	if err != nil || len(want) != 1 {
		t.Fatal(want, err)
	}
	out, _, err := sender.Receive(want[0], f.now, 10)
	if err != nil || len(out) != 2 || out[0].Certificate == nil {
		t.Fatal("fresh leaf hint suppressed changed issuer descriptor", out, err)
	}
	requests, _, err := receiver.Receive(out[0], f.now)
	if err != nil || len(requests) != 1 || requests[0].Certificate.Request.Hash == certificateHash(f.renewed.Certificate.Certificate[0]) {
		t.Fatal("renewed issuer behind known leaf was not fetched", requests, err)
	}
	if _, changed, err := receiver.Receive(out[1], f.now); err != nil || len(changed) != 0 {
		t.Fatal("same-leaf lifetime candidate escaped exact-chain wait", changed, err)
	}
	changed, err := driveWarmCertificateFetch(sender, receiver, requests, f.now)
	if err != nil || len(changed) != 1 || len(dest.Snapshot(f.now)) != 1 || dest.Snapshot(f.now)[0].Revision != 2 {
		t.Fatal("same-leaf pooled issuer renewal failed", changed, err)
	}
}
