package appservice

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
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

func brokerClockRequest() *appv1.ClockRequest {
	return &appv1.ClockRequest{Nonce: bytes.Repeat([]byte{23}, appv1.ClockNonceSize)}
}

func brokerClockReply(scope serviceorigin.Scope, nonce []byte) *appv1.ClockReply {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC).UnixNano()
	return &appv1.ClockReply{Nonce: bytes.Clone(nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: now, LatestUnixNs: now + int64(time.Millisecond), BootId: "test-boot", MonotonicNs: int64(time.Second), Source: "test-authority"}, SampleClockDomain: "simulation:run", SampleUnixNs: now - int64(time.Hour), Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: scope.DeploymentGeneration, ServerBootId: strings.Repeat("a", 32)}
}

func TestClockRequiresFencedReadOnlyScopeAndPreservesOriginalSample(t *testing.T) {
	var fenced atomic.Bool
	f := setup(t, func(cfg *Config) {
		cfg.Clock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *Fence) (*appv1.ClockReply, error) {
			if len(scope.Publish) != 0 {
				return nil, ErrAdmission
			}
			reply := brokerClockReply(scope, nonce)
			if !fenced.Load() {
				return reply, nil
			}
			return reply, fence.Commit(ctx, func(context.Context) error { return nil })
		}
	})
	scope := f.scope
	scope.Publish = nil
	a, _ := dial(t, f.mount(t, scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := a.Clock(ctx, brokerClockRequest()); status.Code(err) != codes.Unavailable || out != nil {
		t.Fatal("unfenced clock became time authority", err)
	}
	fenced.Store(true)
	first, err := a.Clock(ctx, brokerClockRequest())
	if err != nil || !proto.Equal(first, brokerClockReply(scope, brokerClockRequest().Nonce)) {
		t.Fatal("fenced clock changed original scope, interval, nonce or simulation time", err)
	}
	bScope := scope
	bScope.Service = "detector-b"
	b, _ := dial(t, f.mount(t, bScope).SocketPath)
	second, err := b.Clock(ctx, brokerClockRequest())
	if err != nil || !proto.Equal(second, brokerClockReply(bScope, brokerClockRequest().Nonce)) || first.Service == second.Service || f.count() != 0 {
		t.Fatal("same-app services shared clock identity or signed a publication", err)
	}
}

func TestClockRejectsMalformedRequestsBeforeCallback(t *testing.T) {
	var calls atomic.Int32
	f := setup(t, func(cfg *Config) {
		cfg.Clock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *Fence) (*appv1.ClockReply, error) {
			calls.Add(1)
			return brokerClockReply(scope, nonce), fence.Commit(ctx, func(context.Context) error { return nil })
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	unknown := brokerClockRequest()
	unknown.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 99, protowire.BytesType), "foreign-service"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, request := range []*appv1.ClockRequest{{}, {Nonce: make([]byte, 31)}, {Nonce: make([]byte, 33)}, unknown} {
		if out, err := client.Clock(ctx, request); status.Code(err) != codes.InvalidArgument || out != nil {
			t.Fatal("malformed clock request accepted", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("malformed nonce or client-supplied authority reached host callback")
	}
}

func TestClockRefusesHostReplyWithWrongNonceScopeOrInterval(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	for name, mutate := range map[string]func(*appv1.ClockReply){
		"nonce":          func(p *appv1.ClockReply) { p.Nonce[0]++ },
		"service":        func(p *appv1.ClockReply) { p.Service = "detector-b" },
		"generation":     func(p *appv1.ClockReply) { p.DeploymentGeneration++ },
		"site":           func(p *appv1.ClockReply) { p.Site = "another-site" },
		"bounds":         func(p *appv1.ClockReply) { p.Physical.LatestUnixNs = p.Physical.EarliestUnixNs - 1 },
		"unknown":        func(p *appv1.ClockReply) { p.ProtoReflect().SetUnknown(unknown) },
		"nested-unknown": func(p *appv1.ClockReply) { p.Physical.ProtoReflect().SetUnknown(unknown) },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, func(cfg *Config) {
				cfg.Clock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *Fence) (*appv1.ClockReply, error) {
					reply := brokerClockReply(scope, nonce)
					mutate(reply)
					return reply, fence.Commit(ctx, func(context.Context) error { return nil })
				}
			})
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if out, err := client.Clock(ctx, brokerClockRequest()); status.Code(err) != codes.Unavailable || out != nil {
				t.Fatal("invalid host reply escaped clock authority checks", err)
			}
		})
	}
}

