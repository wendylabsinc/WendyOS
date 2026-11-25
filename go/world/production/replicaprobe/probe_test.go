package replicaprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type testTime struct{ time.Time }

func (c testTime) Now() (trustedtime.Interval, error) {
	return trustedtime.Interval{Earliest: c.Time, Latest: c.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-commissioned-clock"}, nil
}

type rpcFixture struct {
	appv1.WorldAppServiceClient
	cfg                                  Config
	now                                  time.Time
	sample                               time.Time
	local, remote                        *serviceorigin.Publisher
	clockCalls, publishCalls, queryCalls int
	clockMutation                        func(*appv1.ClockReply)
	mutation                             func(*appv1.PublishReply)
	query                                *appv1.QueryReply
	publishError                         bool
	nonces                               [][]byte
	requests                             []*appv1.PublishRequest
}

func fixture(t *testing.T) (*Probe, *rpcFixture) {
	t.Helper()
	c := Config{Version: 1, Tenant: testidentity.Tenant, Site: "office", Device: "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/22222222-2222-4222-8222-222222222222", PeerDevice: "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/33333333-3333-4333-8333-333333333333", RunID: "pair-test", ScenarioVersion: "native-repair-v1", Seed: 7, Frame: world.FrameRef{ID: "office", Revision: "v1"}, SampleClockDomain: "simulation:pair-test", DurationMS: 1000, MaxCommands: 8, PollIntervalMS: 10}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	f := &rpcFixture{cfg: c, now: now, sample: now.Add(-3 * time.Hour), query: &appv1.QueryReply{}}
	for i, device := range []string{c.Device, c.PeerDevice} {
		id := testidentity.NewWith(t, now, device)
		certs, e := wendycert.NewDevice(wendycert.Config{Tenant: c.Tenant, Roots: [][]byte{id.RootDER}})
		if e != nil {
			t.Fatal(e)
		}
		p, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: device, App: App, Service: Service, DeploymentGeneration: 9, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: Component, Stream: Stream}}}, Epoch: 41, Lifetime: time.Minute, Clock: testTime{now}, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = p.Close() })
		if i == 0 {
			f.local = p
		} else {
			f.remote = p
		}
	}
	p, e := New(c, f)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, f
}
func (f *rpcFixture) Clock(_ context.Context, q *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.ClockReply, error) {
	f.clockCalls++
	f.nonces = append(f.nonces, bytes.Clone(q.Nonce))
	r := &appv1.ClockReply{Nonce: bytes.Clone(q.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: f.now.UnixNano(), LatestUnixNs: f.now.Add(time.Millisecond).UnixNano(), BootId: "test-boot", MonotonicNs: int64(time.Second), Source: "test-commissioned-clock"}, Tenant: f.cfg.Tenant, Site: f.cfg.Site, Device: f.cfg.Device, App: App, Service: Service, ExecutionDomain: "simulation", DeploymentGeneration: 9, ServerBootId: strings.Repeat("a", 32), SampleClockDomain: f.cfg.SampleClockDomain, SampleUnixNs: f.sample.UnixNano()}
	if f.clockMutation != nil {
		f.clockMutation(r)
	}
	return r, nil
}
func (f *rpcFixture) Publish(ctx context.Context, q *appv1.PublishRequest, _ ...grpc.CallOption) (*appv1.PublishReply, error) {
	f.publishCalls++
	f.requests = append(f.requests, proto.Clone(q).(*appv1.PublishRequest))
	r, e := appv1.DecodePublish(q)
	if e != nil {
		return nil, e
	}
	signed, e := f.local.Publish(ctx, r)
	if e != nil {
		return nil, e
	}
	if f.publishError {
		return nil, status.Error(codes.DeadlineExceeded, "test response lost after signing")
	}
	out, e := appv1.EncodePublishReply(signed)
	if f.mutation != nil {
		f.mutation(out)
	}
	return out, e
}
func (f *rpcFixture) Query(_ context.Context, q *appv1.QueryRequest, _ ...grpc.CallOption) (*appv1.QueryReply, error) {
	f.queryCalls++
	if len(q.Sources) != 1 || q.Sources[0].Device != f.cfg.PeerDevice || q.Sources[0].App != App || q.Sources[0].Service != Service || q.Sources[0].ComponentType != Component || q.Sources[0].Stream != Stream || q.After != "" {
		return nil, ErrResponse
	}
	return proto.Clone(f.query).(*appv1.QueryReply), nil
}
func command(seq uint32, action string) []byte {
	c := Command{Version: 1, Sequence: seq, Action: action}
	if action == "publish" {
		v := 7.0
		c.Value = &v
		c.TTLMS = 3000
	}
	if action == "tombstone" {
		c.TTLMS = 3000
	}
	b, _ := json.Marshal(c)
	return b
}
func remoteOriginal(t *testing.T, p *Probe, f *rpcFixture) []byte {
	t.Helper()
	o, e := p.clock.Observe(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	c, _ := DecodeCommand(command(1, "publish"))
	q, e := p.request(c, o)
	if e != nil {
		t.Fatal(e)
	}
	typed, _ := appv1.DecodePublish(q)
	s, e := f.remote.Publish(context.Background(), typed)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(s)
	return raw
}

func TestFreshClockExactSampleAndTombstone(t *testing.T) {
	p, f := fixture(t)
	for i, action := range []string{"publish", "publish", "tombstone"} {
		if i == 2 {
			f.sample = f.sample.Add(4 * time.Second)
		}
		r, e := p.Execute(context.Background(), command(uint32(i+1), action))
		if e != nil {
			t.Fatal(e)
		}
		if r.Status != "complete" || r.Clock.SampleUnixNS != f.sample.UnixNano() || len(r.Originals) != 1 || r.Originals[0].EnvelopeSHA256 != digest(r.Originals[0].Envelope) {
			t.Fatal("lost original clock or proof", r)
		}
		var signed serviceorigin.SignedRecord
		var record serviceorigin.Record
		_ = json.Unmarshal(r.Originals[0].Envelope, &signed)
		_ = json.Unmarshal(signed.Record, &record)
		if !record.Publication.Timing.SampleTime.Equal(f.sample) || record.Sequence != uint64(i+1) || record.Epoch != 41 || record.Scope.DeploymentGeneration != 9 {
			t.Fatal("rebased or incorrectly pinned receipt", record)
		}
		if action == "tombstone" && (!record.Publication.Tombstone || len(record.Publication.Payload) != 0) {
			t.Fatal("invalid tombstone")
		}
	}
	if f.clockCalls != 3 || f.publishCalls != 3 || bytes.Equal(f.nonces[0], f.nonces[1]) {
		t.Fatal("clock cached or RPC duplicated")
	}
	if _, e := p.Execute(context.Background(), command(4, "finish")); e != nil {
		t.Fatal(e)
	}
	if _, e := p.Execute(context.Background(), command(5, "publish")); !errors.Is(e, ErrStopped) {
		t.Fatal("finish resumed")
	}
}

func TestUnknownPublicationNeverRetries(t *testing.T) {
	for _, mode := range []string{"lost", "unaccepted", "wrong-request", "wrong-generation", "unknown-field"} {
		t.Run(mode, func(t *testing.T) {
			p, f := fixture(t)
			if mode == "lost" {
				f.publishError = true
			} else {
				f.mutation = func(r *appv1.PublishReply) {
					switch mode {
					case "unaccepted":
						r.Accepted = false
					case "unknown-field":
						r.ProtoReflect().SetUnknown([]byte{0x18, 1})
					default:
						var s serviceorigin.SignedRecord
						var record serviceorigin.Record
						_ = json.Unmarshal(r.SignedRecordJson, &s)
						_ = json.Unmarshal(s.Record, &record)
						if mode == "wrong-request" {
							record.Publication.TTL = time.Second
						} else {
							record.Scope.DeploymentGeneration++
						}
						s.Record, _ = json.Marshal(record)
						r.SignedRecordJson, _ = json.Marshal(s)
					}
				}
			}
			r, e := p.Execute(context.Background(), command(1, "publish"))
			if !errors.Is(e, ErrUncertain) || r.Status != "unknown_outcome" {
				t.Fatal("uncertain write accepted", e, r)
			}
			if _, e = p.Execute(context.Background(), command(1, "publish")); !errors.Is(e, ErrStopped) {
				t.Fatal("uncertain mutation retried")
			}
			if f.publishCalls != 1 || f.clockCalls != 1 {
				t.Fatal("RPC retry", f.publishCalls, f.clockCalls)
			}
		})
	}
}

func TestRemoteQueryPreservesExactBytesAndEmptySnapshot(t *testing.T) {
	p, f := fixture(t)
	original := remoteOriginal(t, p, f)
	f.query = &appv1.QueryReply{SignedRecordJson: [][]byte{original}}
	r, e := p.Execute(context.Background(), command(1, "query"))
	if e != nil || len(r.Originals) != 1 || !bytes.Equal(r.Originals[0].Envelope, original) {
		t.Fatal("remote bytes changed", e)
	}
	f.query = &appv1.QueryReply{}
	r, e = p.Execute(context.Background(), command(2, "query"))
	if e != nil || r.Status != "complete" || len(r.Originals) != 0 {
		t.Fatal("empty current snapshot failed", e)
	}
	if f.queryCalls != 2 || f.publishCalls != 0 {
		t.Fatal("unexpected publication or retry")
	}
}

func TestRemoteQueryRefusesLocalAliasAndUnboundedReply(t *testing.T) {
	for _, mode := range []string{"local", "run", "frame", "duplicate", "cursor", "unknown", "tombstone"} {
		t.Run(mode, func(t *testing.T) {
			p, f := fixture(t)
			raw := remoteOriginal(t, p, f)
			var s serviceorigin.SignedRecord
			var r serviceorigin.Record
			_ = json.Unmarshal(raw, &s)
			_ = json.Unmarshal(s.Record, &r)
			switch mode {
			case "local":
				r.Scope.Device = f.cfg.Device
			case "run":
				r.Publication.Provenance.Simulation.RunID = "different"
			case "frame":
				r.Publication.Frame.Revision = "different"
			case "tombstone":
				r.Publication.Tombstone = true
				r.Publication.Payload = nil
			}
			s.Record, _ = json.Marshal(r)
			raw, _ = json.Marshal(s)
			f.query = &appv1.QueryReply{SignedRecordJson: [][]byte{raw}}
			switch mode {
			case "duplicate":
				f.query.SignedRecordJson = append(f.query.SignedRecordJson, raw)
			case "cursor":
				f.query.Next = "unexpected"
			case "unknown":
				f.query.ProtoReflect().SetUnknown([]byte{0x18, 1})
			}
			if out, e := p.Execute(context.Background(), command(1, "query")); !errors.Is(e, ErrResponse) || out.Status != "failed" {
				t.Fatal("bad query accepted", mode, e)
			}
		})
	}
}

func TestChangedClockIdentityOrRegressionStopsBeforeMutation(t *testing.T) {
	for _, mode := range []string{"generation", "boot", "sample", "nonce"} {
		t.Run(mode, func(t *testing.T) {
			p, f := fixture(t)
			if _, e := p.Execute(context.Background(), command(1, "publish")); e != nil {
				t.Fatal(e)
			}
			f.clockMutation = func(r *appv1.ClockReply) {
				switch mode {
				case "generation":
					r.DeploymentGeneration++
				case "boot":
					r.ServerBootId = strings.Repeat("b", 32)
				case "sample":
					r.SampleUnixNs--
				case "nonce":
					r.Nonce[0] ^= 1
				}
			}
			if _, e := p.Execute(context.Background(), command(2, "publish")); e == nil {
				t.Fatal("changed clock accepted")
			}
			if f.publishCalls != 1 {
				t.Fatal("mutated after clock fault")
			}
		})
	}
}

func TestStrictCommandsAndReplayBound(t *testing.T) {
	for _, raw := range []string{`{"version":1,"sequence":1,"action":"publish","value":7,"ttl_ms":3000,"extra":true}`, `{"version":1,"sequence":1,"sequence":2,"action":"query"}`, `{"version":1,"sequence":0,"action":"query"}`, `{"version":1,"sequence":1,"action":"publish","value":null,"ttl_ms":3000}`, `{"version":1,"sequence":1,"action":"tombstone","value":2,"ttl_ms":3000}`, `{"version":1,"sequence":1,"action":"query","ttl_ms":1}`} {
		if _, e := DecodeCommand([]byte(raw)); e == nil {
			t.Fatal("invalid command accepted", raw)
		}
	}
	p, f := fixture(t)
	if _, e := p.Execute(context.Background(), command(1, "query")); e != nil {
		t.Fatal(e)
	}
	if _, e := p.Execute(context.Background(), command(1, "publish")); !errors.Is(e, ErrCommand) {
		t.Fatal("repeated sequence accepted")
	}
	if f.publishCalls != 0 || f.queryCalls != 1 {
		t.Fatal("replay touched API")
	}
}
