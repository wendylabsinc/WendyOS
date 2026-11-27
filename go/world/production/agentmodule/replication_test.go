package agentmodule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"google.golang.org/grpc"
)

// These fixtures exercise the generated service with real signatures/stores.
// The direct client makes no TLS/network claim; the private host tests cover
// routed mTLS and a separate Linux test covers the ordinary socket reader.
type moduleReplicaFixture struct {
	m        *Manager
	config   Config
	deps     Dependencies
	cert     *testidentity.Fixture
	identity *moduleIdentity
	clock    *moduleClock
	sample   atomic.Int64
	policyOK atomic.Bool
	peer     *moduleReplicaFixture
}
type moduleReplicaPeerKey struct{}
type moduleReplicaClient struct {
	server replicav1.WorldReplicaServiceServer
	chain  [][]byte
}

func (c moduleReplicaClient) Inventory(ctx context.Context, q *replicav1.InventoryRequest, _ ...grpc.CallOption) (*replicav1.InventoryReply, error) {
	return c.server.Inventory(context.WithValue(ctx, moduleReplicaPeerKey{}, c.chain), q)
}
func (c moduleReplicaClient) Fetch(ctx context.Context, q *replicav1.FetchRequest, _ ...grpc.CallOption) (*replicav1.FetchReply, error) {
	return c.server.Fetch(context.WithValue(ctx, moduleReplicaPeerKey{}, c.chain), q)
}
func (c moduleReplicaClient) FetchEvidence(ctx context.Context, q *replicav1.FetchEvidenceRequest, _ ...grpc.CallOption) (*replicav1.FetchEvidenceReply, error) {
	return c.server.FetchEvidence(context.WithValue(ctx, moduleReplicaPeerKey{}, c.chain), q)
}
func replicaFingerprint(chain [][]byte) string {
	d := sha256.Sum256(chain[0])
	return hex.EncodeToString(d[:])
}
func newReplicaPair(t *testing.T) (*moduleReplicaFixture, *moduleReplicaFixture) {
	return newReplicaPairConfigured(t, nil)
}

func newReplicaPairConfigured(t *testing.T, configure func(int, *Config)) (*moduleReplicaFixture, *moduleReplicaFixture) {
	t.Helper()
	device := []string{basePolicy().Device, "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/33333333-3333-4333-8333-333333333333"}
	var pair [2]*moduleReplicaFixture
	for i := range pair {
		cert := testidentity.NewWith(t, moduleTestNow, device[i])
		leaf := *cert.Leaf
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
		cert.Leaf = testidentity.Issue(t, &leaf, cert.Issuer, cert.Key.Public(), cert.IssuerKey)
		cert.ChainDER[0] = cert.Leaf.Raw
		pair[i] = &moduleReplicaFixture{cert: cert, identity: newModuleIdentity(cert), clock: moduleTestClock()}
		pair[i].sample.Store(moduleTestNow.Add(100 * time.Millisecond).UnixNano())
		pair[i].policyOK.Store(true)
	}
	for i, f := range pair {
		f.peer = pair[1-i]
		base, e := filepath.EvalSymlinks(os.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		dir, e := os.MkdirTemp(base, "ww-rep-")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		p := basePolicy()
		p.Device = device[i]
		source := serviceaccess.Source{App: "com.example.replica", Service: "sensor", ComponentType: replication.ScalarComponent, Stream: "gas"}
		remote := source
		remote.Device = f.peer.cert.Principal
		p.Grants = []Grant{{App: source.App, Service: source.Service, Publish: []serviceorigin.PublishScope{{ComponentType: source.ComponentType, Stream: source.Stream}}}, {App: source.App, Service: "reader", Subscribe: []serviceaccess.Source{source, remote}}}
		selector := func(d string) proofref.Source {
			return proofref.Source{Tenant: p.Tenant, Site: p.Site, Device: d, App: source.App, Service: source.Service, Component: source.ComponentType, Stream: source.Stream}
		}
		f.config = Config{Policy: p, Roots: [][]byte{f.cert.RootDER, f.peer.cert.RootDER}, Directory: filepath.Join(dir, "sockets"), JournalPath: filepath.Join(dir, "local", "world.db"), CreateJournal: true, CreateReplication: true, UID: os.Geteuid(), GID: os.Getegid(), Replication: &ReplicaConfig{AuthorityPath: filepath.Join(dir, "authority", "state.db"), Peers: []ReplicaPeer{{Device: f.peer.cert.Principal, CredentialSHA256: replicaFingerprint(f.peer.cert.ChainDER), Generation: 1, RemotePath: filepath.Join(dir, "remote", "world.db"), Import: []proofref.Source{selector(f.peer.cert.Principal)}, Export: []proofref.Source{selector(f.cert.Principal)}}}}}
		if configure != nil {
			configure(i, &f.config)
		}
		f.deps = Dependencies{Identity: f.identity, Clock: f.clock, SampleNow: func() time.Time { return time.Unix(0, f.sample.Load()) }, PolicyCurrent: func(context.Context) error {
			if !f.policyOK.Load() {
				return ErrDenied
			}
			return nil
		}, PeerCheck: func(context.Context, appservice.Peer, serviceorigin.Scope) error { return nil }}
		f.m, e = New(context.Background(), f.config, f.deps)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = f.m.Close() })
	}
	return pair[0], pair[1]
}
func (f *moduleReplicaFixture) transport() ReplicaTransport {
	return ReplicaTransport{IncomingPeer: func(ctx context.Context) (replication.Identity, error) {
		chain, _ := ctx.Value(moduleReplicaPeerKey{}).([][]byte)
		return f.m.AuthenticateReplicaPeer(ctx, chain)
	}, Dial: func(ctx context.Context, target string) (replication.Connection, error) {
		if target != f.peer.cert.Principal {
			return replication.Connection{}, ErrDenied
		}
		id, e := f.m.AuthenticateReplicaPeer(ctx, f.peer.cert.ChainDER)
		if e != nil {
			return replication.Connection{}, e
		}
		return replication.Connection{Identity: id, Client: moduleReplicaClient{server: f.peer.m.ReplicaService(), chain: f.cert.ChainDER}, Close: io.NopCloser(bytes.NewReader(nil))}, nil
	}}
}
func startReplicaPair(t *testing.T, a, b *moduleReplicaFixture) {
	t.Helper()
	for _, f := range []*moduleReplicaFixture{a, b} {
		if e := f.m.StartReplication(context.Background(), f.transport()); e != nil {
			t.Fatal(e)
		}
	}
}
func (f *moduleReplicaFixture) publisher(t *testing.T, index int, generation uint64) appservice.Publisher {
	t.Helper()
	g := f.config.Policy.Grants[index]
	scope := f.config.Policy.scope(g, generation)
	f.m.mu.Lock()
	f.m.bindings[bindingKey(g.App, g.Service)] = &binding{scope: scope, subscribe: normalizedSubscriptions(g.Subscribe)}
	f.m.mu.Unlock()
	p, e := f.m.publisher(context.Background(), scope)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}
