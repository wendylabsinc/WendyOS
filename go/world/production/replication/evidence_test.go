package replication

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func (c *directClient) FetchEvidence(ctx context.Context, q *replicav1.FetchEvidenceRequest, _ ...grpc.CallOption) (*replicav1.FetchEvidenceReply, error) {
	c.calls.Add(1)
	return c.server.FetchEvidence(ctx, q)
}

type evidenceClient struct {
	replicav1.WorldReplicaServiceClient
	fetch func(context.Context, *replicav1.FetchEvidenceRequest) (*replicav1.FetchEvidenceReply, error)
}

func (c evidenceClient) FetchEvidence(ctx context.Context, q *replicav1.FetchEvidenceRequest, _ ...grpc.CallOption) (*replicav1.FetchEvidenceReply, error) {
	return c.fetch(ctx, q)
}

func evidenceFixture(t *testing.T, data []byte) (*fixture, proofref.Source, serviceevidence.Reference) {
	t.Helper()
	f := setup(t)
	source := source(deviceA)
	source.Component, source.Stream = sensorrecord.ComponentType, "camera-b"
	owner := evidenceOwner(source)
	ownerHash, err := owner.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	ref := serviceevidence.Reference{Owner: owner, OwnerSHA256: ownerHash, SHA256: hash(data), Size: int64(len(data)), ContentType: "image/jpeg", RetainUntilUnixMS: time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC).UnixMilli()}
	for _, service := range []*Service{f.a, f.b} {
		service.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) {
			return Access{Generation: f.generation.Load(), Sources: []proofref.Source{source}}, nil
		}
	}
	f.a.cfg.ReadEvidence = func(_ context.Context, peer Identity, generation uint64, got proofref.Source, q serviceevidence.GetRequest) (serviceevidence.GetResponse, error) {
		if peer != identity(deviceB) || generation != f.generation.Load() || got != source || q.SHA256 != ref.SHA256 || q.Offset >= int64(len(data)) {
			return serviceevidence.GetResponse{}, ErrDenied
		}
		end := min(int64(len(data)), q.Offset+int64(q.Length))
		return serviceevidence.GetResponse{Reference: ref, Offset: q.Offset, Data: bytes.Clone(data[q.Offset:end])}, nil
	}
	return f, source, ref
}

func TestEvidenceRangesPreserveOriginalOwnerBytesAndRetention(t *testing.T) {
	data := bytes.Repeat([]byte("source-owned-media"), 7000)
	f, source, ref := evidenceFixture(t, data)
	for _, mime := range []string{"image/jpeg", "image/png", "audio/wav"} {
		t.Run(mime, func(t *testing.T) {
			read := f.a.cfg.ReadEvidence
			f.a.cfg.ReadEvidence = func(ctx context.Context, peer Identity, g uint64, s proofref.Source, q serviceevidence.GetRequest) (serviceevidence.GetResponse, error) {
				out, err := read(ctx, peer, g, s, q)
				out.ContentType = mime
				return out, err
			}
			defer func() { f.a.cfg.ReadEvidence = read }()
			var received []byte
			for int64(len(received)) < ref.Size {
				q := serviceevidence.GetRequest{SHA256: ref.SHA256, Offset: int64(len(received)), Length: MaxEvidenceRangeBytes}
				r, err := f.b.ReadPeerEvidence(context.Background(), deviceA, source, q)
				if err != nil || r.Owner != ref.Owner || r.OwnerSHA256 != ref.OwnerSHA256 || r.RetainUntilUnixMS != ref.RetainUntilUnixMS || r.ContentType != mime {
					t.Fatal("original metadata changed", err)
				}
				received = append(received, r.Data...)
			}
			if !bytes.Equal(received, data) || hash(received) != ref.SHA256 {
				t.Fatal("original media bytes changed")
			}
		})
	}
}

