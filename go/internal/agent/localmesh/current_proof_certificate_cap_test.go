package localmesh

import (
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestCurrentProofOriginalPathForwardCorrectionRevokesDirectoryAuthority(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	selectedEnd := now.Add(30 * time.Second)
	d.cache.verify = func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{Device: device, NotAfter: selectedEnd}, nil
	}
	base := now
	d.certificateNow = func() time.Time { return base }
	m := routeManifest(device, now)
	m.Issued, m.Expires = now.Add(-50*time.Second).UnixMilli(), now.Add(10*time.Second).UnixMilli()
	wire := signRouteTestManifest(t, m, chain, key, now)
	if _, err := d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	selectedEnd = now.Add(time.Hour)
	if !d.RouteAuthorizations(now.Add(time.Microsecond))[*m.Route] {
		t.Fatal("longer trusted path prematurely revoked valid original proof")
	}
	base = now.Add(31 * time.Second)
	_, proofs := d.SnapshotWithRoutes(now.Add(2 * time.Microsecond))
	if _, ok := proofs[*m.Route]; ok {
		t.Fatal("CURRENT snapshot retained original-path-expired authority after a longer path renewal")
	}
	if len(d.Snapshot(now.Add(2*time.Microsecond))) != 0 || d.RouteAuthorizations(now.Add(2 * time.Microsecond))[*m.Route] {
		t.Fatal("CURRENT manifest/proof survived its restricted deadline")
	}
	if _, ok := d.CurrentRecordChain(wire, now.Add(2*time.Microsecond)); ok {
		t.Fatal("CURRENT signed body remained exportable beyond its original selected path")
	}
}

