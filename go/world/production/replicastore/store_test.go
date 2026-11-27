package replicastore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
)

const tenant = testidentity.Tenant
const deviceA = "spiffe://wendy.sh/tenant/" + tenant + "/device/22222222-2222-4222-8222-222222222222"
const deviceB = "spiffe://wendy.sh/tenant/" + tenant + "/device/33333333-3333-4333-8333-333333333333"
const deviceC = "spiffe://wendy.sh/tenant/" + tenant + "/device/44444444-4444-4444-8444-444444444444"

func endpoint(device string) replication.Endpoint {
	return replication.Endpoint{Tenant: tenant, Site: "office", Device: device, ExecutionDomain: "simulation", SampleClockDomain: "simulation:repair"}
}
func source(device string) proofref.Source {
	return proofref.Source{Tenant: tenant, Site: "office", Device: device, App: "com.example.sensor", Service: "sensor", Component: replication.ScalarComponent, Stream: "gas"}
}

type testClock struct {
	mu sync.Mutex
	w  trustedtime.Interval
}

func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w, nil
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Earliest = c.w.Earliest.Add(d)
	c.w.Latest = c.w.Latest.Add(d)
	c.w.Monotonic += d
}

type node struct {
	store        *Store
	cfg          Config
	authorityCfg sourceauthority.Config
	fence        sync.Mutex
	allowed      atomic.Bool
	generation   atomic.Uint64
	binding      atomic.Value
}
type fixture struct {
	a, b      *node
	clock     *testClock
	sample    atomic.Int64
	publisher *serviceorigin.Publisher
	scope     serviceorigin.Scope
	identity  *testidentity.Fixture
	request   serviceorigin.PublishRequest
}

func newFixture(t *testing.T) *fixture {
	return newFixtureComponent(t, replication.ScalarComponent, "gas")
}