func replicaRequest() serviceorigin.PublishRequest {
	return serviceorigin.PublishRequest{Entity: "gas", ComponentType: replication.ScalarComponent, Stream: "gas", Frame: basePolicy().Frames[0], Timing: world.Timing{SampleTime: moduleTestNow, ClockDomain: "utc", ErrorKnown: true}, TTL: 2 * time.Second, Provenance: world.Provenance{Origin: world.Measured, AppVersion: "fixture"}, Payload: []byte(`{"sensor_type":"gas","value":7,"unit":"ppm","calibration_revision":"gas-cal-1","uncertainty":0.5}`)}
}
func (f *moduleReplicaFixture) publish(t *testing.T, p appservice.Publisher, q serviceorigin.PublishRequest) serviceorigin.SignedRecord {
	t.Helper()
	proof, e := p.Publish(context.Background(), q)
	if e != nil {
		t.Fatal(e)
	}
	e = f.identity.WithCurrent(1, func() error { return f.m.admitArchived(context.Background(), proof) })
	if e != nil {
		t.Fatal(e)
	}
	return proof
}
func (f *moduleReplicaFixture) sync(t *testing.T) replication.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, e := f.m.SyncPeer(ctx, f.peer.cert.Principal)
	if e != nil || !result.Complete {
		t.Fatalf("repair: %+v %v", result, e)
	}
	return result
}
func (f *moduleReplicaFixture) live(t *testing.T) []serviceorigin.SignedRecord {
	t.Helper()
	values, e := f.m.replica.peers[f.peer.cert.Principal].store.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	var out []serviceorigin.SignedRecord
	for _, v := range values {
		var p serviceorigin.SignedRecord
		if json.Unmarshal(v.Envelope, &p) != nil {
			t.Fatal("bad envelope")
		}
		out = append(out, p)
	}
	return out
}

