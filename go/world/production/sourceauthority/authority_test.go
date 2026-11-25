package sourceauthority

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	bolt "go.etcd.io/bbolt"
)

var background = context.Background()

type testClock struct {
	mu   sync.Mutex
	w    trustedtime.Interval
	fail bool
}

func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return c.w, ErrClock
	}
	return c.w, nil
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Earliest = c.w.Earliest.Add(d)
	c.w.Latest = c.w.Latest.Add(d)
	c.w.Monotonic += d
}

type fixture struct {
	cfg            Config
	origin, remote *Store
	clock          *testClock
	signer         Signer
	identity       *testidentity.Fixture
	scope          serviceorigin.Scope
	record         serviceorigin.SignedRecord
	verified       serviceorigin.VerifiedRecord
	denied         bool
	hook           func()
	calls          int
}

func privateTemp(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func setup(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{clock: &testClock{w: trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "boot1", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-anchor"}}, signer: Signer{id.ChainDER, id.Key}, identity: id}
	f.scope = serviceorigin.Scope{Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.test.scalar", Service: "gas", DeploymentGeneration: 7, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}}}
	f.cfg = Config{Path: filepath.Join(privateTemp(t), "origin", "authority.db"), Tenant: id.Tenant, Site: "office", ExecutionDomain: "simulation", SampleClockDomain: "simulation:run1", Clock: f.clock, Certificates: certs, MaxSources: 4, MaxBytes: 1 << 20}
	f.cfg.AuthorizeLease = func(ctx context.Context, c LeaseClaims, id wendycert.DeviceIdentity) error {
		f.calls++
		if f.hook != nil {
			f.hook()
		}
		if ctx.Err() != nil || f.denied || !reflect.DeepEqual(c.Scope, f.scope) || id.Principal != f.scope.Device {
			return ErrDenied
		}
		return nil
	}
	f.cfg.AuthorizeFloor = func(ctx context.Context, c FloorClaims, id wendycert.DeviceIdentity) error {
		f.calls++
		if f.hook != nil {
			f.hook()
		}
		if ctx.Err() != nil || f.denied || c.Source != SourceOf(f.scope) || id.Principal != f.scope.Device {
			return ErrDenied
		}
		return nil
	}
	f.origin, e = Create(f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.origin.Close() })
	cfg := f.cfg
	cfg.Path = filepath.Join(privateTemp(t), "receiver", "authority.db")
	f.remote, e = Create(cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.remote.Close() })
	f.publish(t, 1, 10*time.Minute)
	return f
}
func (f *fixture) publish(t *testing.T, epoch uint64, lifetime time.Duration) {
	t.Helper()
	p, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: f.scope, Epoch: epoch, Lifetime: lifetime, Clock: f.clock, Certificates: f.cfg.Certificates, ChainDER: f.signer.ChainDER, DeviceKey: f.signer.DeviceKey})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	req := serviceorigin.PublishRequest{Entity: "gas-1", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas", Frame: world.FrameRef{ID: "office-floor", Revision: "1"}, Timing: world.Timing{SampleTime: time.Unix(10, 0).UTC(), ClockDomain: "simulation:run1", ErrorKnown: true}, TTL: 2 * time.Second, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "1", Simulation: &world.SimulationContext{RunID: "run1", ScenarioVersion: "1", Seed: 1, ClockDomain: "simulation:run1"}}, Payload: []byte("{ \"value\": 10, \"unit\": \"ppm\" }\n")}
	f.record, e = p.Publish(background, req)
	if e != nil {
		t.Fatal(e)
	}
	v, e := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: f.scope.Tenant, Site: f.scope.Site, ExecutionDomain: f.scope.ExecutionDomain, Clock: f.clock, Certificates: f.cfg.Certificates, Authorize: func(context.Context, serviceorigin.DelegationClaims, wendycert.DeviceIdentity) error { return nil }})
	if e != nil {
		t.Fatal(e)
	}
	f.verified, e = v.Verify(background, f.record)
	if e != nil {
		t.Fatal(e)
	}
}
func (f *fixture) lease(t *testing.T) SignedLease {
	t.Helper()
	p, e := f.origin.IssueLease(background, f.signer, LeaseRequest{f.record.Delegation, MaxLifetime})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func wire(t *testing.T, p SignedLease) []byte {
	t.Helper()
	b, e := EncodeLease(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func floorWire(t *testing.T, p SignedFloor) []byte {
	t.Helper()
	b, e := EncodeFloor(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func leaseClaims(t *testing.T, p SignedLease) LeaseClaims {
	t.Helper()
	_, c, e := DecodeLease(wire(t, p))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func floorClaims(t *testing.T, p SignedFloor) FloorClaims {
	t.Helper()
	_, c, e := DecodeFloor(floorWire(t, p))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func (tst *fixture) signLease(t *testing.T, c LeaseClaims) SignedLease {
	t.Helper()
	b, _ := json.Marshal(c)
	sig, e := tst.signer.DeviceKey.Sign(nil, append([]byte(leasePurpose), b...), &mldsa.Options{})
	if e != nil {
		t.Fatal(e)
	}
	return SignedLease{b, tst.signer.ChainDER, sig}
}

func TestLeaseExactBytesCurrentScopeAndReopen(t *testing.T) {
	f := setup(t)
	p := f.lease(t)
	original := wire(t, p)
	before, _ := json.Marshal(f.verified)
	if e := f.remote.AcceptLease(background, p); e != nil {
		t.Fatal(e)
	}
	if e := f.remote.AuthorizeRecord(background, p, f.verified); e != nil {
		t.Fatal(e)
	}
	got, e := f.remote.CurrentLease(background, SourceOf(f.scope))
	if e != nil || !bytes.Equal(original, wire(t, got)) {
		t.Fatal("changed original", e)
	}
	got.Claims[0] ^= 1
	again, e := f.remote.CurrentLease(background, SourceOf(f.scope))
	if e != nil || !bytes.Equal(original, wire(t, again)) {
		t.Fatal("shared bytes", e)
	}
	cfg := f.remote.cfg
	f.remote.Close()
	f.remote, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	got, e = f.remote.CurrentLease(background, SourceOf(f.scope))
	if e != nil || !bytes.Equal(original, wire(t, got)) {
		t.Fatal("reopen", e)
	}
	after, _ := json.Marshal(f.verified)
	if !bytes.Equal(before, after) {
		t.Fatal("authority mutated original sample or proof")
	}
	if e = f.remote.AcceptLease(background, p); e != nil {
		t.Fatal("exact duplicate", e)
	}
}
func TestRenewalDoesNotAuthorizeOldBindingOrChangeRecordExpiry(t *testing.T) {
	f := setup(t)
	f.publish(t, 2, 4*time.Second)
	oldRecord := f.verified
	p := f.lease(t)
	if e := f.remote.AcceptLease(background, p); e != nil {
		t.Fatal(e)
	}
	f.clock.advance(2 * time.Second)
	renewed := f.lease(t)
	if e := f.remote.AcceptLease(background, renewed); e != nil {
		t.Fatal(e)
	}
	if e := f.remote.AuthorizeRecord(background, p, oldRecord); e == nil {
		t.Fatal("old envelope remained current")
	}
	if e := f.remote.AuthorizeRecord(background, renewed, oldRecord); e != nil {
		t.Fatal(e)
	}
	f.clock.advance(2 * time.Second)
	if _, e := f.remote.CurrentLease(background, SourceOf(f.scope)); !errors.Is(e, ErrExpired) {
		t.Fatal("delegation expiry extended", e)
	}
	f.scope.DeploymentGeneration++
	f.publish(t, 3, 10*time.Minute)
	newLease := f.lease(t)
	if e := f.remote.AcceptLease(background, newLease); e != nil {
		t.Fatal(e)
	}
	if e := f.remote.AuthorizeRecord(background, newLease, oldRecord); e == nil {
		t.Fatal("new generation authorized old proof")
	}
}
func TestFloorRefreshPreservesActiveLeaseAndSurvivesExpiredAttestation(t *testing.T) {
	f := setup(t)
	old := f.lease(t)
	if e := f.remote.AcceptLease(background, old); e != nil {
		t.Fatal(e)
	}
	floor, e := f.origin.IssueFloor(background, f.signer, SourceOf(f.scope), 2)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.remote.AcceptFloor(background, floor); e != nil {
		t.Fatal(e)
	}
	if e = f.remote.AcceptLease(background, old); !errors.Is(e, ErrReplay) {
		t.Fatal("revoked lease", e)
	}
	f.scope.DeploymentGeneration++
	f.publish(t, 2, 10*time.Minute)
	active := f.lease(t)
	if e = f.remote.AcceptLease(background, active); e != nil {
		t.Fatal(e)
	}
	refresh, e := f.origin.RefreshFloor(background, f.signer, SourceOf(f.scope))
	if e != nil {
		t.Fatal(e)
	}
	a, b := floorClaims(t, floor), floorClaims(t, refresh)
	if a.ThroughRevision != b.ThroughRevision || a.MinimumEpoch != b.MinimumEpoch || b.AttestationRevision <= a.AttestationRevision {
		t.Fatal("refresh changed denial or did not advance attestation")
	}
	if e = f.remote.AcceptFloor(background, refresh); e != nil {
		t.Fatal(e)
	}
	if _, e = f.remote.CurrentLease(background, SourceOf(f.scope)); e != nil {
		t.Fatal("refresh revoked active lease", e)
	}
	if e = f.remote.AcceptFloor(background, floor); !errors.Is(e, ErrReplay) {
		t.Fatal("old floor replay", e)
	}
	f.clock.advance(31 * time.Second)
	cfg := f.remote.cfg
	f.remote.Close()
	f.remote, e = Open(cfg)
	if e != nil {
		t.Fatal("expired floor blocked reopen", e)
	}
	rows, e := f.remote.ListFloors(background)
	if e != nil || len(rows) != 1 || !bytes.Equal(floorWire(t, rows[0]), floorWire(t, refresh)) {
		t.Fatal("lost denial", e)
	}
	if e = f.remote.AcceptFloor(background, refresh); !errors.Is(e, ErrExpired) {
		t.Fatal("expired signature freshly admitted", e)
	}
	newRefresh, e := f.origin.RefreshFloor(background, f.signer, SourceOf(f.scope))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.remote.AcceptFloor(background, newRefresh); e != nil {
		t.Fatal(e)
	}
	if floorClaims(t, newRefresh).ThroughRevision != a.ThroughRevision {
		t.Fatal("refresh revoked later leases")
	}
}
func TestLeaseCanonicalPurposeBoundsAndCommissioningRefusals(t *testing.T) {
	f := setup(t)
	p := f.lease(t)
	c := leaseClaims(t, p)
	for name, change := range map[string]func(*LeaseClaims){"site": func(c *LeaseClaims) { c.Scope.Site = "other" }, "sample domain": func(c *LeaseClaims) { c.SampleClockDomain = "simulation:other" }, "physical domain": func(c *LeaseClaims) { c.Scope.ExecutionDomain = "physical"; c.SampleClockDomain = "utc" }, "app": func(c *LeaseClaims) { c.Scope.App = "other" }, "service": func(c *LeaseClaims) { c.Scope.Service = "other" }, "wildcard": func(c *LeaseClaims) {
		c.Scope.Publish = []serviceorigin.PublishScope{{ComponentType: "*", Stream: "gas"}}
	}, "zero epoch": func(c *LeaseClaims) { c.Epoch = 0 }, "long window": func(c *LeaseClaims) { c.NotAfterUnixMS++ }, "future": func(c *LeaseClaims) { c.NotBeforeUnixMS++; c.NotAfterUnixMS++ }} {
		t.Run(name, func(t *testing.T) {
			v := cloneLease(c)
			change(&v)
			if e := f.remote.AcceptLease(background, f.signLease(t, v)); e == nil {
				t.Fatal("accepted invalid grant")
			}
		})
	}
	bad := p
	bad.Claims = append(bytes.Clone(p.Claims), ' ')
	if _, e := EncodeLease(bad); e == nil {
		t.Fatal("noncanonical claims")
	}
	bad = p
	bad.Signature = bytes.Clone(p.Signature)
	bad.Signature[0] ^= 1
	if e := f.remote.AcceptLease(background, bad); e == nil {
		t.Fatal("forged signature")
	}
	bad = p
	bad.Signature, _ = f.signer.DeviceKey.Sign(nil, append([]byte(floorPurpose), p.Claims...), &mldsa.Options{})
	if e := f.remote.AcceptLease(background, bad); e == nil {
		t.Fatal("cross-purpose signature")
	}
	bad = p
	bad.ChainDER = [][]byte{bytes.Repeat([]byte{1}, wendycert.MaxCertificateBytes+1)}
	if e := f.remote.AcceptLease(background, bad); e == nil {
		t.Fatal("oversize chain")
	}
	f.denied = true
	if e := f.remote.AcceptLease(background, p); e == nil {
		t.Fatal("enrollment inferred permission")
	}
}
func TestIssueRejectsForgedDelegationAndWrongSigner(t *testing.T) {
	f := setup(t)
	d := f.record.Delegation
	d.Signature = bytes.Clone(d.Signature)
	d.Signature[0] ^= 1
	if _, e := f.origin.IssueLease(background, f.signer, LeaseRequest{d, MaxLifetime}); e == nil {
		t.Fatal("forged delegation")
	}
	k, e := mldsa.GenerateKey(mldsa.MLDSA65())
	if e != nil {
		t.Fatal(e)
	}
	signer := f.signer
	signer.DeviceKey = k
	if _, e = f.origin.IssueLease(background, signer, LeaseRequest{f.record.Delegation, MaxLifetime}); e == nil {
		t.Fatal("wrong key")
	}
	for _, duration := range []time.Duration{0, MaxLifetime + time.Millisecond} {
		if _, e = f.origin.IssueLease(background, f.signer, LeaseRequest{f.record.Delegation, duration}); e == nil {
			t.Fatal("bad lifetime")
		}
	}
}
func TestFinalPolicyClockAndCancellationChecks(t *testing.T) {
	for _, kind := range []string{"revoke", "cancel", "expire", "clock"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			p := f.lease(t)
			ctx, cancel := context.WithCancel(background)
			defer cancel()
			calls := 0
			f.hook = func() {
				calls++
				if calls == 2 {
					switch kind {
					case "revoke":
						f.denied = true
					case "cancel":
						cancel()
					case "expire":
						f.clock.advance(MaxLifetime)
					case "clock":
						f.clock.mu.Lock()
						f.clock.fail = true
						f.clock.mu.Unlock()
					}
				}
			}
			if e := f.remote.AcceptLease(ctx, p); e == nil {
				t.Fatal("final refusal ignored")
			}
			if len(f.remote.state.Sources) != 0 {
				t.Fatal("denied lease persisted")
			}
		})
	}
}
func TestUncertainCommitAndPostcommitRefusalFaultClosed(t *testing.T) {
	for _, kind := range []string{"uncertain", "expire", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			p := f.lease(t)
			ctx, cancel := context.WithCancel(background)
			defer cancel()
			f.remote.commit = func(fn func(*bolt.Tx) error) error {
				e := f.remote.db.Update(fn)
				if e != nil {
					return e
				}
				switch kind {
				case "uncertain":
					return errors.New("injected after commit")
				case "expire":
					f.clock.advance(MaxLifetime)
				case "cancel":
					cancel()
				}
				return nil
			}
			if e := f.remote.AcceptLease(ctx, p); !errors.Is(e, ErrFaulted) {
				t.Fatal("not unknown outcome", e)
			}
			if _, e := f.remote.CurrentLease(background, SourceOf(f.scope)); !errors.Is(e, ErrFaulted) {
				t.Fatal("continued after uncertain commit", e)
			}
			cfg := f.remote.cfg
			f.remote.Close()
			var e error
			f.remote, e = Open(cfg)
			if e != nil {
				t.Fatal(e)
			}
			rows, e := f.remote.Snapshot(background)
			if e != nil || len(rows) != 1 || rows[0].LeaseRevision != leaseClaims(t, p).AuthorityRevision {
				t.Fatal("lost committed highwater", e)
			}
		})
	}
}
func TestClockRollbackMarkerCorruptionAndCapacity(t *testing.T) {
	t.Run("rollback", func(t *testing.T) {
		f := setup(t)
		p := f.lease(t)
		if e := f.remote.AcceptLease(background, p); e != nil {
			t.Fatal(e)
		}
		cfg := f.remote.cfg
		f.remote.Close()
		f.clock.advance(-time.Millisecond)
		if _, e := Open(cfg); !errors.Is(e, ErrClock) {
			t.Fatal("rollback", e)
		}
	})
	t.Run("marker missing", func(t *testing.T) {
		f := setup(t)
		cfg := f.remote.cfg
		f.remote.Close()
		if e := os.Remove(cfg.Path + ".installation"); e != nil {
			t.Fatal(e)
		}
		if _, e := Open(cfg); !errors.Is(e, ErrNotInitialized) {
			t.Fatal(e)
		}
		if _, e := Create(cfg); e == nil {
			t.Fatal("recreated existing DB")
		}
	})
	t.Run("corrupt state", func(t *testing.T) {
		f := setup(t)
		cfg := f.remote.cfg
		if e := f.remote.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucket).Put(stateKey, []byte(`{}`)) }); e != nil {
			t.Fatal(e)
		}
		f.remote.Close()
		if _, e := Open(cfg); !errors.Is(e, ErrCorrupt) {
			t.Fatal(e)
		}
	})
	t.Run("capacity no eviction", func(t *testing.T) {
		f := setup(t)
		f.origin.cfg.MaxSources = 1
		f.lease(t)
		other := SourceOf(f.scope)
		other.Service = "other"
		if _, e := f.origin.IssueFloor(background, f.signer, other, 1); !errors.Is(e, ErrCapacity) {
			t.Fatal(e)
		}
		if len(f.origin.state.Sources) != 1 {
			t.Fatal("evicted source")
		}
	})
}
func TestFloorRefreshWithRenewedCertificateKeepsDurableCounter(t *testing.T) {
	f := setup(t)
	f.lease(t)
	floor, e := f.origin.IssueFloor(background, f.signer, SourceOf(f.scope), 2)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.remote.AcceptFloor(background, floor); e != nil {
		t.Fatal(e)
	}
	k, e := mldsa.GenerateKey(mldsa.MLDSA65())
	if e != nil {
		t.Fatal(e)
	}
	leaf := *f.identity.Leaf
	leaf.SerialNumber = big.NewInt(40)
	leaf.Raw = nil
	leaf.RawTBSCertificate = nil
	leaf.PublicKey = k.PublicKey()
	leaf.Signature = nil
	leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	newCert := testidentity.Issue(t, &leaf, f.identity.Issuer, k.Public(), f.identity.IssuerKey)
	signer := Signer{[][]byte{newCert.Raw, f.identity.Issuer.Raw}, k}
	refreshed, e := f.origin.RefreshFloor(background, signer, SourceOf(f.scope))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.remote.AcceptFloor(background, refreshed); e != nil {
		t.Fatal(e)
	}
	a, b := floorClaims(t, floor), floorClaims(t, refreshed)
	if b.AttestationRevision <= a.AttestationRevision || b.ThroughRevision != a.ThroughRevision {
		t.Fatal("certificate renewal reset counters")
	}
}