func newFixtureComponent(t *testing.T, component, stream string) *fixture {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(1794400000, 0).UTC()
	f := &fixture{clock: &testClock{w: trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only-anchor"}}}
	f.sample.Store(time.Unix(20, 0).UnixNano())
	f.identity = testidentity.NewWith(t, now, deviceA)
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: tenant, Roots: [][]byte{f.identity.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	f.scope = serviceorigin.Scope{Tenant: tenant, Site: "office", Device: deviceA, App: source(deviceA).App, Service: source(deviceA).Service, DeploymentGeneration: 17, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: component, Stream: stream}}}
	selector := func(device string) proofref.Source {
		v := source(device)
		v.Component, v.Stream = component, stream
		return v
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	makeNode := func(device, peer string) *node {
		n := &node{}
		n.allowed.Store(true)
		n.generation.Store(1)
		n.binding.Store(strings.Repeat("a", 64))
		verify, e := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: tenant, Site: "office", ExecutionDomain: "simulation", Clock: f.clock, Certificates: certs, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
			if !reflect.DeepEqual(c.Scope, f.scope) || id.Principal != deviceA {
				return ErrDenied
			}
			return nil
		}})
		if e != nil {
			t.Fatal(e)
		}
		replica := world.Config{Tenant: tenant, Site: "office", ClockDomain: "simulation:repair", Now: func() time.Time { return time.Unix(0, f.sample.Load()) }, MaxEntries: 64, MaxFutureSkew: time.Millisecond, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "floor", Revision: "one"}}}
		suffix := device[len(device)-36:]
		local, e := servicejournal.Create(servicejournal.Config{Path: filepath.Join(root, suffix, "local.db"), Replica: replica, Verifier: verify, PhysicalClock: f.clock, MaxEntries: 64, MaxProofBytes: 4 << 20, AdmitSchema: func(_ context.Context, v serviceorigin.VerifiedRecord) error {
			if v.Record.Publication.ComponentType != component {
				return ErrDenied
			}
			return nil
		}})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = local.Close() })
		n.authorityCfg = sourceauthority.Config{Path: filepath.Join(root, suffix, "authority.db"), Tenant: tenant, Site: "office", ExecutionDomain: "simulation", SampleClockDomain: "simulation:repair", Clock: f.clock, Certificates: certs, MaxSources: 8, MaxBytes: 2 << 20, AuthorizeLease: func(_ context.Context, c sourceauthority.LeaseClaims, id wendycert.DeviceIdentity) error {
			if !n.allowed.Load() || !reflect.DeepEqual(c.Scope, f.scope) || id.Principal != deviceA {
				return ErrDenied
			}
			return nil
		}, AuthorizeFloor: func(_ context.Context, c sourceauthority.FloorClaims, id wendycert.DeviceIdentity) error {
			if !n.allowed.Load() || c.Source != sourceauthority.SourceOf(f.scope) || id.Principal != deviceA {
				return ErrDenied
			}
			return nil
		}}
		auth, e := sourceauthority.Create(n.authorityCfg)
		if e != nil {
			t.Fatal(e)
		}
		n.cfg = Config{Endpoint: endpoint(device), Peer: replication.Identity{Device: peer, BindingSHA256: strings.Repeat("a", 64)}, Generation: 1, Import: []proofref.Source{selector(peer)}, Export: []proofref.Source{selector(device)}, Local: local, Authority: auth, RemotePath: filepath.Join(root, suffix, "remote.db"), Replica: replica, Clock: f.clock, Certificates: certs, MaxEntries: 64, MaxProofBytes: 4 << 20, Guard: func(ctx context.Context, fn func() error) error {
			n.fence.Lock()
			defer n.fence.Unlock()
			if e := ctx.Err(); e != nil {
				return e
			}
			return fn()
		}, Authorize: func(_ context.Context, p replication.Identity, g uint64, _ replication.Direction, _ []proofref.Source, w trustedtime.Interval) (servicejournal.AuthorityWindow, error) {
			if !n.allowed.Load() || g != n.generation.Load() || p.Device != peer || p.BindingSHA256 != n.binding.Load().(string) {
				return servicejournal.AuthorityWindow{}, ErrDenied
			}
			return servicejournal.AuthorityWindow{NotBefore: now.Add(-time.Second), NotAfter: now.Add(time.Hour)}, nil
		}, AdmitSchema: func(context.Context, serviceorigin.VerifiedRecord) error { return nil }}
		n.store, e = Create(n.cfg)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = n.store.Close(); _ = n.cfg.Authority.Close() })
		return n
	}
	f.a, f.b = makeNode(deviceA, deviceB), makeNode(deviceB, deviceA)
	epoch, e := f.a.cfg.Local.ReserveEpoch(ctx, tenant, deviceA, f.scope.App, f.scope.Service)
	if e != nil {
		t.Fatal(e)
	}
	f.publisher, e = serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: f.scope, Epoch: epoch, Lifetime: time.Hour, Clock: f.clock, Certificates: certs, ChainDER: f.identity.ChainDER, DeviceKey: f.identity.Key})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = f.publisher.Close() })
	f.request = serviceorigin.PublishRequest{Entity: "gas", ComponentType: replication.ScalarComponent, Stream: "gas", Frame: world.FrameRef{ID: "floor", Revision: "one"}, Timing: world.Timing{SampleTime: time.Unix(0, f.sample.Load()), ClockDomain: "simulation:repair", ErrorKnown: true}, TTL: 3 * time.Second, Payload: []byte(`{"value":2,"unit":"ppm","sensor_type":"gas","calibration_revision":"r1","uncertainty":0.1}`), Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "test", Simulation: &world.SimulationContext{RunID: "repair", ScenarioVersion: "v1", Seed: 1, ClockDomain: "simulation:repair"}}}
	return f
}
func (f *fixture) lease(t *testing.T) {
	t.Helper()
	d, e := f.publisher.PublicDelegation()
	if e != nil {
		t.Fatal(e)
	}
	f.a.fence.Lock()
	defer f.a.fence.Unlock()
	if _, e = f.a.cfg.Authority.IssueLease(context.Background(), sourceauthority.Signer{ChainDER: f.identity.ChainDER, DeviceKey: f.identity.Key}, sourceauthority.LeaseRequest{Delegation: d, Lifetime: 30 * time.Second}); e != nil {
		t.Fatal(e)
	}
}
func (f *fixture) publish(t *testing.T) []byte {
	t.Helper()
	signed, e := f.publisher.Publish(context.Background(), f.request)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.a.cfg.Local.Admit(context.Background(), signed); e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(signed)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

type directClient struct {
	s      *replication.Service
	before func()
}

func (c directClient) Inventory(ctx context.Context, q *replicav1.InventoryRequest, _ ...grpc.CallOption) (*replicav1.InventoryReply, error) {
	if c.before != nil {
		c.before()
	}
	return c.s.Inventory(ctx, q)
}
func (c directClient) Fetch(ctx context.Context, q *replicav1.FetchRequest, _ ...grpc.CallOption) (*replicav1.FetchReply, error) {
	if c.before != nil {
		c.before()
	}
	return c.s.Fetch(ctx, q)
}

func (c directClient) FetchEvidence(ctx context.Context, q *replicav1.FetchEvidenceRequest, _ ...grpc.CallOption) (*replicav1.FetchEvidenceReply, error) {
	return c.s.FetchEvidence(ctx, q)
}
func (c directClient) PushLatest(ctx context.Context, q *replicav1.PushLatestRequest, _ ...grpc.CallOption) (*replicav1.PushLatestReply, error) {
	return c.s.PushLatest(ctx, q)
}
func (f *fixture) syncService(t *testing.T) *replication.Service {
	t.Helper()
	config := func(n *node) replication.Config {
		return replication.Config{Endpoint: n.cfg.Endpoint, Store: n.store, IncomingPeer: func(context.Context) (replication.Identity, error) { return n.cfg.Peer, nil }, Authorize: func(_ context.Context, p replication.Identity, d replication.Direction) (replication.Access, error) {
			if p != n.cfg.Peer || !n.allowed.Load() {
				return replication.Access{}, ErrDenied
			}
			sources := n.cfg.Import
			if d == replication.Export {
				sources = n.cfg.Export
			}
			return replication.Access{Generation: n.generation.Load(), Sources: sources}, nil
		}, Current: func(_ context.Context, p replication.Identity, _ replication.Direction, g uint64) error {
			if p != n.cfg.Peer || g != n.generation.Load() || !n.allowed.Load() {
				return ErrDenied
			}
			return nil
		}, RemoteAuthority: n.store.RemoteAuthority, Dial: func(context.Context, string) (replication.Connection, error) {
			return replication.Connection{}, ErrDenied
		}}
	}
	a, e := replication.New(config(f.a))
	if e != nil {
		t.Fatal(e)
	}
	cfg := config(f.b)
	cfg.Dial = func(context.Context, string) (replication.Connection, error) {
		check := func() {
			for _, n := range []*node{f.a, f.b} {
				if !n.fence.TryLock() {
					t.Fatal("host fence crossed network")
				}
				n.fence.Unlock()
			}
		}
		check()
		return replication.Connection{Identity: f.b.cfg.Peer, Client: directClient{a, check}, Close: io.NopCloser(strings.NewReader(""))}, nil
	}
	b, e := replication.New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestRealRepairExactBytesTombstoneFloorAndReopen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.lease(t)
	raw := f.publish(t)
	syncer := f.syncService(t)
	r, e := syncer.SyncPeer(ctx, deviceA)
	if e != nil || !r.Complete || r.Admitted != 2 {
		t.Fatal(r, e)
	}
	live, e := f.b.store.Snapshot(ctx)
	if e != nil || len(live) != 1 || !bytes.Equal(live[0].Envelope, raw) {
		t.Fatal(len(live), e)
	}
	original := live[0].Descriptor
	if r, e = syncer.SyncPeer(ctx, deviceA); e != nil || !r.Complete || r.Admitted != 0 {
		t.Fatal(r, e)
	}
	f.request.Tombstone = true
	f.request.Payload = nil
	removed := f.publish(t)
	if r, e = syncer.SyncPeer(ctx, deviceA); e != nil || r.Admitted != 1 {
		t.Fatal(r, e)
	}
	if live, e = f.b.store.Snapshot(ctx); e != nil || len(live) != 0 {
		t.Fatal("tombstone became live", len(live), e)
	}
	if need, e := f.b.store.Need(ctx, recordEntry(original)); e != nil || need {
		t.Fatal("old version requested after tombstone", need, e)
	}
	page, e := f.a.store.Inventory(ctx, f.a.cfg.Export, "", 64)
	if e != nil {
		t.Fatal(e)
	}
	for _, hint := range page.Entries {
		if hint.Kind == replication.Record {
			got, ok, e := f.a.store.Fetch(ctx, hint)
			if e != nil || !ok || !hint.Tombstone || !bytes.Equal(got.Envelope, removed) {
				t.Fatal(ok, e)
			}
		}
	}
	f.a.fence.Lock()
	_, e = f.a.cfg.Authority.IssueFloor(ctx, sourceauthority.Signer{ChainDER: f.identity.ChainDER, DeviceKey: f.identity.Key}, sourceauthority.SourceOf(f.scope), 2)
	f.a.fence.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	if r, e = syncer.SyncPeer(ctx, deviceA); e != nil || r.Admitted != 1 {
		t.Fatal("floor repair", r, e)
	}
	states, e := f.b.cfg.Authority.Snapshot(ctx)
	if e != nil || len(states) != 1 || states[0].ThroughRevision == 0 || states[0].MinimumEpoch != 2 {
		t.Fatal(states, e)
	}
	if e = f.b.store.Close(); e != nil {
		t.Fatal(e)
	}
	if e = f.b.cfg.Authority.Close(); e != nil {
		t.Fatal(e)
	}
	f.b.cfg.Authority, e = sourceauthority.Open(f.b.authorityCfg)
	if e != nil {
		t.Fatal(e)
	}
	f.b.store, e = Open(f.b.cfg)
	if e != nil {
		t.Fatal(e)
	}
	f.clock.advance(31 * time.Second)
	if _, e = f.b.cfg.Authority.CurrentLease(ctx, sourceauthority.SourceOf(f.scope)); e == nil {
		t.Fatal("expired floor signature removed permanent denial")
	}
	if live, e = f.b.store.Snapshot(ctx); e != nil || len(live) != 0 {
		t.Fatal(len(live), e)
	}
	// Import did not touch any local epoch reservation.
	if epoch, e := f.b.cfg.Local.ReserveEpoch(ctx, tenant, deviceA, f.scope.App, f.scope.Service); e != nil || epoch != 1 {
		t.Fatal("remote import advanced local reservation", epoch, e)
	}
}
func TestBoundedInventoryExactFetchAndOriginalCaps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.lease(t)
	for i := 0; i < 5; i++ {
		f.request.Entity = fmt.Sprintf("gas-%d", i)
		f.publish(t)
	}
	after := ""
	var entries []replication.Entry
	for range 8 {
		page, e := f.a.store.Inventory(ctx, f.a.cfg.Export, after, 2)
		if e != nil || len(page.Entries) > 2 {
			t.Fatal(page, e)
		}
		for _, hint := range page.Entries {
			if hint.OrderKey() <= after {
				t.Fatal("order regression")
			}
			after = hint.OrderKey()
			entries = append(entries, hint)
		}
		if !page.More {
			break
		}
	}
	if len(entries) != 6 {
		t.Fatal("bounded inventory lost suffix", len(entries))
	}
	for _, hint := range entries {
		item, ok, e := f.a.store.Fetch(ctx, hint)
		if e != nil || !ok {
			t.Fatal(ok, e)
		}
		if e = f.b.store.Admit(ctx, item, func(context.Context) error { return nil }); e != nil {
			t.Fatal(e)
		}
	}
	live, e := f.b.store.Snapshot(ctx)
	if e != nil || len(live) != 5 {
		t.Fatal(len(live), e)
	}
	hint := recordEntry(live[0].Descriptor)
	changed := hint
	changed.Reference.Version.Sequence++
	if _, ok, e := f.a.store.Fetch(ctx, changed); e != nil || ok {
		t.Fatal("substituted descriptor", ok, e)
	}
	item, ok, e := f.a.store.Fetch(ctx, hint)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	item.Entry.ExpiresAtUnixNS -= int64(time.Second)
	if e = f.b.store.Admit(ctx, item, func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
	f.sample.Store(item.Entry.ExpiresAtUnixNS)
	live, e = f.b.store.Snapshot(ctx)
	if e != nil || len(live) != 4 {
		t.Fatal("shortened cap ignored", len(live), e)
	}
	if e = f.b.store.Admit(ctx, replication.Item{Entry: hint, Envelope: item.Envelope}, func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
	live, e = f.b.store.Snapshot(ctx)
	if e != nil || len(live) != 4 {
		t.Fatal("duplicate renewed sample", len(live), e)
	}
}
func TestPostCommitFailureFaultsAdapterAndRetainedContextRefuses(t *testing.T) {
	for _, kind := range []replication.Kind{replication.Lease, replication.Record} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			f.lease(t)
			f.publish(t)
			page, e := f.a.store.Inventory(ctx, f.a.cfg.Export, "", 64)
			if e != nil {
				t.Fatal(e)
			}
			var target replication.Item
			for _, hint := range page.Entries {
				item, ok, e := f.a.store.Fetch(ctx, hint)
				if e != nil || !ok {
					t.Fatal(ok, e)
				}
				if hint.Kind == kind {
					target = item
					break
				}
				if e = f.b.store.Admit(ctx, item, func(context.Context) error { return nil }); e != nil {
					t.Fatal(e)
				}
			}
			calls := 0
			var retained context.Context
			e = f.b.store.Admit(ctx, target, func(ctx context.Context) error {
				calls++
				retained = ctx
				if calls == 2 {
					return errors.New("postcommit policy invalidation")
				}
				return nil
			})
			if !errors.Is(e, ErrFaulted) || !errors.Is(e, replication.ErrUncertain) || calls != 2 {
				t.Fatal(e, calls)
			}
			if f.b.store.inside(retained) {
				t.Fatal("operation context survived guard")
			}
			if e = f.b.store.RemoteAuthority(retained, target); e == nil {
				t.Fatal("retained context authorized new work")
			}
			if _, e = f.b.store.Need(ctx, target.Entry); !errors.Is(e, ErrFaulted) {
				t.Fatal(e)
			}
			if e = f.b.store.Close(); e != nil {
				t.Fatal(e)
			}
			f.b.store, e = Open(f.b.cfg)
			if e != nil {
				t.Fatal(e)
			}
			need, e := f.b.store.Need(ctx, target.Entry)
			if e != nil || need {
				t.Fatal("postcommit failure lost committed bytes", need, e)
			}
		})
	}
}
func TestPeerPolicyAndThirdOriginRefuseBeforeAdmission(t *testing.T) {
	for _, mode := range []string{"fingerprint", "generation", "policy", "third-origin", "wrong-stream"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			f.lease(t)
			page, e := f.a.store.Inventory(ctx, f.a.cfg.Export, "", 64)
			if e != nil {
				t.Fatal(e)
			}
			item, _, e := f.a.store.Fetch(ctx, page.Entries[0])
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "fingerprint":
				f.b.binding.Store(strings.Repeat("b", 64))
			case "generation":
				f.b.generation.Store(2)
			case "policy":
				f.b.allowed.Store(false)
			case "third-origin":
				item.Entry.Authority.Actor.Device = deviceC
			case "wrong-stream":
				f.b.store.cfg.Import[0].Stream = "temperature"
			}
			calls := 0
			e = f.b.store.Admit(ctx, item, func(context.Context) error { calls++; return nil })
			if e == nil || calls != 0 || f.b.store.faulted.Load() {
				t.Fatal("bad peer reached mutation", e, calls)
			}
			states, e := f.b.cfg.Authority.Snapshot(ctx)
			if e != nil || len(states) != 0 {
				t.Fatal(states, e)
			}
		})
	}
}
