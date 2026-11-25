package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func (c *directClient) PushLatest(ctx context.Context, q *replicav1.PushLatestRequest, _ ...grpc.CallOption) (*replicav1.PushLatestReply, error) {
	c.calls.Add(1)
	return c.server.PushLatest(ctx, q)
}

type latestClientHook struct {
	replicav1.WorldReplicaServiceClient
	hook func(*replicav1.PushLatestReply, error) (*replicav1.PushLatestReply, error)
}

func TestLatestActualGRPCKeepsOriginalBatchAndCallerConnection(t *testing.T) {
	f := setup(t)
	listener := bufconn.Listen(MaxLatestBytes * 2)
	server := grpc.NewServer(grpc.MaxRecvMsgSize(MaxLatestBytes))
	replicav1.RegisterWorldReplicaServiceServer(server, f.b)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///latest-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	connection := Connection{identity(deviceB), replicav1.NewWorldReplicaServiceClient(conn), f.closed}
	items := []Item{recordItem(t, "gas", 1), leaseItem(t), floorItem(t, 8)}
	r, err := f.a.SendLatest(context.Background(), connection, items)
	if err != nil || !r.Complete || r.Admitted != len(items) || f.closed.closed.Load() != 0 {
		t.Fatal(r, err)
	}
	for _, original := range items {
		got := f.local.items[original.Entry.OrderKey()]
		if got.Entry != original.Entry || !bytes.Equal(got.Envelope, original.Envelope) {
			t.Fatal("gRPC changed original item")
		}
	}
	r, err = f.a.SendLatest(context.Background(), connection, []Item{recordItem(t, "gas", 2)})
	if err != nil || !r.Complete || r.Admitted != 1 {
		t.Fatal("caller connection was not reusable", r, err)
	}
}

func (c latestClientHook) PushLatest(ctx context.Context, q *replicav1.PushLatestRequest, opts ...grpc.CallOption) (*replicav1.PushLatestReply, error) {
	r, err := c.WorldReplicaServiceClient.PushLatest(ctx, q, opts...)
	return c.hook(r, err)
}

func TestLatestLostOrMalformedReplyDoesNotRetryOrFaultFreshValues(t *testing.T) {
	for _, mode := range []string{"lost", "nil", "scope", "generation", "flags", "count", "unknown-field"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			conn, client := latestConnection(f)
			conn.Client = latestClientHook{client, func(r *replicav1.PushLatestReply, err error) (*replicav1.PushLatestReply, error) {
				if err != nil {
					t.Fatal(err)
				}
				if !f.a.mu.TryLock() {
					t.Fatal("sender lock held during network call")
				}
				f.a.mu.Unlock()
				switch mode {
				case "lost":
					return nil, status.Error(codes.Unavailable, "reply lost")
				case "nil":
					return nil, nil
				case "scope":
					r.Scope.Device = deviceC
				case "generation":
					r.Generation = 0
				case "flags":
					r.UnknownOutcome = true
				case "count":
					r.Skipped++
				case "unknown-field":
					r.ProtoReflect().SetUnknown([]byte{0x50, 1})
				}
				return r, nil
			}}
			r, err := f.a.SendLatest(context.Background(), conn, []Item{recordItem(t, "gas", 1)})
			if !errors.Is(err, ErrUncertain) || !r.UnknownOutcome || r.Complete || client.calls.Load() != 1 || len(f.local.admitted) != 1 || f.closed.closed.Load() != 0 {
				t.Fatal(r, err)
			}
			conn.Client = client
			r, err = f.a.SendLatest(context.Background(), conn, []Item{recordItem(t, "gas", 2)})
			if err != nil || !r.Complete || r.Admitted != 1 || client.calls.Load() != 2 || len(f.local.admitted) != 2 {
				t.Fatal("fresh value blocked by remote delivery unknown", r, err)
			}
		})
	}
}