func TestRenewedLeaseSignerCannotHideDeniedOriginalDelegationKey(t *testing.T) {
	f := setup(t)
	k, e := mldsa.GenerateKey(mldsa.MLDSA65())
	if e != nil {
		t.Fatal(e)
	}
	leaf := *f.identity.Leaf
	leaf.SerialNumber = big.NewInt(41)
	leaf.Raw = nil
	leaf.RawTBSCertificate = nil
	leaf.PublicKey = k.PublicKey()
	leaf.Signature = nil
	cert := testidentity.Issue(t, &leaf, f.identity.Issuer, k.Public(), f.identity.IssuerKey)
	newSigner := Signer{[][]byte{cert.Raw, f.identity.Issuer.Raw}, k}
	w, _ := f.clock.Now()
	old, e := f.cfg.Certificates.Verify(f.signer.ChainDER, w.Earliest, w.Latest)
	if e != nil {
		t.Fatal(e)
	}
	base := f.origin.cfg.AuthorizeLease
	seenNew, seenOld := false, false
	f.origin.cfg.AuthorizeLease = func(ctx context.Context, c LeaseClaims, id wendycert.DeviceIdentity) error {
		if id.SigningKeySHA256 == old.SigningKeySHA256 {
			seenOld = true
			return ErrDenied
		}
		seenNew = true
		return base(ctx, c, id)
	}
	if _, e = f.origin.IssueLease(background, newSigner, LeaseRequest{f.record.Delegation, MaxLifetime}); !errors.Is(e, ErrDenied) {
		t.Fatal("denied original delegation certified by new key", e)
	}
	if !seenNew || !seenOld {
		t.Fatal("did not check both certificate identities")
	}
	rows, e := f.origin.Snapshot(background)
	if e != nil || len(rows) != 1 || rows[0].Revision != 1 || len(rows[0].Lease) != 0 {
		t.Fatal("failed issuance exposed authority or reused revision", e)
	}
}