func TestClockRechecksOriginalPeerAfterFinalizer(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	f := setup(t, func(cfg *Config) {
		cfg.Clock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *Fence) (*appv1.ClockReply, error) {
			return brokerClockReply(scope, nonce), fence.Commit(ctx, func(context.Context) error { return nil })
		}
		cfg.FinalizeResponse = func(ctx context.Context, _ serviceorigin.Scope, reply proto.Message) (proto.Message, error) {
			close(entered)
			select {
			case <-resume:
				return reply, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	// This seam isolates death after the last blocking peer check. The Linux
	// child-process test below separately exercises kernel peer credentials.
	pin := &finalResponsePin{}
	f.manager.peerPin = func(*net.UnixConn) (peerPin, error) { return pin, nil }
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	type result struct {
		out *appv1.ClockReply
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := client.Clock(ctx, brokerClockRequest()); done <- result{out, err} }()
	finalResponseAwait(t, entered)
	pin.dead.Store(true)
	unblock()
	select {
	case got := <-done:
		if got.err == nil || got.out != nil {
			t.Fatal("dead original peer received a clock sample")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("clock did not finish after finalizer release")
	}
}

func TestClockLinuxExactServiceAndGenerationPeer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("real Linux peer credentials and pidfd required")
	}
	type binding struct {
		service    string
		generation uint64
	}
	var mu sync.Mutex
	peers := map[int32]binding{}
	var accepted atomic.Int32
	f := setup(t, func(cfg *Config) {
		cfg.PeerCheck = func(_ context.Context, peer Peer, scope serviceorigin.Scope) error {
			mu.Lock()
			defer mu.Unlock()
			if b, ok := peers[peer.PID]; !ok || b.service != scope.Service || b.generation != scope.DeploymentGeneration {
				return ErrPeer
			}
			return nil
		}
		cfg.Clock = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *Fence) (*appv1.ClockReply, error) {
			return brokerClockReply(scope, nonce), fence.Commit(ctx, func(context.Context) error { accepted.Add(1); return nil })
		}
	})
	mount := f.mount(t, f.scope)
	run := func(service string, generation uint64) string {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestClockPeerProcess$")
		cmd.Env = append(os.Environ(), "WENDY_WORLD_CLOCK_PEER="+mount.SocketPath)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		peers[int32(cmd.Process.Pid)] = binding{service, generation}
		mu.Unlock()
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("clock child failed: %v: %s", err, out.String())
		}
		mu.Lock()
		delete(peers, int32(cmd.Process.Pid))
		mu.Unlock()
		return strings.TrimSpace(out.String())
	}
	if got := run(f.scope.Service, f.scope.DeploymentGeneration); got != "true" {
		t.Fatal("commissioned clock peer denied", got)
	}
	if got := run("detector-b", f.scope.DeploymentGeneration); got != "false" {
		t.Fatal("foreign service obtained clock", got)
	}
	if got := run(f.scope.Service, f.scope.DeploymentGeneration+1); got != "false" {
		t.Fatal("foreign generation obtained clock", got)
	}
	if accepted.Load() != 1 {
		t.Fatal("denied clock peer reached callback", accepted.Load())
	}
}

func TestClockPeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_WORLD_CLOCK_PEER")
	if path == "" {
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	client, _ := dial(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := client.Clock(ctx, brokerClockRequest())
	if err == nil && out != nil && bytes.Equal(out.Nonce, brokerClockRequest().Nonce) {
		_, _ = os.Stdout.WriteString("true\n")
	} else {
		_, _ = os.Stdout.WriteString("false\n")
	}
	os.Exit(0)
}
