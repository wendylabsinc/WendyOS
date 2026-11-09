package localmesh

import (
	"crypto/ecdsa"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Independent real issuers exercise current-chain trust as well as a changed
// signing key. The retired issuer remains unavailable after renewal.
func routeRenewalDirectory(t *testing.T, now time.Time, sameKey bool) (*Directory, *Credentials, *Credentials, func(time.Time)) {
	t.Helper()
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
		return Identity{}, errors.New("old route issuer retired")
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(old.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirectory(old.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	return d, old, renewed, func(at time.Time) {
		t.Helper()
		allowOld = false
		if _, err := cache.Put(renewed.Certificate.Certificate, at); err != nil {
			t.Fatal(err)
		}
	}
}

func signRenewedRoute(t *testing.T, m Manifest, credentials *Credentials, now time.Time) SignedManifest {
	t.Helper()
	w, err := SignManifest(m, credentials.Certificate.Certificate, credentials.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRouteAuthorizationRenewalPreservesOriginalLeaseAndReplayFloor(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotated-key", true: "same-key"}[sameKey], func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			d, old, renewed, renew := routeRenewalDirectory(t, now, sameKey)
			saves := 0
			if err := d.SetPersistence(nil, func([]DirectoryReceipt) error { saves++; return nil }, now); err != nil {
				t.Fatal(err)
			}
			m := routeManifest(old.Device, now)
			m.Revision = 7
			wire := signRenewedRoute(t, m, old, now)
			if _, err := d.Accept(wire, now); err != nil {
				t.Fatal(err)
			}
			before, receipt := d.entries[wire.Principal], d.history[wire.Principal]
			renew(now.Add(time.Second))
			if got := d.RouteAuthorizations(now.Add(time.Second))[*m.Route]; got != sameKey {
				t.Fatalf("old proof after renewal authorized=%v sameKey=%v", got, sameKey)
			}
			resigned := signRenewedRoute(t, m, renewed, now.Add(time.Second))
			if changed, err := d.Accept(resigned, now.Add(time.Second)); err != nil || changed == sameKey {
				t.Fatalf("same revision recovery changed=%v sameKey=%v err=%v", changed, sameKey, err)
			}
			after := d.entries[wire.Principal]
			if !after.activeUntil.Equal(before.activeUntil) || after.expired != before.expired || !after.retainUntil.Equal(before.retainUntil) || after.hash != before.hash || d.history[wire.Principal] != receipt || saves != 0 {
				t.Fatal("re-sign changed original deadline, retention or replay receipt")
			}
			if !d.RouteAuthorizations(now.Add(time.Second))[*m.Route] {
				t.Fatal("current-key route did not recover")
			}
			current := resigned
			if sameKey {
				current = wire
			}
			if chain, ok := d.CurrentRecordChain(current, now.Add(time.Second)); !ok || !sameChain(chain, renewed.Certificate.Certificate) {
				t.Fatal("route proof not paired with its currently trusted chain")
			}
			// A valid current-key signature on an older revision cannot reset
			// replay history after the old signing key loses authorization.
			stale := m
			stale.Revision--
			if _, err := d.Accept(signRenewedRoute(t, stale, renewed, now.Add(2*time.Second)), now.Add(2*time.Second)); !errors.Is(err, ErrStaleManifest) {
				t.Fatalf("renewal erased revision floor: %v", err)
			}
			if _, ok := d.CurrentRecordChain(current, now.Add(time.Minute)); ok || len(d.RouteAuthorizations(now.Add(time.Minute))) != 0 {
				t.Fatal("renewal extended original signed route expiry")
			}
			if _, ok := d.CurrentRecordChain(current, now.Add(30*time.Second)); ok || len(d.RouteAuthorizations(now.Add(30*time.Second))) != 0 {
				t.Fatal("current-key proof revived after observed expiry and clock rollback")
			}
		})
	}
}

func TestRouteRenewalTickRemovesAndRestoresAppliedForwarding(t *testing.T) {
	n := newSignedRouteTestNetwork(t, 2)
	n.epoch = time.Now().Truncate(time.Second)
	d, old, renewed, renew := routeRenewalDirectory(t, n.now(), false)
	n.nodes[1].directory = d
	n.connect(0, 1, 1)
	n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
	m := routeManifest(old.Device, n.now())
	m.Route = &RouteOrigin{Prefix: n.nodes[0].device.prefix, RouterID: n.nodes[0].device.routerID, Seqno: n.nodes[0].routing.Snapshot().Seqno}
	n.accept(1, signRenewedRoute(t, m, old, n.now()))
	n.advance(3 * time.Second)
	n.assertForwarded(1, true, m.Route.Seqno)
	renew(n.now())
	// No route packet accompanies renewal. The next policy snapshot must
	// revoke both selected state and the applied forwarding table.
	n.step(1, babel.Tick{})
	n.assertForwarded(1, false, 0)
	n.accept(1, signRenewedRoute(t, m, renewed, n.now()))
	n.step(1, babel.Tick{})
	n.assertForwarded(1, true, m.Route.Seqno)
}

func TestRouteRenewalResignCannotReviveElapsedRemainingLease(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	d, old, renewed, renew := routeRenewalDirectory(t, now, false)
	clock := &directoryElapsedClock{now: now}
	d.monotonicNow = clock.read
	m := routeManifest(old.Device, now)
	m.Expires = now.Add(2 * time.Minute).UnixMilli()
	wire := signRenewedRoute(t, m, old, now)
	if _, err := d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	receipt := d.history[wire.Principal]
	clock.advance(2 * time.Minute)
	if _, ok := d.CurrentRecordChain(wire, now); ok || len(d.entries) != 0 {
		t.Fatal("public lookup retained elapsed payload")
	}
	renew(now)
	resigned := signRenewedRoute(t, m, renewed, now)
	if changed, err := d.Accept(resigned, now); changed || !errors.Is(err, ErrStaleManifest) {
		t.Fatal("re-sign revived consumed local TTL", changed, err)
	}
	if len(d.entries) != 0 || d.history[wire.Principal] != receipt {
		t.Fatal("re-sign changed expired payload/watermark state")
	}
	if len(d.RouteAuthorizations(now)) != 0 || len(d.Records(now)) != 0 || len(d.Snapshot(now)) != 0 {
		t.Fatal("re-sign resurrected route/exports")
	}
}

func TestInvalidRenewedRouteDoesNotBlockAnotherAddressOwner(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
	if _, err := d.Accept(signRouteTestManifest(t, m, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	id := device.UUID()
	id[0] ^= 1 // Different full principal, same derived final eighty address bits.
	other := testDevice(t, testPrincipal(testTenantUUID, id.String()))
	otherChain, otherKey := addRouteTestIdentity(t, d, other, now)
	otherManifest := routeManifest(other, now)
	otherManifest.Route.RouterID++
	if otherManifest.Route.Prefix != netip.PrefixFrom(device.MeshAddress(), 128) {
		t.Fatal("fixture did not produce an address collision")
	}
	if _, err := d.Accept(signRouteTestManifest(t, otherManifest, otherChain, otherKey, now), now); err != nil {
		t.Fatal(err)
	}
	if len(d.RouteAuthorizations(now)) != 0 {
		t.Fatal("valid colliding claims did not fail closed")
	}
	// Replace only the first principal's signing key. Its now-invalid record
	// must be ignored before address conflict accounting.
	addRouteTestIdentity(t, d, device, now)
	got := d.RouteAuthorizations(now)
	if len(got) != 1 || !got[*otherManifest.Route] {
		t.Fatalf("invalid old signature blocked independent current owner: %v", got)
	}
}
