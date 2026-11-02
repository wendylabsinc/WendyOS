package localmesh

import (
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"testing"
	"time"
)

func TestWarmReceiverDefersNewKeyManifestDuringAdvertisedChainFetch(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	renewed := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(2*time.Hour))
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
			return id, nil
		}
		return old.Verify(chain, at)
	}
	sourceCache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	destCache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sourceCache.Put(renewed.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	if _, err = destCache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	source, err := NewDirectory(renewed.Device.Tenant(), 10, sourceCache)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := NewDirectory(old.Device.Tenant(), 10, destCache)
	if err != nil {
		t.Fatal(err)
	}
	manifest := routeManifest(renewed.Device, now)
	manifest.Revision = 2
	wire, err := SignManifest(manifest, renewed.Certificate.Certificate, renewed.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	sender, receiver := NewSynchronizer(source, sourceCache), NewSynchronizer(dest, destCache)
	defer sender.Close()
	defer receiver.Close()
	messages := sender.Record(wire, now)
	if len(messages) != 2 || messages[0].Certificate == nil || messages[1].Manifest == nil {
		t.Fatal("expected descriptor followed immediately by manifest")
	}
	replies, changed, err := receiver.Receive(messages[0], now)
	if err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Certificate.Request == nil {
		t.Fatalf("missing renewed leaf was not requested: replies=%v changed=%v err=%v", replies, changed, err)
	}
	if _, _, ok := destCache.Get(old.Device.Principal(), now); !ok {
		t.Fatal("warm old identity unexpectedly disappeared")
	}
	_, changed, err = receiver.Receive(messages[1], now)
	if err != nil {
		t.Fatalf("healthy warm receiver rejected new manifest before advertised chain completed: %v", err)
	}
	if len(changed) != 0 || len(dest.Snapshot(now)) != 0 {
		t.Fatal("unverified renewal was admitted before chain completion")
	}
	assertWarmRouteAuthorities(t, dest, now)
	// Resume the exact asynchronous certificate requests after the manifest.
	var toSender []ControlMessage
	toSender = append(toSender, replies...)
	for steps := 0; len(toSender) > 0; steps++ {
		if steps > 128 {
			t.Fatal("renewed chain fetch did not converge")
		}
		request := toSender[0]
		toSender = toSender[1:]
		chunks, _, e := sender.Receive(request, now)
		if e != nil {
			t.Fatal(e)
		}
		for _, chunk := range chunks {
			next, admitted, e := receiver.Receive(chunk, now)
			if e != nil {
				t.Fatal(e)
			}
			toSender = append(toSender, next...)
			changed = append(changed, admitted...)
		}
	}
	if len(changed) != 1 || len(dest.Snapshot(now)) != 1 {
		t.Fatal("verified new-key manifest did not recover after certificate completion")
	}
	assertWarmRouteAuthorities(t, dest, now, wire)
	if _, err = dest.Accept(wire, now.Add(2*time.Minute)); !errors.Is(err, ErrExpiredManifest) {
		t.Fatal("pending renewal extended the signed lease", err)
	}
	assertWarmRouteAuthorities(t, dest, now.Add(2*time.Minute))
}

type warmRenewalFixture struct {
	now                 time.Time
	old, renewed, third *Credentials
	verify              IdentityVerifier
}

func newWarmRenewalFixture(t *testing.T) warmRenewalFixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(time.Hour))
	issuer := newManifestTestAuthority(t, now)
	renewed := issuer.issue(t, now, nil, now.Add(2*time.Hour))
	third := issuer.issue(t, now, nil, now.Add(3*time.Hour))
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
			return id, nil
		}
		return old.Verify(chain, at)
	}
	return warmRenewalFixture{now, old, renewed, third, verify}
}
func (f warmRenewalFixture) receiver(t *testing.T) (*Synchronizer, *Directory, *IdentityCache) {
	t.Helper()
	c, err := OpenIdentityCache("", DefaultCacheLimits(), f.verify, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Put(f.old.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(f.old.Device.Tenant(), 256, c)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSynchronizer(d, c)
	t.Cleanup(s.Close)
	return s, d, c
}
func (f warmRenewalFixture) wire(t *testing.T, at time.Time) SignedManifest {
	t.Helper()
	m := routeManifest(f.renewed.Device, at)
	m.Revision = 2
	m.Expires = at.Add(MaxLease).UnixMilli()
	w, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, at)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func (f warmRenewalFixture) source(t *testing.T, credentials *Credentials) *Synchronizer {
	t.Helper()
	c, err := OpenIdentityCache("", DefaultCacheLimits(), f.verify, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Put(credentials.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(credentials.Device.Tenant(), 256, c)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSynchronizer(d, c)
	t.Cleanup(s.Close)
	return s
}
func warmDescriptor(c *Credentials) ControlMessage {
	return ControlMessage{Kind: "certificate", Certificate: DescribeCertificates(c.Device.Principal(), c.Certificate.Certificate)}
}

// Exercise the actual route-admission consumer of the renewed signed wires.
// routeManifest follows the introducing stage's current proof version, so the
// same warm-cache cases also verify the later full-principal signature format.
func assertWarmRouteAuthorities(t *testing.T, d *Directory, now time.Time, wires ...SignedManifest) {
	t.Helper()
	authorized := d.RouteAuthorizations(now)
	if len(authorized) != len(wires) {
		t.Fatalf("route authorities=%v, want %d verified current proofs", authorized, len(wires))
	}
	for _, wire := range wires {
		var m Manifest
		if err := netwire.Unmarshal(wire.Body, &m); err != nil {
			t.Fatal(err)
		}
		if m.Version != ManifestVersionRoute || m.Route == nil || !authorized[*m.Route] {
			t.Fatalf("verified current route proof was not authorized: %+v", m)
		}
		if _, ok := d.CurrentRecordChain(wire, now); !ok {
			t.Fatal("route authorization did not match the current signed wire")
		}
	}
}

func driveWarmCertificateFetch(sender, receiver *Synchronizer, requests []ControlMessage, now time.Time) ([]SignedManifest, error) {
	var changed []SignedManifest
	for steps := 0; len(requests) > 0; steps++ {
		if steps > 128 {
			return nil, errors.New("certificate exchange did not converge")
		}
		request := requests[0]
		requests = requests[1:]
		responses, _, err := sender.Receive(request, now)
		if err != nil {
			return nil, err
		}
		for _, response := range responses {
			next, admitted, err := receiver.Receive(response, now)
			if err != nil {
				return nil, err
			}
			changed = append(changed, admitted...)
			requests = append(requests, next...)
		}
	}
	return changed, nil
}

func TestWarmRenewalDoesNotDeferOrdinaryInvalidSignatures(t *testing.T) {
	f := newWarmRenewalFixture(t)
	for _, scenario := range []string{"absent-descriptor", "same-leaf", "other-principal", "expired-descriptor", "malformed-body"} {
		t.Run(scenario, func(t *testing.T) {
			receiver, d, _ := f.receiver(t)
			at := f.now
			descriptor := warmDescriptor(f.renewed)
			switch scenario {
			case "same-leaf":
				descriptor = warmDescriptor(f.old)
			case "other-principal":
				descriptor.Certificate.Chain.Principal = testPrincipal(testTenantUUID, testDeviceBID)
			}
			if scenario != "absent-descriptor" {
				if _, _, err := receiver.Receive(descriptor, at); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "expired-descriptor" {
				at = at.Add(MaxLease)
			}
			w := f.wire(t, at)
			if scenario == "malformed-body" {
				w.Body = []byte("invalid json")
			}
			_, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, at)
			if err == nil || len(changed) != 0 || len(d.Snapshot(at)) != 0 || len(receiver.pendingRenewals) != 0 {
				t.Fatal("invalid manifest escaped strict failure", err)
			}
			if scenario != "malformed-body" && !errors.Is(err, ErrInvalidManifestSignature) {
				t.Fatal("unexpected rejection", err)
			}
			assertWarmRouteAuthorities(t, d, at)
		})
	}
}

func TestWarmRenewalInvalidSignatureFailsAfterMatchingChainVerification(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, d, _ := f.receiver(t)
	sender := f.source(t, f.renewed)
	requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil {
		t.Fatal(err)
	}
	w := f.wire(t, f.now)
	w.Signature[0] ^= 1
	if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil || len(changed) != 0 {
		t.Fatal("pending advertised chain was not isolated", err)
	}
	if len(receiver.pendingRenewals) != 1 || len(d.Snapshot(f.now)) != 0 {
		t.Fatal("unverified signature gained authority")
	}
	assertWarmRouteAuthorities(t, d, f.now)
	if _, err = driveWarmCertificateFetch(sender, receiver, requests, f.now); !errors.Is(err, ErrInvalidManifestSignature) {
		t.Fatal("bad signature did not fail after matching chain verification", err)
	}
	if len(d.Snapshot(f.now)) != 0 || len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 {
		t.Fatal("failed retry was retained or projected")
	}
	assertWarmRouteAuthorities(t, d, f.now)
}

func TestWarmRenewalDescriptorReplacementDropsBoundWire(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, d, cache := f.receiver(t)
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	w := f.wire(t, f.now)
	if _, _, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
		t.Fatal(err)
	}
	requests, _, err := receiver.Receive(warmDescriptor(f.third), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 {
		t.Fatal("replacement retained a candidate bound to a different exact chain")
	}
	// The old in-flight element can complete first. Its public bytes must not
	// retry the discarded candidate under either descriptor.
	oldOwnerRequests := []ControlMessage{{Kind: "certificate", Certificate: &CertificateMessage{Request: receiver.certificates.waiting}}}
	responses, _, err := f.source(t, f.renewed).Receive(oldOwnerRequests[0], f.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, response := range responses {
		next, changed, err := receiver.Receive(response, f.now)
		if err != nil || len(changed) != 0 {
			t.Fatal("old bytes revived replaced candidate", err)
		}
		requests = append(requests, next...)
	}
	if _, err = driveWarmCertificateFetch(f.source(t, f.third), receiver, requests, f.now); err != nil {
		t.Fatal(err)
	}
	chain, _, ok := cache.Get(f.old.Device.Principal(), f.now)
	if !ok || !sameChain(chain, f.third.Certificate.Certificate) || len(d.Snapshot(f.now)) != 0 {
		t.Fatal("descriptor replacement did not remain independent of old pending signature")
	}
	assertWarmRouteAuthorities(t, d, f.now)
}

func TestWarmRenewalOriginalDeadlineDoesNotExtendOnRepeatedHints(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, d, _ := f.receiver(t)
	sender := f.source(t, f.renewed)
	requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil {
		t.Fatal(err)
	}
	at := f.now.Add(10 * time.Second)
	w := f.wire(t, at)
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, at); err != nil {
		t.Fatal(err)
	}
	deadline := receiver.pendingRenewals[w.Principal].expires
	if _, _, err = receiver.Receive(warmDescriptor(f.renewed), f.now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !receiver.pendingRenewals[w.Principal].expires.Equal(deadline) {
		t.Fatal("duplicate hint extended original pending deadline")
	}
	at = deadline.Add(time.Second)
	// Certificate traffic itself prunes the pending candidate before any replay;
	// its signed lease is still valid for another nine seconds here.
	if changed, err := driveWarmCertificateFetch(sender, receiver, requests, at); err != nil || len(changed) != 0 {
		t.Fatal("expired pending candidate was retried", err)
	}
	if len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 || len(d.Snapshot(at)) != 0 {
		t.Fatal("pending deadline was renewed")
	}
	assertWarmRouteAuthorities(t, d, at)
}

func TestWarmRenewalCrossNeighbourCompletionAndOwnedPendingWire(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, d, cache := f.receiver(t)
	owner := NewSynchronizer(d, cache)
	defer owner.Close()
	sender := f.source(t, f.renewed)
	requests, _, err := owner.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if replies, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil || len(replies) != 0 {
		t.Fatal("second neighbour duplicated the shared claim", err)
	}
	w := f.wire(t, f.now)
	original := cloneSignedManifest(w)
	if _, _, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
		t.Fatal(err)
	}
	w.Body[0] ^= 1
	w.Signature[0] ^= 1
	if _, err = driveWarmCertificateFetch(sender, owner, requests, f.now); err != nil {
		t.Fatal(err)
	}
	_, changed, err := receiver.ContinueCertificates(f.now)
	if err != nil || len(changed) != 1 || len(d.Snapshot(f.now)) != 1 {
		t.Fatal("verified shared bytes did not complete waiting neighbour", err)
	}
	if _, ok := d.CurrentRecordChain(original, f.now); !ok {
		t.Fatal("pending input aliased caller bytes or mismatched completion")
	}
	assertWarmRouteAuthorities(t, d, f.now, original)
}

func TestWarmRenewalPendingCapacityRemainsBounded(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, d, _ := f.receiver(t)
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	for i := range 128 {
		receiver.pending[fmt.Sprint(i)] = SignedManifest{Principal: fmt.Sprint(i)}
	}
	w := f.wire(t, f.now)
	if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil || len(changed) != 0 {
		t.Fatal("overflow tore down renewal exchange", err)
	}
	if len(receiver.pending) != 128 || len(receiver.pendingRenewals) != 0 || len(d.Snapshot(f.now)) != 0 {
		t.Fatal("pending overflow exceeded existing bound or admitted authority")
	}
	assertWarmRouteAuthorities(t, d, f.now)
}

func TestWarmRenewalFullDescriptorBindingSurvivesIdentityEviction(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, _, cache := f.receiver(t)
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	w := f.wire(t, f.now)
	if _, _, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
		t.Fatal(err)
	}
	original := receiver.pendingRenewals[w.Principal]
	// An ordinary cache eviction during fetch must not downgrade an exact warm
	// retry to the older principal-only cold-cache pending path.
	cache.mu.Lock()
	delete(cache.entries, w.Principal)
	cache.mu.Unlock()
	if _, _, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
		t.Fatal(err)
	}
	if pending, ok := receiver.pendingRenewals[w.Principal]; !ok || !sameCertificateDescription(pending.chain, original.chain) || !pending.expires.Equal(original.expires) {
		t.Fatal("cache miss stripped descriptor/deadline binding")
	}
	replacement := warmDescriptor(f.renewed)
	// Same leaf, different exact chain: a leaf-only fingerprint is insufficient.
	replacement.Certificate.Chain.Hashes = append(replacement.Certificate.Chain.Hashes, certificateHash(f.old.Certificate.Certificate[0]))
	if _, _, err := receiver.Receive(replacement, f.now); err != nil {
		t.Fatal(err)
	}
	if len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 {
		t.Fatal("same-leaf different-chain replacement kept old pending wire")
	}
}

func TestWarmRenewalSeveralSharedCompletionsRetryEveryPrincipal(t *testing.T) {
	f := newWarmRenewalFixture(t)
	oldB := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceBID))
	newB := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceBID))
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := f.verify(chain, at); err == nil {
			return id, nil
		}
		return newB.Verify(chain, at)
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Credentials{f.old, oldB} {
		if _, err = cache.Put(c.Certificate.Certificate, f.now); err != nil {
			t.Fatal(err)
		}
	}
	d, err := NewDirectory(f.old.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	receiver := NewSynchronizer(d, cache)
	defer receiver.Close()
	providerCache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Credentials{f.renewed, newB} {
		if _, err = providerCache.Put(c.Certificate.Certificate, f.now); err != nil {
			t.Fatal(err)
		}
	}
	providerDir, err := NewDirectory(f.old.Device.Tenant(), 10, providerCache)
	if err != nil {
		t.Fatal(err)
	}
	provider := NewSynchronizer(providerDir, providerCache)
	defer provider.Close()
	var owners []*Synchronizer
	var requests [][]ControlMessage
	for _, c := range []*Credentials{f.renewed, newB} {
		owner := NewSynchronizer(d, cache)
		defer owner.Close()
		owners = append(owners, owner)
		next, _, err := owner.Receive(warmDescriptor(c), f.now)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, next)
		if _, _, err = receiver.Receive(warmDescriptor(c), f.now); err != nil {
			t.Fatal(err)
		}
		m := routeManifest(c.Device, f.now)
		m.Revision = 2
		w, err := SignManifest(m, c.Certificate.Certificate, c.Signer, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
			t.Fatal(err)
		}
	}
	if len(receiver.pendingRenewals) != 2 {
		t.Fatal("expected both exact pending descriptors")
	}
	assertWarmRouteAuthorities(t, d, f.now)
	for i, owner := range owners {
		if _, err = driveWarmCertificateFetch(provider, owner, requests[i], f.now); err != nil {
			t.Fatal(err)
		}
	}
	_, changed, err := receiver.ContinueCertificates(f.now)
	if err != nil || len(changed) != 2 || len(d.Snapshot(f.now)) != 2 {
		t.Fatal("batch completion discarded a later principal's pending retry", len(changed), err)
	}
	assertWarmRouteAuthorities(t, d, f.now, changed...)
}

