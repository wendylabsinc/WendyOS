package localmesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"math/big"
	"net/netip"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func routeManifest(device meshid.Device, now time.Time) Manifest {
	return Manifest{
		Version: ManifestVersionRoute, Device: device.UUID(), Revision: 1,
		Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052,
		Route: &RouteOrigin{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: 42, Seqno: 7},
	}
}

func signRouteTestManifest(t *testing.T, m Manifest, chain [][]byte, key ed25519.PrivateKey, now time.Time) SignedManifest {
	t.Helper()
	w, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// rawRouteManifest bypasses sender validation to exercise receiver checks against
// a valid signature produced by a malicious enrolled origin.
func rawRouteManifest(t *testing.T, m Manifest, device meshid.Device, key ed25519.PrivateKey) SignedManifest {
	t.Helper()
	body, err := netwire.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := signingInput(key.Public(), device.Principal(), body)
	return SignedManifest{Principal: device.Principal(), Body: body, Signature: ed25519.Sign(key, input)}
}

func addRouteTestIdentity(t *testing.T, d *Directory, device meshid.Device, now time.Time) ([][]byte, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(device.Principal())
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	d.cache.verify = func(chain [][]byte, _ time.Time) (Identity, error) {
		leaf, err := x509.ParseCertificate(chain[0])
		if err != nil {
			return Identity{}, err
		}
		identity, err := meshid.DeviceFromCert(leaf)
		return Identity{Device: identity, NotAfter: leaf.NotAfter}, err
	}
	chain := [][]byte{der}
	if _, err := d.cache.Put(chain, now); err != nil {
		t.Fatal(err)
	}
	return chain, key
}

func TestRouteManifestRejectsUnauthorizedOrigins(t *testing.T) {
	other := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	for name, mutate := range map[string]func(*Manifest){
		"legacy claim":        func(m *Manifest) { m.Version = 1 },
		"missing claim":       func(m *Manifest) { m.Route = nil },
		"aggregate":           func(m *Manifest) { m.Route.Prefix = netip.PrefixFrom(m.Route.Prefix.Addr(), 64).Masked() },
		"noncanonical prefix": func(m *Manifest) { m.Route.Prefix = netip.PrefixFrom(m.Route.Prefix.Addr(), 64) },
		"other address":       func(m *Manifest) { m.Route.Prefix = netip.PrefixFrom(other.MeshAddress(), 128) },
		"invalid prefix":      func(m *Manifest) { m.Route.Prefix = netip.Prefix{} },
		"zero router ID":      func(m *Manifest) { m.Route.RouterID = 0 },
		"reserved router ID":  func(m *Manifest) { m.Route.RouterID = ^babel.RouterID(0) },
		"other owner":         func(m *Manifest) { m.Device = other.UUID() },
	} {
		t.Run(name, func(t *testing.T) {
			d, chain, key, device, now := directoryFixture(t)
			m := routeManifest(device, now)
			mutate(&m)
			if _, err := SignManifest(m, chain, key, now); err == nil {
				t.Fatal("sender signed invalid route authorization")
			}
			if _, err := d.Accept(rawRouteManifest(t, m, device, key), now); err == nil {
				t.Fatal("receiver admitted invalid route authorization")
			}
			if len(d.RouteAuthorizations(now)) != 0 {
				t.Fatal("rejected claim authorized a route")
			}
		})
	}
}

func TestRouteAuthorizationBindsOriginAndSequence(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
	w := signRouteTestManifest(t, m, chain, key, now)
	for _, field := range []string{"router", "sequence", "signature"} {
		t.Run(field, func(t *testing.T) {
			forged := w
			body := routeManifest(device, now)
			switch field {
			case "router":
				body.Route.RouterID++
			case "sequence":
				body.Route.Seqno++
			case "signature":
				forged.Signature = append([]byte(nil), w.Signature...)
				forged.Signature[0] ^= 1
			}
			var err error
			forged.Body, err = netwire.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Accept(forged, now); err == nil {
				t.Fatal("forged authorization accepted")
			}
		})
	}
	if changed, err := d.Accept(w, now); err != nil || !changed {
		t.Fatalf("accept signed origin: %v %v", changed, err)
	}
	authorized := d.RouteAuthorizations(now)
	if len(authorized) != 1 || !authorized[*m.Route] {
		t.Fatalf("signed route absent: %v", authorized)
	}
	wrongRouter := *m.Route
	wrongRouter.RouterID++
	wrongSeqno := *m.Route
	wrongSeqno.Seqno++
	if authorized[wrongRouter] || authorized[wrongSeqno] {
		t.Fatal("authorization admitted a different origin or sequence")
	}
	delete(authorized, *m.Route)
	snapshot := d.Snapshot(now)
	snapshot[0].Route.Seqno++
	if !d.RouteAuthorizations(now)[*m.Route] {
		t.Fatal("snapshot mutation changed route authorization")
	}
}

func TestRouteAuthorizationLeaseReplayAndClockRollback(t *testing.T) {
	d, chain, key, device, now, clock := monotonicDirectoryFixture(t)
	m := routeManifest(device, now)
	w := signRouteTestManifest(t, m, chain, key, now)
	if _, err := d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	clock.advance(50 * time.Second)
	if changed, err := d.Accept(w, now.Add(-598*time.Second)); err != nil || changed {
		t.Fatal(changed, err)
	}
	clock.advance(10 * time.Second)
	d.Sweep(now)
	if len(d.RouteAuthorizations(now)) != 0 || len(d.Records(now)) != 0 || len(d.entries) != 0 {
		t.Fatal("duplicate or wall rollback extended local lease")
	}
	if _, err := d.Accept(w, now); !errors.Is(err, ErrStaleManifest) {
		t.Fatal("expired same-body replay renewed route", err)
	}
}

func TestRouteAuthorizationWatermarkExpiryAllowsFirstLearning(t *testing.T) {
	d, chain, key, device, now, clock := monotonicDirectoryFixture(t)
	m := routeManifest(device, now)
	w := signRouteTestManifest(t, m, chain, key, now)
	if _, err := d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Minute)
	d.Sweep(now)
	if len(d.entries) != 0 || len(d.history) != 0 {
		t.Fatal("local retention did not release payload and metadata")
	}
	// Once the receiver intentionally forgets the compact watermark, an old
	// still-authenticated record may establish one new local duration.
	if changed, err := d.Accept(w, now); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if !d.RouteAuthorizations(now)[*m.Route] {
		t.Fatal("first-learned exact route missing")
	}
}

