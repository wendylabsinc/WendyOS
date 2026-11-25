package replication

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const deviceA = "spiffe://wendy.sh/tenant/" + tenant + "/device/22222222-2222-4222-8222-222222222222"
const deviceB = "spiffe://wendy.sh/tenant/" + tenant + "/device/33333333-3333-4333-8333-333333333333"
const deviceC = "spiffe://wendy.sh/tenant/" + tenant + "/device/44444444-4444-4444-8444-444444444444"

func endpoint(device string) Endpoint {
	return Endpoint{tenant, "office", device, "simulation", "simulation:replication-test"}
}
func identity(device string) Identity { return Identity{device, strings.Repeat("a", 64)} }
func source(device string) proofref.Source {
	return proofref.Source{Tenant: tenant, Site: "office", Device: device, App: "com.example.sensor", Service: "sensor", Component: ScalarComponent, Stream: "gas"}
}

// Test envelopes exercise exact-byte transport and structural refusal. Their
// signatures are deliberately not cryptographic credentials. These tests
// inject an explicit host authorizer; sourceauthority/serviceorigin separately
// qualify cryptography and durable authority, which this package cannot infer.
func recordItem(t *testing.T, entity string, seq uint64) Item {
	t.Helper()
	scope := serviceorigin.Scope{Tenant: tenant, Site: "office", Device: deviceA, App: "com.example.sensor", Service: "sensor", DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: ScalarComponent, Stream: "gas"}}}
	publication := serviceorigin.PublishRequest{Entity: entity, ComponentType: ScalarComponent, Stream: "gas", Frame: world.FrameRef{ID: "floor", Revision: "one"}, Timing: world.Timing{SampleTime: time.Unix(1794400000, 0), ClockDomain: endpoint(deviceA).SampleClockDomain, ErrorKnown: true}, TTL: 3 * time.Second, Payload: []byte(`{"value":1}`)}
	record := serviceorigin.Record{Version: 1, AuthorityDomain: "physical", Scope: scope, Epoch: 2, Sequence: seq, Publication: publication}
	raw, _ := json.Marshal(record)
	envelope, _ := json.Marshal(serviceorigin.SignedRecord{Record: raw, Signature: []byte("test-only")})
	ref := proofref.Reference{Key: world.Key{Tenant: tenant, Site: "office", Entity: entity, Type: ScalarComponent, Source: world.Source{Device: deviceA, App: scope.App, Service: scope.Service, Stream: "gas"}}, Version: world.Version{Epoch: 2, Sequence: seq}, EnvelopeSHA256: hash(envelope), RecordSHA256: hash(raw)}
	return Item{Entry: Entry{Kind: Record, Reference: ref, ExpiresAtUnixNS: publication.Timing.SampleTime.Add(publication.TTL).UnixNano()}, Envelope: envelope}
}
func floorItem(t *testing.T, revision uint64) Item {
	t.Helper()
	a := Actor{tenant, "office", deviceA, "com.example.sensor", "sensor"}
	c := sourceauthority.FloorClaims{Version: 1, AuthorityDomain: "physical", Source: sourceauthority.Source{Tenant: tenant, Device: deviceA, App: a.App, Service: a.Service}, Site: a.Site, ExecutionDomain: "simulation", SampleClockDomain: endpoint(deviceA).SampleClockDomain, AttestationRevision: revision, ThroughRevision: 7, MinimumEpoch: 2, NotBeforeUnixMS: 1794400000000, NotAfterUnixMS: 1794400030000}
	raw, _ := json.Marshal(c)
	envelope, err := sourceauthority.EncodeFloor(sourceauthority.SignedFloor{Claims: raw, ChainDER: [][]byte{{1}}, Signature: make([]byte, mldsa.MLDSA65SignatureSize)})
	if err != nil {
		t.Fatal(err)
	}
	return Item{Entry: Entry{Kind: Floor, Authority: AuthorityHint{Actor: a, Revision: revision, Epoch: 2, ThroughRevision: 7, EnvelopeSHA256: hash(envelope)}}, Envelope: envelope}
}
func leaseItem(t *testing.T) Item {
	t.Helper()
	a := Actor{tenant, "office", deviceA, "com.example.sensor", "sensor"}
	c := sourceauthority.LeaseClaims{Version: 1, AuthorityDomain: "physical", Scope: serviceorigin.Scope{Tenant: tenant, Site: a.Site, Device: deviceA, App: a.App, Service: a.Service, DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: ScalarComponent, Stream: "gas"}}}, SampleClockDomain: endpoint(deviceA).SampleClockDomain, Epoch: 2, DelegationSHA256: strings.Repeat("a", 64), AuthorityRevision: 10, NotBeforeUnixMS: 1794400000000, NotAfterUnixMS: 1794400030000}
	raw, _ := json.Marshal(c)
	envelope, err := sourceauthority.EncodeLease(sourceauthority.SignedLease{Claims: raw, ChainDER: [][]byte{{1}}, Signature: make([]byte, mldsa.MLDSA65SignatureSize)})
	if err != nil {
		t.Fatal(err)
	}
	return Item{Entry: Entry{Kind: Lease, Authority: AuthorityHint{Actor: a, Revision: 10, Epoch: 2, EnvelopeSHA256: hash(envelope)}}, Envelope: envelope}
}