func TestLatestBoundsScopeAndExportRefuseBeforeSending(t *testing.T) {
	for _, mode := range []string{"empty", "count", "duplicate", "large", "changed-envelope", "third-origin", "scope", "export", "generation", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			conn, client := latestConnection(f)
			items := []Item{recordItem(t, "gas", 1)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "empty":
				items = nil
			case "count":
				for i := 0; i < MaxLatestItems; i++ {
					items = append(items, items[0])
				}
			case "duplicate":
				items = append(items, items[0])
			case "large":
				items = nil
				for i := 0; i < MaxLatestItems; i++ {
					item := recordItem(t, string(rune('a'+i)), 1)
					var signed serviceorigin.SignedRecord
					_ = json.Unmarshal(item.Envelope, &signed)
					var record serviceorigin.Record
					_ = json.Unmarshal(signed.Record, &record)
					record.Publication.Payload = bytes.Repeat([]byte{'x'}, 40000)
					signed.Record, _ = json.Marshal(record)
					item.Envelope, _ = json.Marshal(signed)
					item.Entry.Reference.RecordSHA256 = hash(signed.Record)
					item.Entry.Reference.EnvelopeSHA256 = hash(item.Envelope)
					if ValidateItem(item, endpoint(deviceA), deviceA) != nil {
						t.Fatal("oversize batch fixture invalid item")
					}
					items = append(items, item)
				}
			case "changed-envelope":
				items[0].Envelope[0] ^= 1
			case "third-origin":
				items[0].Entry.Reference.Key.Source.Device = deviceC
			case "scope":
				conn.Identity = Identity{}
			case "export":
				f.a.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) { return Access{Generation: 1}, nil }
			case "generation":
				f.a.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) {
					return Access{1, []proofref.Source{source(deviceA)}}, nil
				}
				f.generation.Store(2)
			case "canceled":
				cancel()
			}
			r, err := f.a.SendLatest(ctx, conn, items)
			if err == nil || r.UnknownOutcome || client.calls.Load() != 0 || len(f.local.admitted) != 0 {
				t.Fatal("invalid local batch sent", r, err)
			}
		})
	}
}

func TestLatestInboundValidatesWholeBatchBeforeAnyAdmission(t *testing.T) {
	for _, mode := range []string{"nil", "version", "scope", "order", "duplicate", "trailing-invalid", "unknown-field", "import", "identity"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			floor, lease, record := floorItem(t, 9), leaseItem(t), recordItem(t, "gas", 1)
			req := &replicav1.PushLatestRequest{Version: Version, Scope: scopeWire(endpoint(deviceA)), Items: []*replicav1.Item{{Entry: entryWire(floor.Entry), Envelope: floor.Envelope}, {Entry: entryWire(lease.Entry), Envelope: lease.Envelope}, {Entry: entryWire(record.Entry), Envelope: record.Envelope}}}
			switch mode {
			case "nil":
				req = nil
			case "version":
				req.Version++
			case "scope":
				req.Scope.Device = deviceC
			case "order":
				req.Items[0], req.Items[2] = req.Items[2], req.Items[0]
			case "duplicate":
				req.Items = append(req.Items, proto.Clone(req.Items[2]).(*replicav1.Item))
			case "trailing-invalid":
				req.Items[2].Envelope = []byte("malformed")
			case "unknown-field":
				req.Items[2].ProtoReflect().SetUnknown([]byte{0x18, 1})
			case "import":
				f.b.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) { return Access{Generation: 1}, nil }
			case "identity":
				f.b.cfg.IncomingPeer = func(context.Context) (Identity, error) { return Identity{}, nil }
			}
			if r, err := f.b.PushLatest(context.Background(), req); err == nil || r != nil || len(f.local.admitted) != 0 {
				t.Fatal("bad request partially admitted", r, err)
			}
		})
	}
}

func TestLatestSlotsAreIndependentOfRepairAndBoundPerPeer(t *testing.T) {
	f := setup(t)
	f.a.outbound[deviceB] = true
	f.b.inbound[deviceA] = true
	conn, _ := latestConnection(f)
	if r, e := f.a.SendLatest(context.Background(), conn, []Item{recordItem(t, "gas", 1)}); e != nil || !r.Complete {
		t.Fatal(r, e)
	}
	f.a.latestOutbound[deviceB] = true
	if _, e := f.a.SendLatest(context.Background(), conn, []Item{recordItem(t, "gas", 2)}); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	delete(f.a.latestOutbound, deviceB)
	f.b.latestInbound[deviceA] = true
	item := recordItem(t, "gas", 2)
	req := &replicav1.PushLatestRequest{Version: Version, Scope: scopeWire(endpoint(deviceA)), Items: []*replicav1.Item{{Entry: entryWire(item.Entry), Envelope: item.Envelope}}}
	if _, e := f.b.PushLatest(context.Background(), req); status.Code(e) != codes.ResourceExhausted {
		t.Fatal(e)
	}
}