func TestReplicaModuleExactProofAndTombstoneRepair(t *testing.T) {
	a, b := newReplicaPair(t)
	if a.m.ReplicaService() == nil {
		t.Fatal("configured dispatcher missing before start")
	}
	if _, e := a.m.ReplicaService().Inventory(context.Background(), &replicav1.InventoryRequest{}); e == nil {
		t.Fatal("pre-start service accepted")
	}
	startReplicaPair(t, a, b)
	pa := a.publisher(t, 0, 1)
	pb := b.publisher(t, 0, 1)
	reader := b.publisher(t, 1, 1)
	_ = reader
	original := a.publish(t, pa, replicaRequest())
	local := b.publish(t, pb, replicaRequest())
	b.sync(t)
	live := b.live(t)
	if len(live) != 1 || !bytes.Equal(live[0].Record, original.Record) || !bytes.Equal(live[0].Signature, original.Signature) {
		t.Fatal("remote original changed")
	}
	scope := b.config.Policy.scope(b.config.Policy.Grants[1], 1)
	filtered, e := b.m.filterQueryCurrent(context.Background(), []serviceorigin.SignedRecord{local, original}, scope)
	if e != nil || len(filtered) != 2 {
		t.Fatalf("local/remote collision: %d %v", len(filtered), e)
	}
	q := replicaRequest()
	q.Tombstone = true
	q.Payload = nil
	a.publish(t, pa, q)
	b.sync(t)
	filtered, e = b.m.filterQueryCurrent(context.Background(), []serviceorigin.SignedRecord{local, original}, scope)
	if e != nil || len(filtered) != 1 || !bytes.Equal(filtered[0].Record, local.Record) {
		t.Fatalf("tombstone hid wrong device: %d %v", len(filtered), e)
	}
}

func TestReplicaModuleFloorOnlyRevocationAndReplacement(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	pa := a.publisher(t, 0, 1)
	original := a.publish(t, pa, replicaRequest())
	b.sync(t)
	a.m.Release(a.config.Policy.Grants[0].App, "sensor", 1)
	b.sync(t)
	if len(b.live(t)) != 0 {
		t.Fatal("floor did not hide retained original")
	}
	pa = a.publisher(t, 0, 2)
	replacement := a.publish(t, pa, replicaRequest())
	b.sync(t)
	live := b.live(t)
	if len(live) != 1 || bytes.Equal(live[0].Record, original.Record) || !bytes.Equal(live[0].Record, replacement.Record) {
		t.Fatal("replacement mixed old epoch")
	}
	states, e := a.m.replica.authority.Snapshot(context.Background())
	if e != nil || len(states) != 1 || states[0].MinimumEpoch != 2 || states[0].ThroughRevision == 0 {
		t.Fatalf("durable floor: %+v %v", states, e)
	}
}

func TestReplicaModuleSourceLeaseNeverRenewsSample(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	p := a.publisher(t, 0, 1)
	a.publish(t, p, replicaRequest())
	b.sync(t)
	a.sample.Store(moduleTestNow.Add(3 * time.Second).UnixNano())
	b.sample.Store(moduleTestNow.Add(3 * time.Second).UnixNano())
	if e := a.m.RefreshReplication(context.Background()); e != nil {
		t.Fatal(e)
	}
	b.sync(t)
	if len(b.live(t)) != 0 {
		t.Fatal("refreshed lease renewed expired sample")
	}
}

func TestReplicaModuleIdentityLossAndMissingStoreRefuse(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	a.identity.invalidate()
	if _, e := a.m.SyncPeer(context.Background(), b.cert.Principal); e == nil {
		t.Fatal("old local identity retained transport")
	}
	if _, e := a.m.AuthenticateReplicaPeer(context.Background(), b.cert.ChainDER); e == nil {
		t.Fatal("old identity authenticated peer")
	}
	_ = b.m.Close()
	b.config.CreateJournal = false
	b.config.CreateReplication = false
	if e := os.Remove(b.config.Replication.Peers[0].RemotePath); e != nil {
		t.Fatal(e)
	}
	if m, e := New(context.Background(), b.config, b.deps); e == nil {
		_ = m.Close()
		t.Fatal("missing store recreated")
	}
}

func TestReplicaModuleLeaseBounds(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	a.publisher(t, 0, 1)
	states, e := a.m.replica.authority.Snapshot(context.Background())
	if e != nil || len(states) != 1 {
		t.Fatal(e)
	}
	_, lease, e := sourceauthority.DecodeLease(states[0].Lease)
	if e != nil || time.Duration(lease.NotAfterUnixMS-lease.NotBeforeUnixMS)*time.Millisecond > sourceauthority.MaxLifetime {
		t.Fatal("unbounded source lease", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := a.m.SyncPeer(ctx, b.cert.Principal); e == nil {
		t.Fatal("canceled sync accepted")
	}
}