func TestMonotonicFloorAndBindingCannotRegressWithValidSignature(t *testing.T) {
	f := setup(t)
	p := f.lease(t)
	if e := f.remote.AcceptLease(background, p); e != nil {
		t.Fatal(e)
	}
	c := leaseClaims(t, p)
	c.AuthorityRevision++
	c.DelegationSHA256 = hash([]byte("different delegation"))
	if e := f.remote.AcceptLease(background, f.signLease(t, c)); !errors.Is(e, ErrReplay) {
		t.Fatal("changed same-epoch delegation", e)
	}
	floor, e := f.origin.IssueFloor(background, f.signer, SourceOf(f.scope), 2)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.remote.AcceptFloor(background, floor); e != nil {
		t.Fatal(e)
	}
	for _, which := range []string{"epoch", "through", "attestation-conflict"} {
		t.Run(which, func(t *testing.T) {
			c := floorClaims(t, floor)
			if which != "attestation-conflict" {
				c.AttestationRevision++
			}
			switch which {
			case "epoch":
				c.MinimumEpoch--
			case "through":
				c.ThroughRevision--
			default:
				c.NotAfterUnixMS--
			}
			b, _ := json.Marshal(c)
			sig, e := f.signer.DeviceKey.Sign(nil, append([]byte(floorPurpose), b...), &mldsa.Options{})
			if e != nil {
				t.Fatal(e)
			}
			if e = f.remote.AcceptFloor(background, SignedFloor{b, f.signer.ChainDER, sig}); !errors.Is(e, ErrReplay) {
				t.Fatal("floor rollback/conflict", e)
			}
		})
	}
}