func TestEvidenceRejectsWrongPeerSourceGrantAndRangeBeforeRead(t *testing.T) {
	f, source, ref := evidenceFixture(t, []byte("media"))
	reads := 0
	original := f.a.cfg.ReadEvidence
	f.a.cfg.ReadEvidence = func(ctx context.Context, p Identity, g uint64, s proofref.Source, q serviceevidence.GetRequest) (serviceevidence.GetResponse, error) {
		reads++
		return original(ctx, p, g, s, q)
	}
	q := serviceevidence.GetRequest{SHA256: ref.SHA256, Length: 5}
	for _, change := range []func(*proofref.Source, *serviceevidence.GetRequest){
		func(s *proofref.Source, _ *serviceevidence.GetRequest) { s.Device = deviceC },
		func(s *proofref.Source, _ *serviceevidence.GetRequest) { s.Stream = "ungranted" },
		func(s *proofref.Source, _ *serviceevidence.GetRequest) { s.Component = ScalarComponent },
		func(_ *proofref.Source, q *serviceevidence.GetRequest) { q.Offset = -1 },
		func(_ *proofref.Source, q *serviceevidence.GetRequest) { q.Length = MaxEvidenceRangeBytes + 1 },
	} {
		s, request := source, q
		change(&s, &request)
		if got, err := f.b.ReadPeerEvidence(context.Background(), deviceA, s, request); err == nil || len(got.Data) != 0 {
			t.Fatal("invalid source or range accepted")
		}
	}
	if reads != 0 {
		t.Fatal("invalid request reached evidence store")
	}
	req := &replicav1.FetchEvidenceRequest{Version: Version, Source: evidenceSourceWire(source), Sha256: ref.SHA256, Length: 5}
	req.Source.Actor.Device = deviceC
	if _, err := f.a.FetchEvidence(context.Background(), req); status.Code(err) != codes.InvalidArgument || reads != 0 {
		t.Fatal("server forwarded a third origin", err)
	}
}

func TestEvidenceRefusesAlteredRepliesAndUnknownFields(t *testing.T) {
	for _, corrupt := range []func(*replicav1.FetchEvidenceReply){
		func(r *replicav1.FetchEvidenceReply) { r.Source.Actor.Device = deviceC },
		func(r *replicav1.FetchEvidenceReply) { r.OwnerSha256 = hash([]byte("other owner")) },
		func(r *replicav1.FetchEvidenceReply) { r.Sha256 = hash([]byte("other blob")) },
		func(r *replicav1.FetchEvidenceReply) { r.Offset++ },
		func(r *replicav1.FetchEvidenceReply) { r.Data = r.Data[:2] },
		func(r *replicav1.FetchEvidenceReply) { r.Size = MaxEvidenceBytes + 1 },
		func(r *replicav1.FetchEvidenceReply) { r.RetainUntilUnixMs = 0 },
		func(r *replicav1.FetchEvidenceReply) { r.ContentType = "application/octet-stream" },
		func(r *replicav1.FetchEvidenceReply) { r.Generation = 0 },
		func(r *replicav1.FetchEvidenceReply) { r.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) },
	} {
		f, source, ref := evidenceFixture(t, []byte("media"))
		f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
			return Connection{identity(deviceA), evidenceClient{WorldReplicaServiceClient: f.client, fetch: func(ctx context.Context, q *replicav1.FetchEvidenceRequest) (*replicav1.FetchEvidenceReply, error) {
				r, err := f.a.FetchEvidence(ctx, q)
				if err == nil {
					corrupt(r)
				}
				return r, err
			}}, f.closed}, nil
		}
		if out, err := f.b.ReadPeerEvidence(context.Background(), deviceA, source, serviceevidence.GetRequest{SHA256: ref.SHA256, Length: 5}); !errors.Is(err, ErrProtocol) || len(out.Data) != 0 {
			t.Fatal("corrupt response exposed data", err)
		}
	}
}

func TestEvidenceCurrentPolicyIsRecheckedAfterReadAndNetwork(t *testing.T) {
	for _, boundary := range []string{"server", "client"} {
		t.Run(boundary, func(t *testing.T) {
			f, source, ref := evidenceFixture(t, []byte("media"))
			if boundary == "server" {
				read := f.a.cfg.ReadEvidence
				f.a.cfg.ReadEvidence = func(ctx context.Context, p Identity, g uint64, s proofref.Source, q serviceevidence.GetRequest) (serviceevidence.GetResponse, error) {
					r, err := read(ctx, p, g, s, q)
					f.generation.Add(1)
					return r, err
				}
			} else {
				f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
					return Connection{identity(deviceA), evidenceClient{WorldReplicaServiceClient: f.client, fetch: func(ctx context.Context, q *replicav1.FetchEvidenceRequest) (*replicav1.FetchEvidenceReply, error) {
						r, err := f.a.FetchEvidence(ctx, q)
						f.generation.Add(1)
						return r, err
					}}, f.closed}, nil
				}
			}
			if out, err := f.b.ReadPeerEvidence(context.Background(), deviceA, source, serviceevidence.GetRequest{SHA256: ref.SHA256, Length: 5}); err == nil || len(out.Data) != 0 {
				t.Fatal("policy replacement left readable bytes")
			}
		})
	}
}