type memoryStore struct {
	mu        sync.Mutex
	items     map[string]Item
	admitted  []Item
	admit     func(context.Context, Item, func(context.Context) error) error
	inventory func()
}

func store(items ...Item) *memoryStore {
	s := &memoryStore{items: map[string]Item{}}
	for _, i := range items {
		s.items[i.Entry.OrderKey()] = cloneItem(i)
	}
	return s
}
func (s *memoryStore) Inventory(_ context.Context, sources []proofref.Source, after string, limit int) (InventoryPage, error) {
	if s.inventory != nil {
		s.inventory()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []Entry
	for _, i := range s.items {
		if allowed(i.Entry, sources) && i.Entry.OrderKey() > after {
			entries = append(entries, i.Entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].OrderKey() < entries[j].OrderKey() })
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	return InventoryPage{entries, more}, nil
}
func (s *memoryStore) Fetch(_ context.Context, e Entry) (Item, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.items[e.OrderKey()]
	return cloneItem(i), ok && i.Entry == e, nil
}
func (s *memoryStore) Need(_ context.Context, e Entry) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.items[e.OrderKey()]
	return !ok || i.Entry != e, nil
}
func (s *memoryStore) Admit(ctx context.Context, i Item, check func(context.Context) error) error {
	if s.admit != nil {
		return s.admit(ctx, i, check)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := check(ctx); err != nil {
		return err
	}
	s.items[i.Entry.OrderKey()] = cloneItem(i)
	s.admitted = append(s.admitted, cloneItem(i))
	return nil
}

type closer struct{ closed atomic.Int32 }

func (c *closer) Close() error { c.closed.Add(1); return nil }

type directClient struct {
	server    *Service
	inventory func(*replicav1.InventoryReply)
	fetch     func(*replicav1.FetchReply)
	calls     atomic.Int32
}

func (c *directClient) Inventory(ctx context.Context, q *replicav1.InventoryRequest, _ ...grpc.CallOption) (*replicav1.InventoryReply, error) {
	c.calls.Add(1)
	p, e := c.server.Inventory(ctx, q)
	if e == nil && c.inventory != nil {
		c.inventory(p)
	}
	return p, e
}
func (c *directClient) Fetch(ctx context.Context, q *replicav1.FetchRequest, _ ...grpc.CallOption) (*replicav1.FetchReply, error) {
	c.calls.Add(1)
	p, e := c.server.Fetch(ctx, q)
	if e == nil && c.fetch != nil {
		c.fetch(p)
	}
	return p, e
}

type fixture struct {
	a, b          *Service
	remote, local *memoryStore
	client        *directClient
	closed        *closer
	generation    atomic.Uint64
}

func setup(t *testing.T, items ...Item) *fixture {
	t.Helper()
	f := &fixture{remote: store(items...), local: store(), closed: &closer{}}
	f.generation.Store(1)
	base := func(device, peer string, storage Store) Config {
		return Config{Endpoint: endpoint(device), Store: storage, IncomingPeer: func(context.Context) (Identity, error) { return identity(peer), nil }, Authorize: func(_ context.Context, _ Identity, d Direction) (Access, error) {
			origin := device
			if d == Import {
				origin = peer
			}
			return Access{f.generation.Load(), []proofref.Source{source(origin)}}, nil
		}, Current: func(_ context.Context, _ Identity, _ Direction, g uint64) error {
			if g != f.generation.Load() {
				return ErrDenied
			}
			return nil
		}, RemoteAuthority: func(context.Context, Item) error { return nil }, Dial: func(context.Context, string) (Connection, error) { return Connection{}, ErrDenied }}
	}
	var err error
	f.a, err = New(base(deviceA, deviceB, f.remote))
	if err != nil {
		t.Fatal(err)
	}
	f.client = &directClient{server: f.a}
	cfg := base(deviceB, deviceA, f.local)
	cfg.Dial = func(context.Context, string) (Connection, error) {
		return Connection{identity(deviceA), f.client, f.closed}, nil
	}
	f.b, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRepairPreservesExactOriginAndIndependentAuthorityOrder(t *testing.T) {
	record := recordItem(t, "gas", 1)
	floor := floorItem(t, 9)
	lease := leaseItem(t)
	f := setup(t, record, floor, lease)
	f.client.fetch = func(p *replicav1.FetchReply) {
		for i, j := 0, len(p.Items)-1; i < j; i, j = i+1, j-1 {
			p.Items[i], p.Items[j] = p.Items[j], p.Items[i]
		}
	}
	result, err := f.b.SyncPeer(context.Background(), deviceA)
	if err != nil || !result.Complete || result.Admitted != 3 || f.closed.closed.Load() != 1 {
		t.Fatal(result, err)
	}
	for n, want := range []Item{floor, lease, record} {
		got := f.local.admitted[n]
		if got.Entry != want.Entry || !bytes.Equal(got.Envelope, want.Envelope) {
			t.Fatal("changed origin envelope or authority ordering")
		}
	}
	result, err = f.b.SyncPeer(context.Background(), deviceA)
	if err != nil || !result.Complete || result.Admitted != 0 {
		t.Fatal("duplicate repair changed state", result, err)
	}
}
func TestFloorOnlyRepairAndFreshAttestationDoNotNeedRecords(t *testing.T) {
	f := setup(t, floorItem(t, 9))
	first, err := f.b.SyncPeer(context.Background(), deviceA)
	if err != nil || first.Admitted != 1 {
		t.Fatal(first, err)
	}
	refresh := floorItem(t, 11)
	f.remote.items[refresh.Entry.OrderKey()] = refresh
	next, err := f.b.SyncPeer(context.Background(), deviceA)
	if err != nil || next.Admitted != 1 {
		t.Fatal(next, err)
	}
	if f.local.admitted[0].Entry.Authority.ThroughRevision != f.local.admitted[1].Entry.Authority.ThroughRevision || f.local.admitted[1].Entry.Authority.Revision != 11 {
		t.Fatal("re-attestation changed denial ordering")
	}
}
func TestMalformedOrMismatchedRepliesNeverAdmit(t *testing.T) {
	cases := map[string]func(*fixture){
		"scope":     func(f *fixture) { f.client.inventory = func(p *replicav1.InventoryReply) { p.Scope.Device = deviceC } },
		"enum-wrap": func(f *fixture) { f.client.inventory = func(p *replicav1.InventoryReply) { p.Entries[0].Kind = 259 } },
		"unknown": func(f *fixture) {
			f.client.inventory = func(p *replicav1.InventoryReply) { p.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) }
		},
		"nested-unknown": func(f *fixture) {
			f.client.inventory = func(p *replicav1.InventoryReply) {
				p.Entries[0].GetRecord().ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
			}
		},
		"duplicate": func(f *fixture) {
			f.client.inventory = func(p *replicav1.InventoryReply) { p.Entries = append(p.Entries, p.Entries[0]) }
		},
		"generation": func(f *fixture) { f.client.fetch = func(p *replicav1.FetchReply) { p.Generation++ } },
		"hash":       func(f *fixture) { f.client.fetch = func(p *replicav1.FetchReply) { p.Items[0].Envelope[0] ^= 1 } },
		"version": func(f *fixture) {
			f.client.fetch = func(p *replicav1.FetchReply) { p.Items[0].Entry.GetRecord().Reference.Sequence++ }
		},
		"missing": func(f *fixture) { f.client.fetch = func(p *replicav1.FetchReply) { p.Items = nil } },
		"duplicate-result": func(f *fixture) {
			f.client.fetch = func(p *replicav1.FetchReply) { p.Items = append(p.Items, p.Items[0]) }
		},
		"oversize": func(f *fixture) {
			f.client.fetch = func(p *replicav1.FetchReply) { p.Items[0].Envelope = make([]byte, MaxPageBytes) }
		},
		"expiry-extension": func(f *fixture) {
			for key, item := range f.remote.items {
				item.Entry.ExpiresAtUnixNS++
				f.remote.items[key] = item
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t, recordItem(t, "gas", 1))
			mutate(f)
			result, err := f.b.SyncPeer(context.Background(), deviceA)
			if err == nil || result.Complete || len(f.local.admitted) != 0 {
				t.Fatal("malformed response admitted", result, err)
			}
		})
	}
}
func TestPolicyReplacementAndCommitFenceFailClosed(t *testing.T) {
	for _, mode := range []string{"export-policy", "after-fetch", "remote-authority", "missing-check", "excess-checks", "uncertain", "cancel-at-commit"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, recordItem(t, "gas", 1))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "export-policy":
				f.remote.inventory = func() { f.generation.Add(1) }
			case "after-fetch":
				f.client.fetch = func(*replicav1.FetchReply) { f.generation.Add(1) }
			case "remote-authority":
				f.b.cfg.RemoteAuthority = func(context.Context, Item) error { return ErrDenied }
			case "missing-check":
				f.local.admit = func(context.Context, Item, func(context.Context) error) error { return nil }
			case "excess-checks":
				f.local.admit = func(ctx context.Context, _ Item, check func(context.Context) error) error {
					for range MaxFinalChecks + 1 {
						_ = check(ctx)
					}
					return nil
				}
			case "uncertain":
				f.local.admit = func(ctx context.Context, _ Item, check func(context.Context) error) error {
					if e := check(ctx); e != nil {
						return e
					}
					return errors.New("commit outcome unknown")
				}
			case "cancel-at-commit":
				f.local.admit = func(ctx context.Context, _ Item, check func(context.Context) error) error {
					cancel()
					return check(ctx)
				}
			}
			result, err := f.b.SyncPeer(ctx, deviceA)
			if err == nil || result.Complete || result.Admitted != 0 || f.closed.closed.Load() != 1 {
				t.Fatal(result, err)
			}
			if mode == "missing-check" || mode == "excess-checks" || mode == "uncertain" || mode == "cancel-at-commit" {
				if !result.UnknownOutcome || !errors.Is(err, ErrUncertain) {
					t.Fatal("uncertain admission acknowledged", result, err)
				}
			}
		})
	}
}
func TestCursorPinsPeerBindingGenerationLifetimeAndOrder(t *testing.T) {
	f := setup(t)
	p := identity(deviceB)
	after := recordItem(t, "gas", 1).Entry.OrderKey()
	now := time.Second
	f.a.elapsed = func() time.Duration { return now }
	raw, err := f.a.encodeCursor(p, 1, after)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.a.decodeCursor(raw, p, 1); err != nil || got != after {
		t.Fatal(got, err)
	}
	for _, mode := range []string{"peer", "binding", "generation", "expired", "tamper", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			copy := append([]byte(nil), raw...)
			peer := p
			g := uint64(1)
			now = time.Second
			switch mode {
			case "peer":
				peer.Device = deviceC
			case "binding":
				peer.BindingSHA256 = strings.Repeat("b", 64)
			case "generation":
				g = 2
			case "expired":
				now += 2 * time.Minute
			case "tamper":
				copy[0] ^= 1
			case "oversize":
				copy = make([]byte, MaxCursorBytes+1)
			}
			if _, err := f.a.decodeCursor(copy, peer, g); err == nil {
				t.Fatal("foreign/stale cursor accepted")
			}
		})
	}
}
func TestOneRepairPerPeerAndCancellationReleaseCapacity(t *testing.T) {
	f := setup(t)
	f.b.cfg.Limits.MaxPeers = 1
	entered := make(chan struct{})
	var once sync.Once
	f.b.cfg.Dial = func(ctx context.Context, _ string) (Connection, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return Connection{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := f.b.SyncPeer(ctx, deviceA); done <- e }()
	<-entered
	if _, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	if _, e := f.b.SyncPeer(context.Background(), deviceC); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
		return Connection{identity(deviceA), f.client, f.closed}, nil
	}
	if r, e := f.b.SyncPeer(context.Background(), deviceA); e != nil || !r.Complete {
		t.Fatal(r, e)
	}
}
func TestRoundEntryAndPageQuotasAndUnavailableHaveNoRetries(t *testing.T) {
	items := make([]Item, 65)
	for i := range items {
		items[i] = recordItem(t, fmt.Sprintf("entity-%03d", i), 1)
	}
	f := setup(t, items...)
	f.b.cfg.Limits.MaxEntries = 1
	if r, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrCapacity) || r.Admitted != 1 {
		t.Fatal(r, e)
	}
	f = setup(t, items...)
	f.b.cfg.Limits.MaxPages = 1
	if r, e := f.b.SyncPeer(context.Background(), deviceA); !errors.Is(e, ErrCapacity) || r.Pages != 1 || r.Complete {
		t.Fatal(r, e)
	}
	f = setup(t, items[0])
	f.client.fetch = func(p *replicav1.FetchReply) { p.Unavailable = []*replicav1.Entry{p.Items[0].Entry}; p.Items = nil }
	r, e := f.b.SyncPeer(context.Background(), deviceA)
	if e != nil || r.Complete || r.Unavailable != 1 || f.client.calls.Load() != 2 {
		t.Fatal(r, e, "unexpected retry")
	}
}
func TestGRPCWirePreservesExactBytesWithInjectedPeer(t *testing.T) {
	f := setup(t, recordItem(t, "gas", 1))
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.MaxRecvMsgSize(MaxInventoryBytes), grpc.MaxSendMsgSize(MaxPageBytes))
	replicav1.RegisterWorldReplicaServiceServer(server, f.a)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///replica-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
		return Connection{identity(deviceA), replicav1.NewWorldReplicaServiceClient(conn), io.NopCloser(strings.NewReader(""))}, nil
	}
	r, e := f.b.SyncPeer(context.Background(), deviceA)
	if e != nil || !r.Complete || r.Admitted != 1 {
		t.Fatal(r, e)
	}
	if !bytes.Equal(f.local.admitted[0].Envelope, f.remote.items[f.local.admitted[0].Entry.OrderKey()].Envelope) {
		t.Fatal("gRPC rewrote signed bytes")
	}
}
