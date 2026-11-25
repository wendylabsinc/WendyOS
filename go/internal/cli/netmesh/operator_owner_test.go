package netmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"math/big"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// Normal simultaneous installations of one real operator credential have
// independent origin revisions, without duplicating certificate cache entries.
func TestOperatorInstallationsKeepIndependentManifestLeases(t *testing.T) {
	var authority fixtureAuthority
	c := fixtureCredentials(t, &authority)
	operator := c[1]
	now := time.Now()
	cachePath := filepath.Join(t.TempDir(), "identities.json")
	cache, err := localmesh.OpenIdentityCache(cachePath, localmesh.DefaultCacheLimits(), c[0].Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(operator.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	elapsed := now
	directory, err := localmesh.NewDirectoryWithClock(operator.Device.Tenant(), 64, cache, func() time.Time { return elapsed })
	if err != nil {
		t.Fatal(err)
	}
	var receipts []localmesh.DirectoryReceipt
	receiptWrites := 0
	if err = directory.SetPersistence(nil, func(r []localmesh.DirectoryReceipt) error {
		receiptWrites++
		receipts = append([]localmesh.DirectoryReceipt(nil), r...)
		return nil
	}, now); err != nil {
		t.Fatal(err)
	}
	second, err := operator.Device.WithInstance(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondLeaf, err := x509.ParseCertificate(operator.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	secondLeaf.SerialNumber = big.NewInt(900)
	secondLeaf.NotAfter = secondLeaf.NotAfter.Add(-time.Minute)
	secondDER, err := x509.CreateCertificate(rand.Reader, secondLeaf, authority.certificate, secondKey.Public(), authority.key)
	if err != nil {
		t.Fatal(err)
	}
	secondChain := [][]byte{secondDER}
	if _, err = cache.Put(secondChain, now); err != nil {
		t.Fatal(err)
	}
	// Each installation owns its revision/checkpoint file. Received foreign
	// announcements never become persisted presence or reset this safety state.
	statePath := filepath.Join(t.TempDir(), "routing-state.json")
	stores := map[uuid.UUID]*localmesh.StateStore{}
	states := map[uuid.UUID]localmesh.PersistentState{}
	routerIDs := map[uuid.UUID]babel.RouterID{operator.Device.UUID(): 100, second.UUID(): 101}
	for i, instance := range []uuid.UUID{operator.Device.UUID(), second.UUID()} {
		owner, err := operator.Device.WithInstance(instance)
		if err != nil {
			t.Fatal(err)
		}
		store, state, err := localmesh.OpenDeviceStateStore(statePath, owner, routerIDs[instance])
		if err != nil {
			t.Fatal(err)
		}
		engine, err := babel.New(babel.Config{RouterID: routerIDs[instance], InitialSeqno: uint16(10 + i)})
		if err != nil {
			t.Fatal(err)
		}
		checkpoint := engine.Checkpoint()
		state.Babel = &checkpoint
		stores[instance], states[instance] = store, state
	}
	wires := map[uuid.UUID]localmesh.SignedManifest{}
	for _, peer := range []struct {
		instance uuid.UUID
		revision uint64
	}{{operator.Device.UUID(), 1}, {second.UUID(), 1}, {operator.Device.UUID(), 2}} {
		if peer.revision == 2 {
			elapsed = now.Add(time.Minute)
		}
		owner, err := operator.Device.WithInstance(peer.instance)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := meshoperator.ParsePrincipal(owner.Principal())
		if err != nil {
			t.Fatal(err)
		}
		ownerAddress := meshoperator.Endpoint{Operator: parsed, Instance: meshoperator.InstanceID(owner.UUID())}
		if owner.MeshAddress() != ownerAddress.Address() {
			t.Fatal("installation address derivation changed")
		}
		duration := 2 * time.Minute
		if owner.UUID() == second.UUID() {
			duration = 4 * time.Minute
		}
		m := localmesh.Manifest{Version: 4, Owner: &localmesh.OperatorOwner{Principal: owner.Principal(), Instance: owner.UUID()}, Revision: peer.revision, Issued: now.UnixMilli(), Expires: now.Add(duration).UnixMilli(), Route: &localmesh.RouteOrigin{Prefix: netip.PrefixFrom(owner.MeshAddress(), 128), RouterID: routerIDs[owner.UUID()], Seqno: 1}}
		chain, signer := operator.Certificate.Certificate, operator.Signer
		if owner.UUID() == second.UUID() {
			chain, signer = secondChain, secondKey
		}
		wire, err := localmesh.SignManifest(m, chain, signer, now)
		if err != nil {
			t.Fatal(err)
		}
		state := states[owner.UUID()]
		state.Revision = peer.revision
		if err := stores[owner.UUID()].Save(state); err != nil {
			t.Fatal(err)
		}
		states[owner.UUID()] = state
		if accepted, err := directory.Accept(wire, now); err != nil || !accepted {
			t.Fatalf("normal instance update: %v %v", accepted, err)
		}
		wires[owner.UUID()] = wire
	}
	seen := map[uuid.UUID]localmesh.Manifest{}
	for _, m := range directory.Snapshot(now) {
		seen[m.Owner.Instance] = m
	}
	if len(seen) != 2 || seen[operator.Device.UUID()].Revision != 2 || seen[second.UUID()].Revision != 1 {
		t.Fatal("installation revision namespaces were conflated", seen)
	}
	if seen[operator.Device.UUID()].LeaseRemaining(now) != 2*time.Minute || seen[second.UUID()].LeaseRemaining(now) != 3*time.Minute {
		t.Fatal("one installation renewed the other's lease")
	}
	if got := len(cache.Fingerprints(now, 64)); got != 1 {
		t.Fatalf("same-principal installations duplicated identity cache entries: %d", got)
	}
	if receiptWrites != 0 || len(receipts) != 0 {
		t.Fatal("foreign receive leases were persisted")
	}

	// The first instance expires at minute3; its compact RAM watermark
	// prevents replay while the independently admitted second remains live.
	elapsed = now.Add(3 * time.Minute)
	directory.Sweep(now)
	live := directory.Snapshot(now)
	if len(live) != 1 || live[0].Owner.Instance != second.UUID() || live[0].LeaseRemaining(now) != time.Minute {
		t.Fatal("independent local lease expiry was lost", live)
	}
	if _, err := directory.Accept(wires[operator.Device.UUID()], now); !errors.Is(err, localmesh.ErrStaleManifest) {
		t.Fatal("expired installation replay revived its lease", err)
	}
	if changed, err := directory.Accept(wires[second.UUID()], now); err != nil || changed {
		t.Fatal("duplicate installation refreshed its lease", changed, err)
	}
	elapsed = now.Add(4 * time.Minute)
	directory.Sweep(now)
	if len(directory.Records(now)) != 0 || len(directory.Snapshot(now)) != 0 || len(directory.RouteAuthorizations(now)) != 0 {
		t.Fatal("expired installation payload or route presence remained")
	}

	if err := cache.Flush(now); err != nil {
		t.Fatal(err)
	}
	restoredCache, err := localmesh.OpenIdentityCache(cachePath, localmesh.DefaultCacheLimits(), c[0].Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := restoredCache.Get(operator.Device.Principal(), now); !ok {
		t.Fatal("configured operator trust/cache material did not survive normal restore")
	}
	foreign := fixtureCredentials(t)[1]
	if _, err := restoredCache.Put(foreign.Certificate.Certificate, now); err == nil {
		t.Fatal("restore promoted an unrelated issuer into trusted authority")
	}
	for instance, previous := range states {
		owner, err := operator.Device.WithInstance(instance)
		if err != nil {
			t.Fatal(err)
		}
		_, state, err := localmesh.OpenDeviceStateStore(statePath, owner, routerIDs[instance])
		if err != nil {
			t.Fatal(err)
		}
		if state.Device != previous.Device || state.RouterID != previous.RouterID || state.Revision != previous.Revision || state.Babel == nil || state.Babel.RouterID != previous.Babel.RouterID || state.Babel.Seqno != previous.Babel.Seqno || len(state.Receipts) != 0 {
			t.Fatal("foreign lease cleanup changed own revision/checkpoint or persisted presence", state)
		}
	}
	restored, err := localmesh.NewDirectoryWithClock(operator.Device.Tenant(), 64, restoredCache, func() time.Time { return elapsed })
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.SetPersistence(receipts, func([]localmesh.DirectoryReceipt) error { receiptWrites++; return nil }, now); err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot(now)) != 0 || len(restored.Records(now)) != 0 || len(restored.RouteAuthorizations(now)) != 0 {
		t.Fatal("restart restored foreign presence")
	}
	// Public certificate parts are also RAM-bounded. New authenticated
	// neighbours present their chains again under the configured authority;
	// cached or presented certificate material alone creates no presence.
	for _, chain := range [][][]byte{operator.Certificate.Certificate, secondChain} {
		if _, err := restoredCache.Put(chain, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(restored.Records(now)) != 0 {
		t.Fatal("certificate presentation created foreign presence")
	}
	// RAM markers intentionally do not survive restart. Re-learning signed
	// bytes is a new local lease, not presence resurrected from a state file.
	for _, wire := range wires {
		if accepted, err := restored.Accept(wire, now); err != nil || !accepted {
			t.Fatal("normal post-restart first-learn failed", accepted, err)
		}
	}
	if len(restored.RouteAuthorizations(now)) != 2 || receiptWrites != 0 {
		t.Fatal("installation origins lost or foreign persistence resumed")
	}
}