func TestLatestRealSignedCaptureKeepsOriginalBytesAndPhysicalCap(t *testing.T) {
	scope, request, capture := captureRequest(t, "camera")
	now := time.Unix(0, capture.CapturedUnixNS).Add(100 * time.Millisecond)
	id := testidentity.NewWith(t, now, scope.Device)
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	clock := &captureClock{trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "latest-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-issued"}}
	publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 2, Lifetime: time.Minute, Clock: clock, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	signed, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: tenant, Site: scope.Site, ExecutionDomain: scope.ExecutionDomain, Certificates: certs, Clock: clock, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, d wendycert.DeviceIdentity) error {
		if !reflect.DeepEqual(c.Scope, scope) || c.Epoch != 2 || d.Principal != scope.Device {
			return ErrDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := verifier.Verify(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(signed)
	component := v.Component()
	item := Item{Entry: Entry{Kind: Record, Reference: proofref.Reference{Key: component.Key, Version: component.Version, EnvelopeSHA256: hash(raw), RecordSHA256: hash(signed.Record)}, ExpiresAtUnixNS: request.Timing.SampleTime.Add(request.TTL).UnixNano()}, Envelope: raw}
	until, err := RecordInterpretationUntil(v.Record)
	if err != nil || !until.Equal(time.Unix(0, capture.CapturedUnixNS).Add(sensorrecord.MaxPhysicalAge)) {
		t.Fatal(until, err)
	}
	for _, expired := range []bool{false, true} {
		f := setup(t)
		for _, s := range []*Service{f.a, f.b} {
			s.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) {
				return Access{1, []proofref.Source{item.Entry.Reference.Source()}}, nil
			}
		}
		f.b.cfg.RemoteAuthority = func(ctx context.Context, received Item) error {
			var original serviceorigin.SignedRecord
			if json.Unmarshal(received.Envelope, &original) != nil {
				return ErrProtocol
			}
			_, e := verifier.Verify(ctx, original)
			return e
		}
		f.local.admit = func(ctx context.Context, received Item, check func(context.Context) error) error {
			if !clock.window.Latest.Before(until) {
				return ErrRecordWindowDeferred
			}
			if err := check(ctx); err != nil {
				return err
			}
			f.local.admitted = append(f.local.admitted, cloneItem(received))
			return check(ctx)
		}
		if expired {
			clock.window.Earliest = now.Add(7 * time.Second)
			clock.window.Latest = now.Add(7*time.Second + time.Millisecond)
			clock.window.Monotonic += 7 * time.Second
		}
		conn, _ := latestConnection(f)
		r, e := f.a.SendLatest(context.Background(), conn, []Item{item})
		if expired {
			if !errors.Is(e, ErrAdmissionDeferred) || !r.Deferred || r.UnknownOutcome || len(f.local.admitted) != 0 {
				t.Fatal(r, e)
			}
		} else if e != nil || !r.Complete || !bytes.Equal(f.local.admitted[0].Envelope, raw) || f.local.admitted[0].Entry != item.Entry {
			t.Fatal("signed original changed", r, e)
		}
		if got, e := RecordInterpretationUntil(v.Record); e != nil || !got.Equal(until) {
			t.Fatal("receipt rebased expiry", got, e)
		}
	}
}

func latestConnection(f *fixture) (Connection, *directClient) {
	c := &directClient{server: f.b}
	return Connection{identity(deviceB), c, f.closed}, c
}

func TestLatestPushPreservesOriginalsAndOrdersAuthorityWithoutRepair(t *testing.T) {
	items := []Item{recordItem(t, "gas", 1), leaseItem(t), floorItem(t, 9)}
	f := setup(t, items...)
	f.remote.inventory = func() { t.Fatal("latest push performed inventory") }
	conn, client := latestConnection(f)
	r, err := f.a.SendLatest(context.Background(), conn, items)
	if err != nil || !r.Complete || r.Admitted != 3 || client.calls.Load() != 1 || f.closed.closed.Load() != 0 {
		t.Fatal(r, err)
	}
	for i, want := range []Item{items[2], items[1], items[0]} {
		got := f.local.admitted[i]
		if got.Entry != want.Entry || !bytes.Equal(got.Envelope, want.Envelope) {
			t.Fatal("original or authority order changed")
		}
	}
	r, err = f.a.SendLatest(context.Background(), conn, items)
	if err != nil || !r.Complete || r.Admitted != 0 || r.Skipped != 3 || len(f.local.admitted) != 3 {
		t.Fatal("duplicate push changed state", r, err)
	}
}

func TestLatestWindowRefusalPreservesUnknownAdmissionDistinction(t *testing.T) {
	for _, mode := range []string{"window", "unmarked", "marked-uncertain", "postcommit", "generation"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.local.admit = func(ctx context.Context, _ Item, check func(context.Context) error) error {
				switch mode {
				case "window":
					return ErrRecordWindowDeferred
				case "unmarked":
					return errors.New("unknown store error")
				case "marked-uncertain":
					return errors.Join(ErrRecordWindowDeferred, ErrUncertain)
				case "postcommit":
					_ = check(ctx)
					_ = check(ctx)
					return ErrRecordWindowDeferred
				case "generation":
					f.generation.Store(2)
					return check(ctx)
				}
				return nil
			}
			conn, client := latestConnection(f)
			r, err := f.a.SendLatest(context.Background(), conn, []Item{recordItem(t, "gas", 1)})
			if mode == "window" {
				if !errors.Is(err, ErrAdmissionDeferred) || !r.Deferred || r.UnknownOutcome {
					t.Fatal(r, err)
				}
			} else if !errors.Is(err, ErrUncertain) || !r.UnknownOutcome || r.Deferred {
				t.Fatal(r, err)
			}
			if client.calls.Load() != 1 || r.Complete || f.closed.closed.Load() != 0 {
				t.Fatal("replayed or closed caller connection")
			}
		})
	}
}