func TestCurrentProofOriginalPathExpiryRetiresActualNodeFIBAndExports(t *testing.T) {
	f := newHandoffActorFixture(t)
	n, source := f.nodes[1], f.nodes[0]
	now := time.Now()
	base, selectedEnd := now, now.Add(30*time.Second)
	verify := n.cache.verify
	n.cache.verify = func(chain [][]byte, at time.Time) (Identity, error) {
		id, err := verify(chain, at)
		if err == nil && id.Device == source.Credentials.Device {
			id.NotAfter = selectedEnd
		}
		return id, err
	}
	n.directory.certificateNow = func() time.Time { return base }
	m, err := DecodeManifest(f.ownProof(0).Body)
	if err != nil {
		t.Fatal(err)
	}
	m.Revision++
	m.Issued, m.Expires = now.Add(-50*time.Second).UnixMilli(), now.Add(10*time.Second).UnixMilli()
	wire, err := SignManifest(m, source.Credentials.Certificate.Certificate, source.Credentials.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	f.proof(1, wire) // Actual signature/identity admission and the real actor/FIB barrier.
	if len(n.directory.handoffs) != 0 {
		t.Fatal("fixture must exercise CURRENT, not held, authority")
	}
	f.assertForwarded(true, m.Route.Seqno)
	selectedEnd = now.Add(time.Hour) // Selected-path fixture result; signing leaf/key stay real and unchanged.
	f.step(1, babel.Tick{})
	f.assertForwarded(true, m.Route.Seqno)
	base = now.Add(31 * time.Second) // Receive/Babel clock has not advanced: no peer-loss confounder.
	f.step(1, babel.Tick{})
	view := n.Snapshot()
	if view.RouteAuthorizations[*m.Route] {
		t.Fatal("CURRENT actual Node still authorizes the route beyond original selected-path expiry")
	}
	f.assertForwarded(false, m.Route.Seqno)
	for _, manifest := range view.Devices {
		if manifest.Device == m.Device {
			t.Fatal("CURRENT Node exported expired remote management/service metadata")
		}
	}
	for _, record := range n.directory.Records(f.now()) {
		if record.Principal == wire.Principal {
			t.Fatal("CURRENT expired record remained eligible for gossip export")
		}
	}
	if _, ok := n.directory.CurrentRecordChain(wire, f.now()); ok {
		t.Fatal("CURRENT record loader exported expired selected-path authority")
	}
}

func TestCurrentProofPublicAndGossipBoundsAreCoherentAndDoNotRenew(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	base, selectedEnd := now, now.Add(30*time.Second)
	d.certificateNow = func() time.Time { return base }
	d.cache.verify = func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{Device: device, NotAfter: selectedEnd}, nil
	}
	m := routeManifest(device, now)
	m.Issued, m.Expires = now.Add(-50*time.Second).UnixMilli(), now.Add(10*time.Second).UnixMilli()
	wire := signRouteTestManifest(t, m, chain, key, now)
	if _, err := d.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	owner := wire.ownerKey()
	active, mark := d.entries[owner].activeUntil, d.history[owner]
	check := func(at, want time.Time) {
		t.Helper()
		manifests, proofs := d.SnapshotWithRoutes(at)
		if len(manifests) != 1 || !manifests[0].AdmittedUntil.Equal(want) || !proofs[*m.Route].Deadline.Equal(want) {
			t.Fatalf("current metadata and exact route proof have incoherent/unbounded expiry: %+v %+v want=%s", manifests, proofs, want)
		}
		if got, ok := d.RecordDeadline(wire, at); !ok || !got.Equal(want) {
			t.Fatalf("record loader cap=%s valid=%v want=%s", got, ok, want)
		}
		if d.entries[owner].activeUntil != active || d.history[owner] != mark {
			t.Fatal("effective certificate restriction changed original lease/watermark")
		}
	}
	check(now, now.Add(30*time.Second))
	selectedEnd = now.Add(time.Hour)
	base = now.Add(10 * time.Second)
	check(now.Add(2*time.Second), now.Add(22*time.Second))
	base = now.Add(-time.Minute)
	check(now.Add(3*time.Second), now.Add(22*time.Second))
	if changed, err := d.Accept(wire, now.Add(3*time.Second)); err != nil || changed {
		t.Fatal("duplicate renewed current body", changed, err)
	}
	check(now.Add(3*time.Second), now.Add(22*time.Second))
	// A later forward correction retires the original bound permanently.
	base = now.Add(31 * time.Second)
	if len(d.Snapshot(now.Add(4*time.Second))) != 0 {
		t.Fatal("forward correction did not retire current metadata")
	}
	base = now
	if changed, err := d.Accept(wire, now.Add(5*time.Second)); err != nil || changed {
		t.Fatal("duplicate resurrected expired current proof", changed, err)
	}
	if d.RouteAuthorizations(now.Add(5 * time.Second))[*m.Route] {
		t.Fatal("backward correction resurrected original current proof")
	}
	if _, ok := d.RecordDeadline(wire, now.Add(5*time.Second)); ok {
		t.Fatal("retired proof retained a gossip lease")
	}
	if d.history[owner] != mark || d.entries[owner].activeUntil != active {
		t.Fatal("retirement/duplicate changed original replay bounds")
	}
	// A genuinely newer signed publication under the currently valid longer
	// path creates its own lease; the retired body's watermark cannot roll back.
	m.Revision++
	m.Issued, m.Expires = now.UnixMilli(), now.Add(time.Minute).UnixMilli()
	next := signRouteTestManifest(t, m, chain, key, now.Add(6*time.Second))
	if changed, err := d.Accept(next, now.Add(6*time.Second)); err != nil || !changed {
		t.Fatal("new signed revision could not establish fresh authority", changed, err)
	}
	if !d.RouteAuthorizations(now.Add(6 * time.Second))[*m.Route] {
		t.Fatal("new valid publication missing authority")
	}
	if _, err := d.Accept(wire, now.Add(7*time.Second)); err != ErrStaleManifest {
		t.Fatalf("old body bypassed current replay floor: %v", err)
	}
}