func TestEvidenceDoesNotHoldLocksAcrossNetworkOrShareRepairSlot(t *testing.T) {
	f, source, ref := evidenceFixture(t, []byte("media"))
	var policy sync.Mutex
	f.b.cfg.Current = func(context.Context, Identity, Direction, uint64) error {
		policy.Lock()
		defer policy.Unlock()
		return nil
	}
	checkUnlocked := func() {
		for _, lock := range []*sync.Mutex{&policy, &f.local.mu, &f.b.mu} {
			if !lock.TryLock() {
				t.Fatal("lock held across evidence network operation")
			}
			lock.Unlock()
		}
	}
	f.b.outbound[deviceA], f.a.inbound[deviceB] = true, true
	f.b.cfg.Dial = func(context.Context, string) (Connection, error) {
		checkUnlocked()
		return Connection{identity(deviceA), evidenceClient{WorldReplicaServiceClient: f.client, fetch: func(ctx context.Context, q *replicav1.FetchEvidenceRequest) (*replicav1.FetchEvidenceReply, error) {
			checkUnlocked()
			return f.a.FetchEvidence(ctx, q)
		}}, f.closed}, nil
	}
	if _, err := f.b.ReadPeerEvidence(context.Background(), deviceA, source, serviceevidence.GetRequest{SHA256: ref.SHA256, Length: 5}); err != nil {
		t.Fatal("ordinary evidence blocked by independent state repair", err)
	}
}

func TestEvidenceMissingStoreAndCanceledReadReturnNoBytes(t *testing.T) {
	f, source, ref := evidenceFixture(t, []byte("media"))
	f.a.cfg.ReadEvidence = nil
	q := serviceevidence.GetRequest{SHA256: ref.SHA256, Length: 5}
	if out, err := f.b.ReadPeerEvidence(context.Background(), deviceA, source, q); status.Code(err) != codes.Unimplemented || len(out.Data) != 0 {
		t.Fatal(out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	f.b.cfg.Dial = func(context.Context, string) (Connection, error) { called = true; return Connection{}, nil }
	if out, err := f.b.ReadPeerEvidence(ctx, deviceA, source, q); !errors.Is(err, context.Canceled) || called || len(out.Data) != 0 {
		t.Fatal("canceled request dialed or returned bytes", err)
	}
}

func TestEvidenceRangeThroughActualGRPC(t *testing.T) {
	data := bytes.Repeat([]byte("source-camera-frame"), 3000)
	f, source, ref := evidenceFixture(t, data)
	listener := bufconn.Listen(MaxEvidenceReplyBytes * 2)
	server := grpc.NewServer()
	replicav1.RegisterWorldReplicaServiceServer(server, f.a)
	defer server.Stop()
	defer listener.Close()
	go server.Serve(listener)
	f.b.cfg.Dial = func(ctx context.Context, device string) (Connection, error) {
		conn, err := grpc.NewClient("passthrough:///evidence-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
		if err != nil {
			return Connection{}, err
		}
		return Connection{Identity: identity(device), Client: replicav1.NewWorldReplicaServiceClient(conn), Close: conn}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := f.b.ReadPeerEvidence(ctx, deviceA, source, serviceevidence.GetRequest{SHA256: ref.SHA256, Offset: MaxEvidenceRangeBytes, Length: MaxEvidenceRangeBytes})
	if err != nil || !bytes.Equal(out.Data, data[MaxEvidenceRangeBytes:]) || out.Owner != ref.Owner || out.RetainUntilUnixMS != ref.RetainUntilUnixMS {
		t.Fatal("gRPC evidence range changed", err)
	}
}

func TestEvidenceBoundsConcurrentRangesAndReleasesCanceledSlot(t *testing.T) {
	f, source, ref := evidenceFixture(t, []byte("media"))
	f.a.evidenceSlots = make(chan struct{}, 1)
	entered := make(chan struct{})
	f.a.cfg.ReadEvidence = func(ctx context.Context, _ Identity, _ uint64, _ proofref.Source, _ serviceevidence.GetRequest) (serviceevidence.GetResponse, error) {
		close(entered)
		<-ctx.Done()
		return serviceevidence.GetResponse{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	q := serviceevidence.GetRequest{SHA256: ref.SHA256, Length: 5}
	go func() { _, err := f.b.ReadPeerEvidence(ctx, deviceA, source, q); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first range did not enter callback")
	}
	if out, err := f.b.ReadPeerEvidence(ctx, deviceA, source, q); !errors.Is(err, ErrBusy) || len(out.Data) != 0 {
		t.Fatal("parallel range exceeded per-peer bound", err)
	}
	request := &replicav1.FetchEvidenceRequest{Version: Version, Source: evidenceSourceWire(source), Sha256: ref.SHA256, Length: 5}
	if out, err := f.a.FetchEvidence(ctx, request); status.Code(err) != codes.ResourceExhausted || out != nil {
		t.Fatal("server admitted more ranges than its slots", err)
	}
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatal("cancellation was not preserved", err)
	}
	if len(f.b.evidenceOutbound) != 0 || len(f.a.evidenceInbound) != 0 || len(f.a.evidenceSlots) != 0 {
		t.Fatal("canceled range retained capacity")
	}
}