func TestProtectedStorageAndConfigurationCannotBeReplaced(t *testing.T) {
	f := setup(t)
	cfg := f.remote.cfg
	f.remote.Close()
	changed := cfg
	changed.SampleClockDomain = "simulation:other"
	if _, e := Open(changed); !errors.Is(e, ErrCorrupt) {
		t.Fatal("recommissioned without preserved history", e)
	}
	real := cfg.Path + ".real"
	if e := os.Rename(cfg.Path, real); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(real, cfg.Path); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(cfg); e == nil {
		t.Fatal("followed DB symlink")
	}
	if e := os.Remove(cfg.Path); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(real, cfg.Path); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Dir(cfg.Path), 0777); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(filepath.Dir(cfg.Path), 0700)
	if _, e := Open(cfg); e == nil {
		t.Fatal("unsafe ancestor accepted")
	}
}

func TestRemoteVerifierHookUsesRetainedAuthorityAndOriginalProof(t *testing.T) {
	f := setup(t)
	p := f.lease(t)
	v, e := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: f.cfg.Tenant, Site: f.cfg.Site, ExecutionDomain: f.cfg.ExecutionDomain, Clock: f.clock, Certificates: f.cfg.Certificates, Authorize: f.remote.AuthorizeDelegation})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = v.Verify(background, f.record); e == nil {
		t.Fatal("record verified without retained lease")
	}
	if e = f.remote.AcceptLease(background, p); e != nil {
		t.Fatal(e)
	}
	got, e := v.Verify(background, f.record)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got.Bytes, f.record.Record) {
		t.Fatal("original bytes changed")
	}
	if e = f.remote.AuthorizeRecord(background, p, got); e != nil {
		t.Fatal(e)
	}
	wrong := got.Delegation
	wrong.Epoch++
	if e = f.remote.AuthorizeDelegation(background, wrong, got.Device); e == nil {
		t.Fatal("different epoch authorized")
	}
	floor, e := f.origin.IssueFloor(background, f.signer, SourceOf(f.scope), 2)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.remote.AcceptFloor(background, floor); e != nil {
		t.Fatal(e)
	}
	if _, e = v.Verify(background, f.record); e == nil {
		t.Fatal("verifier bypassed retained revocation")
	}
}