func TestRouteAuthorizationNewLeaseAfterClockRollback(t *testing.T) {
	d, chain, key, device, now, clock := monotonicDirectoryFixture(t)
	m := routeManifest(device, now)
	m.Expires = now.Add(2 * time.Minute).UnixMilli()
	w := signRouteTestManifest(t, m, chain, key, now)
	clock.advance(90 * time.Second)
	rolledBack := now.Add(-598 * time.Second)
	if _, err := d.Accept(w, rolledBack); err != nil {
		t.Fatal(err)
	}
	if !d.history[device.Key()].Until.Equal(clock.now.Add(20 * time.Minute)) {
		t.Fatal("watermark is not ten receive-local durations")
	}
	clock.advance(119 * time.Second)
	if !d.RouteAuthorizations(rolledBack)[*m.Route] {
		t.Fatal("wallclock shortened receive-local lease")
	}
	clock.advance(time.Second)
	if len(d.RouteAuthorizations(rolledBack)) != 0 {
		t.Fatal("local two-minute lease did not expire")
	}
}

func TestRouteAuthorizationRestartAndWithdrawal(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	saves := 0
	persist := func([]DirectoryReceipt) error { saves++; return nil }
	if err := d.SetPersistence(nil, persist, now); err != nil {
		t.Fatal(err)
	}
	m := routeManifest(device, now)
	w := signRouteTestManifest(t, m, chain, key, now)
	if _, err := d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	m.Revision++
	m.Withdraw = true
	if _, err := d.Accept(signRouteTestManifest(t, m, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	if len(d.RouteAuthorizations(now)) != 0 {
		t.Fatal("withdrawn route remained authorized")
	}
	if _, err := d.Accept(w, now); !errors.Is(err, ErrStaleManifest) {
		t.Fatal("in-process withdrawal floor lost", err)
	}
	restored, err := NewDirectory(testTenant(t), 10, d.cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.SetPersistence(nil, persist, now); err != nil {
		t.Fatal(err)
	}
	if len(restored.RouteAuthorizations(now)) != 0 {
		t.Fatal("restart restored route without network evidence")
	}
	if changed, err := restored.Accept(w, now); err != nil || !changed {
		t.Fatal("fresh process did not first-learn one local TTL", changed, err)
	}
	if saves != 0 {
		t.Fatal("foreign receive-local deadline written to legacy persistent state")
	}
}

func TestRouteAuthorizationRequiresCurrentTrustAndVersion(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
	legacy := m
	legacy.Version, legacy.Route = 1, nil
	if _, err := d.Accept(rawRouteManifest(t, legacy, device, key), now); err == nil {
		t.Fatal("legacy directory advertisement accepted")
	}
	if len(d.Snapshot(now)) != 0 || len(d.RouteAuthorizations(now)) != 0 {
		t.Fatal("legacy directory advertisement changed presence or routing")
	}
	if len(authorizedOrigins(d.Snapshot(now), d.tenant)) != 0 {
		t.Fatal("legacy verified snapshot authorized routing")
	}
	m.Revision++
	if _, err := d.Accept(signRouteTestManifest(t, m, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	if !d.RouteAuthorizations(now)[*m.Route] {
		t.Fatal("upgraded route missing")
	}
	if !authorizedOrigins(d.Snapshot(now), d.tenant)[*m.Route] {
		t.Fatal("upgraded route missing from verified snapshot")
	}
	d.cache.verify = func([][]byte, time.Time) (Identity, error) { return Identity{}, errors.New("trust removed") }
	if len(authorizedOrigins(d.Snapshot(now), d.tenant)) != 0 {
		t.Fatal("snapshot authorization survived loss of certificate trust")
	}
	if len(d.RouteAuthorizations(now)) != 0 {
		t.Fatal("authorization survived loss of certificate trust")
	}
}

func TestRouteAuthorizationSeparatesAddressOwnershipFromRouterIDs(t *testing.T) {
	for _, collision := range []string{"router ID", "mesh address"} {
		t.Run(collision, func(t *testing.T) {
			d, chain, key, device, now := directoryFixture(t)
			m := routeManifest(device, now)
			if _, err := d.Accept(signRouteTestManifest(t, m, chain, key, now), now); err != nil {
				t.Fatal(err)
			}
			otherUUID := testDeviceBID
			if collision == "mesh address" {
				// The address uses the final 80 UUID bits. Another full identity
				// can collide if allocation enforcement fails upstream.
				id := device.UUID()
				id[0] ^= 1
				otherUUID = id.String()
			}
			other := testDevice(t, testPrincipal(testTenantUUID, otherUUID))
			otherChain, otherKey := addRouteTestIdentity(t, d, other, now)
			otherManifest := routeManifest(other, now)
			if collision == "mesh address" {
				otherManifest.Route.RouterID++
			}
			if _, err := d.Accept(signRouteTestManifest(t, otherManifest, otherChain, otherKey, now), now); err != nil {
				t.Fatal(err)
			}
			got := d.RouteAuthorizations(now)
			if fromSnapshot := authorizedOrigins(d.Snapshot(now), d.tenant); !reflect.DeepEqual(fromSnapshot, got) {
				t.Fatalf("snapshot and route admission disagree about %s: %v / %v", collision, fromSnapshot, got)
			}
			if collision == "mesh address" {
				if len(got) != 0 {
					t.Fatalf("ambiguous address ownership authorized routes: %v", got)
				}
			} else if len(got) != 2 || !got[*m.Route] || !got[*otherManifest.Route] {
				// Router IDs are signed origin fields, not a globally allocated
				// identity namespace. Claiming another device's ID for one's own
				// address must not revoke that device's unrelated signed prefix.
				t.Fatalf("duplicate router ID revoked independent address ownership: %v", got)
			}
			otherManifest.Revision++
			otherManifest.Withdraw = true
			if _, err := d.Accept(signRouteTestManifest(t, otherManifest, otherChain, otherKey, now), now); err != nil {
				t.Fatal(err)
			}
			if !d.RouteAuthorizations(now)[*m.Route] {
				t.Fatal("withdrawn conflicting claim still blocked valid owner")
			}
			if !authorizedOrigins(d.Snapshot(now), d.tenant)[*m.Route] {
				t.Fatal("snapshot retained withdrawn conflicting address owner")
			}
		})
	}
}

func TestVerifiedSnapshotAuthorizationExpiresWithLease(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
	m.Expires = now.Add(30 * time.Second).UnixMilli()
	if _, err := d.Accept(signRouteTestManifest(t, m, chain, key, now), now); err != nil {
		t.Fatal(err)
	}
	if !authorizedOrigins(d.Snapshot(now.Add(29*time.Second)), d.tenant)[*m.Route] {
		t.Fatal("live verified lease missing")
	}
	if len(authorizedOrigins(d.Snapshot(now.Add(30*time.Second)), d.tenant)) != 0 {
		t.Fatal("snapshot authorization survived lease expiry")
	}
	if len(authorizedOrigins(d.Snapshot(now.Add(10*time.Second)), d.tenant)) != 0 {
		t.Fatal("clock rollback revived expired snapshot authorization")
	}
}

func TestVerifiedSnapshotAuthorizationRequiresExactOrigin(t *testing.T) {
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	for name, mutate := range map[string]func(*Manifest){
		"aggregate":          func(m *Manifest) { m.Route.Prefix = netip.PrefixFrom(m.Route.Prefix.Addr(), 64).Masked() },
		"zero router ID":     func(m *Manifest) { m.Route.RouterID = 0 },
		"reserved router ID": func(m *Manifest) { m.Route.RouterID = ^babel.RouterID(0) },
		"legacy":             func(m *Manifest) { m.Version = 1 },
		"withdrawn":          func(m *Manifest) { m.Withdraw = true },
		"missing origin":     func(m *Manifest) { m.Route = nil },
	} {
		t.Run(name, func(t *testing.T) {
			m := routeManifest(device, time.Now())
			mutate(&m)
			if len(authorizedOrigins([]Manifest{m}, device.Tenant())) != 0 {
				t.Fatal("invalid origin admitted from snapshot")
			}
		})
	}
}