func TestWarmRenewalPendingChainSnapshotIsOwnedAndActorScoped(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, _, cache := f.receiver(t)
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	description, deadline, ok := receiver.certificates.PendingChain(f.old.Device.Principal(), f.now)
	if !ok || !deadline.Equal(f.now.Add(MaxLease)) {
		t.Fatal("missing active exact descriptor snapshot")
	}
	description.Hashes[0] = "caller mutation"
	again, _, ok := receiver.certificates.PendingChain(f.old.Device.Principal(), f.now)
	if !ok || again.Hashes[0] != certificateHash(f.renewed.Certificate.Certificate[0]) {
		t.Fatal("pending snapshot aliases exchange")
	}
	chain, _, ok := cache.Get(f.old.Device.Principal(), f.now)
	if !ok || !sameChain(chain, f.old.Certificate.Certificate) {
		t.Fatal("advertised descriptor granted unverified identity")
	}
	if _, _, ok = receiver.certificates.PendingChain(f.old.Device.Principal(), deadline); ok {
		t.Fatal("expired descriptor remained active")
	}
	receiver.Close()
	if _, _, ok = receiver.certificates.PendingChain(f.old.Device.Principal(), f.now); ok {
		t.Fatal("closed neighbour exported a pending descriptor")
	}
}

