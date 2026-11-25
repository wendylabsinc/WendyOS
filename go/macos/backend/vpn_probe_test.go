package backend

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type vpnProbeClient struct {
	agentpb.WendyAgentServiceClient
	call func(context.Context) error
}

func (c vpnProbeClient) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{}, c.call(ctx)
}

type vpnProbeCloser func() error

func (f vpnProbeCloser) Close() error { return f() }

func probeTestConnection(call func(context.Context) error, close func() error) *grpcclient.AgentConnection {
	return &grpcclient.AgentConnection{AgentService: vpnProbeClient{call: call}, ExtraClosers: []io.Closer{vpnProbeCloser(close)}}
}

func TestVPNProbesReuseConnectionButRequireFreshResponses(t *testing.T) {
	var dials, calls, closes atomic.Int32
	var transport context.Context
	failure := errors.New("device offline")
	p := newVPNProbes([]vpn.Route{{Selector: "mesh:one"}}, func(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
		if selector != "mesh:one" {
			t.Errorf("wrong selector: %s", selector)
		}
		dials.Add(1)
		transport = ctx
		return probeTestConnection(func(context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if calls.Add(1) == 3 {
				return failure
			}
			return nil
		}, func() error { closes.Add(1); return nil }), nil
	})
	defer p.Close()
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := p.Probe(ctx, "mesh:one")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if transport.Err() != nil {
			t.Fatal("echo cancellation closed the shared transport")
		}
	}
	if dials.Load() != 1 || calls.Load() != 2 || closes.Load() != 0 {
		t.Fatalf("dials=%d calls=%d closes=%d", dials.Load(), calls.Load(), closes.Load())
	}
	if err := p.Probe(context.Background(), "mesh:one"); !errors.Is(err, failure) {
		t.Fatalf("failed RPC must not produce a cached success: %v", err)
	}
	if closes.Load() != 1 || transport.Err() == nil {
		t.Fatal("failed connection was not discarded")
	}
	if err := p.Probe(context.Background(), "mesh:one"); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 || calls.Load() != 4 {
		t.Fatal("next echo did not reconnect and issue a fresh RPC")
	}
	p.Close()
	p.Close()
	if closes.Load() != 2 || transport.Err() == nil {
		t.Fatal("shutdown did not close each transport exactly once")
	}
	if err := p.Probe(context.Background(), "mesh:one"); !errors.Is(err, context.Canceled) {
		t.Fatalf("probe after shutdown: %v", err)
	}
}

func TestVPNProbesConcurrentDevices(t *testing.T) {
	var dials, calls atomic.Int32
	started, release := make(chan struct{}, 2), make(chan struct{})
	p := newVPNProbes([]vpn.Route{{Selector: "mesh:one"}, {Selector: "mesh:two"}}, func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
		dials.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return probeTestConnection(func(context.Context) error { calls.Add(1); return nil }, func() error { return nil }), nil
	})
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, selector := range []string{"mesh:one", "mesh:two"} {
		for range 8 {
			wg.Go(func() {
				if err := p.Probe(ctx, selector); err != nil {
					t.Error(err)
				}
			})
		}
	}
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("one device's setup blocked the other")
		}
	}
	close(release)
	wg.Wait()
	if dials.Load() != 2 || calls.Load() != 16 {
		t.Fatalf("dials=%d calls=%d", dials.Load(), calls.Load())
	}
	if err := p.Probe(ctx, "mesh:unselected"); err == nil || dials.Load() != 2 {
		t.Fatal("unselected device reached the connector")
	}
}

func TestVPNProbesCancellation(t *testing.T) {
	for _, phase := range []string{"connect", "rpc"} {
		for _, shutdown := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "/request", true: "/shutdown"}[shutdown], func(t *testing.T) {
				started := make(chan struct{})
				var life context.Context
				var closes atomic.Int32
				p := newVPNProbes([]vpn.Route{{Selector: "mesh:one"}}, func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
					life = ctx
					if phase == "connect" {
						close(started)
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return probeTestConnection(func(ctx context.Context) error {
						close(started)
						<-ctx.Done()
						return ctx.Err()
					}, func() error { closes.Add(1); return nil }), nil
				})
				defer p.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- p.Probe(ctx, "mesh:one") }()
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("probe did not start")
				}
				// A queued request must honor its own cancellation too.
				queued, cancelQueued := context.WithCancel(context.Background())
				cancelQueued()
				if err := p.Probe(queued, "mesh:one"); !errors.Is(err, context.Canceled) {
					t.Fatalf("queued probe: %v", err)
				}
				if shutdown {
					go p.Close()
				} else {
					cancel()
				}
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled probe: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not stop the probe")
				}
				if life.Err() == nil || phase == "rpc" && closes.Load() != 1 {
					t.Fatal("cancellation leaked the transport")
				}
			})
		}
	}
}

func TestVPNBackendClosesProbeSessions(t *testing.T) {
	for _, action := range []string{"stop", "replace", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			b := New()
			defer b.Shutdown()
			routes := []vpn.Route{{Selector: "mesh:one"}}
			cfg, err := b.StartVPN(routes)
			if err != nil {
				t.Fatal(err)
			}
			var closes atomic.Int32
			b.vpnProbes.connect = func(context.Context, string) (*grpcclient.AgentConnection, error) {
				return probeTestConnection(func(context.Context) error { return nil }, func() error { closes.Add(1); return nil }), nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := vpn.ProbeRelay(ctx, cfg, cfg.Routes[0].Address); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "stop":
				err = b.StopVPN()
			case "replace":
				_, err = b.StartVPN(routes)
			case "shutdown":
				err = b.Shutdown()
			}
			if err != nil || closes.Load() != 1 {
				t.Fatalf("%s: err=%v closes=%d", action, err, closes.Load())
			}
			if vpn.Healthy(ctx, cfg) {
				t.Fatal("old relay still accepts requests")
			}
		})
	}
}
