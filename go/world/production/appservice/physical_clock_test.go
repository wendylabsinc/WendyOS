package appservice

import (
	"bytes"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func brokerPhysicalReply(scope serviceorigin.Scope, nonce []byte) *appv1.PhysicalClockReply {
	c := brokerClockReply(scope, nonce)
	return &appv1.PhysicalClockReply{Nonce: c.Nonce, Physical: c.Physical, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration, ServerBootId: c.ServerBootId}
}

func TestPhysicalClockRequiresFenceAndIndependentCallback(t *testing.T) {
	var fenced atomic.Bool
	f := setup(t, func(cfg *Config) {
		cfg.Clock = func(context.Context, serviceorigin.Scope, []byte, *Fence) (*appv1.ClockReply, error) {
			t.Error("physical clock fell back to source clock")
			return nil, ErrAdmission
		}
		cfg.PhysicalClock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, f *Fence) (*appv1.PhysicalClockReply, error) {
			if len(scope.Publish) != 0 {
				return nil, ErrAdmission
			}
			reply := brokerPhysicalReply(scope, nonce)
			if !fenced.Load() {
				return reply, nil
			}
			return reply, f.Commit(ctx, func(context.Context) error { return nil })
		}
	})
	scope := f.scope
	scope.Publish = nil
	client, _ := dial(t, f.mount(t, scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := client.PhysicalClock(ctx, brokerClockRequest()); status.Code(err) != codes.Unavailable || out != nil {
		t.Fatal("unfenced physical time accepted", err)
	}
	fenced.Store(true)
	got, err := client.PhysicalClock(ctx, brokerClockRequest())
	if err != nil || !proto.Equal(got, brokerPhysicalReply(scope, brokerClockRequest().Nonce)) || f.count() != 0 {
		t.Fatal("read-only physical sample changed scope/bytes or published", err)
	}
	other := scope
	other.Service = "detector-b"
	second, _ := dial(t, f.mount(t, other).SocketPath)
	if out, err := second.PhysicalClock(ctx, brokerClockRequest()); err != nil || out.Service != other.Service {
		t.Fatal("services shared physical clock identity", err)
	}
}

func TestPhysicalClockRefusesMissingCallbackMalformedRequestsAndReplies(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	for name, mutate := range map[string]func(*appv1.PhysicalClockReply){
		"nonce":      func(p *appv1.PhysicalClockReply) { p.Nonce[0]++ },
		"service":    func(p *appv1.PhysicalClockReply) { p.Service = "other" },
		"generation": func(p *appv1.PhysicalClockReply) { p.DeploymentGeneration++ },
		"site":       func(p *appv1.PhysicalClockReply) { p.Site = "other" },
		"bounds":     func(p *appv1.PhysicalClockReply) { p.Physical.LatestUnixNs = 0 },
		"unknown":    func(p *appv1.PhysicalClockReply) { p.ProtoReflect().SetUnknown(unknown) },
		"nested":     func(p *appv1.PhysicalClockReply) { p.Physical.ProtoReflect().SetUnknown(unknown) },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, func(cfg *Config) {
				cfg.PhysicalClock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *Fence) (*appv1.PhysicalClockReply, error) {
					r := brokerPhysicalReply(scope, nonce)
					mutate(r)
					return r, fence.Commit(ctx, func(context.Context) error { return nil })
				}
			})
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if out, err := client.PhysicalClock(ctx, brokerClockRequest()); status.Code(err) != codes.Unavailable || out != nil {
				t.Fatal("invalid physical reply accepted", err)
			}
		})
	}
	f := setup(t, nil)
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if out, err := client.PhysicalClock(ctx, brokerClockRequest()); status.Code(err) != codes.Unimplemented || out != nil {
		t.Fatal("missing independent callback did not fail closed", err)
	}
	bad := brokerClockRequest()
	bad.ProtoReflect().SetUnknown(unknown)
	for _, req := range []*appv1.ClockRequest{{}, {Nonce: bytes.Repeat([]byte{1}, 33)}, bad} {
		if out, err := client.PhysicalClock(ctx, req); status.Code(err) != codes.InvalidArgument || out != nil {
			t.Fatal("malformed request accepted", err)
		}
	}
}

func TestPhysicalClockRefusesFinalizerOrOriginalPeerFailure(t *testing.T) {
	for _, mode := range []string{"finalizer", "peer", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			defer unblock()
			f := setup(t, func(cfg *Config) {
				cfg.PhysicalClock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *Fence) (*appv1.PhysicalClockReply, error) {
					return brokerPhysicalReply(scope, nonce), fence.Commit(ctx, func(context.Context) error { return nil })
				}
				cfg.FinalizeResponse = func(ctx context.Context, _ serviceorigin.Scope, reply proto.Message) (proto.Message, error) {
					close(entered)
					select {
					case <-resume:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					if mode == "finalizer" {
						return nil, ErrAdmission
					}
					return reply, nil
				}
			})
			pin := &finalResponsePin{}
			f.manager.peerPin = func(*net.UnixConn) (peerPin, error) { return pin, nil }
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			type result struct {
				out *appv1.PhysicalClockReply
				err error
			}
			done := make(chan result, 1)
			go func() { out, err := client.PhysicalClock(ctx, brokerClockRequest()); done <- result{out, err} }()
			finalResponseAwait(t, entered)
			if mode == "peer" {
				pin.dead.Store(true)
			}
			if mode == "cancel" {
				cancel()
			}
			unblock()
			select {
			case got := <-done:
				if got.err == nil || got.out != nil {
					t.Fatal("failed final authority returned physical time")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("physical clock did not terminate")
			}
		})
	}
}