func TestWarmRenewalPreservesColdCertificateRepair(t *testing.T) {
	f := newWarmRenewalFixture(t)
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), f.verify, f.now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(f.old.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	receiver := NewSynchronizer(d, cache)
	defer receiver.Close()
	requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil {
		t.Fatal(err)
	}
	w := f.wire(t, f.now)
	if replies, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil || len(changed) != 0 || len(replies) != 1 || replies[0].Kind != "identity-request" {
		t.Fatal("cold-cache repair changed", err)
	}
	changed, err := driveWarmCertificateFetch(f.source(t, f.renewed), receiver, requests, f.now)
	if err != nil || len(changed) != 1 || len(d.Snapshot(f.now)) != 1 {
		t.Fatal("cold cache did not recover", err)
	}
	assertWarmRouteAuthorities(t, d, f.now, w)
}

func TestWarmRenewalExpiredSignedCandidateDropsBeforeFetchDeadline(t *testing.T) {
	f := newWarmRenewalFixture(t)
	receiver, d, _ := f.receiver(t)
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	m := routeManifest(f.renewed.Device, f.now)
	m.Revision = 2
	m.Expires = f.now.Add(10 * time.Second).UnixMilli()
	w, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
		t.Fatal(err)
	}
	at := f.now.Add(11 * time.Second)
	if _, changed, err := receiver.ContinueCertificates(at); err != nil || len(changed) != 0 {
		t.Fatal(err)
	}
	if len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 || len(d.Snapshot(at)) != 0 {
		t.Fatal("expired signed candidate remained pending")
	}
	if _, _, active := receiver.certificates.PendingChain(w.Principal, at); !active {
		t.Fatal("test did not distinguish signed expiry from the longer fetch deadline")
	}
	assertWarmRouteAuthorities(t, d, at)
}
