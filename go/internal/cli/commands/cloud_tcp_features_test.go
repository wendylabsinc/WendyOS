package commands

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type tcpFeatureVersionAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	calls     atomic.Int32
	halfClose atomic.Bool
}

func (a *tcpFeatureVersionAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	a.calls.Add(1)
	version := &agentpb.GetAgentVersionResponse{}
	if a.halfClose.Load() {
		version.Featureset = []string{cloudrelay.TCPHalfCloseFeature}
	}
	return version, nil
}

func TestCloudTCPFeatureProbeWarmsSlowManagementTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	srv := grpc.NewServer()
	device := new(tcpFeatureVersionAgent)
	device.halfClose.Store(true)
	agentpb.RegisterWendyAgentServiceServer(srv, device)
	listener := bufconn.Listen(64 << 10)
	defer listener.Close()
	go srv.Serve(listener)
	defer srv.Stop()

	features := new(cloudTCPFeatures)
	started := make(chan struct{})
	ready := make(chan struct{})
	var dials atomic.Int32
	cc, err := grpc.NewClient("passthrough:///device", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(features.observeVersion),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			if dials.Add(1) == 1 {
				close(started)
			}
			select {
			case <-ready:
				return listener.DialContext(ctx)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	agent := agentpb.NewWendyAgentServiceClient(cc)

	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	first := make(chan bool, 1)
	go func() { first <- features.flow(firstCtx, agent).halfClose }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("capability check did not start a management transport")
	}
	cancelFirst()
	if <-first {
		t.Fatal("abandoned socket exposed half-close capability")
	}
	if features.flow(ctx, agent).halfClose {
		t.Fatal("management probe with no response exposed half-close capability")
	}
	if last := features.last.Load(); last == nil || last.halfClose {
		t.Fatal("timeout did not record a conservative result")
	}

	close(ready)
	for state := cc.GetState(); state != connectivity.Ready; state = cc.GetState() {
		if !cc.WaitForStateChange(ctx, state) {
			t.Fatal("management transport stopped warming when the probe timed out")
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dialed management %d times, want 1", got)
	}
	// A normal VPN health check can populate support sooner than the fallback
	// cache expires. Future forward dials reuse that verified version.
	if _, err := agent.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if !features.flow(ctx, agent).halfClose {
			t.Fatal("cached capability did not enable half-close")
		}
	}
	if got := device.calls.Load(); got != 1 {
		t.Fatalf("requested version %d times, want 1", got)
	}
	// A fresh management observation after rollback invalidates older support.
	device.halfClose.Store(false)
	if _, err := agent.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatal(err)
	}
	if features.flow(ctx, agent).halfClose {
		t.Fatal("an agent that omitted the feature still enabled half-close")
	}
}

func TestForcedCloudTCPFeatureCacheIsScopedToDeviceAndSession(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	base := context.WithValue(t.Context(), cloudTunnelOnlyKey{}, true)
	device := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: "first"}, key: "first"}
	other := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: "second"}, key: "second"}
	first := cloudTCPFeaturesFor(session.Context(base), auth, device)
	if slot := cloudTCPFeaturesFor(session.Context(base), auth, device); slot != first {
		t.Fatal("VPN management and flow connections did not share the feature cache")
	}
	if slot := cloudTCPFeaturesFor(session.Context(base), auth, other); slot == first {
		t.Fatal("feature cache was shared across Cloud devices")
	}
	session.SetActive(false)
	session.SetActive(true)
	if slot := cloudTCPFeaturesFor(session.Context(base), auth, device); slot == first {
		t.Fatal("feature cache survived the old account generation")
	}
}

func TestVPNFlowsRefreshFeaturesOnOneManagementTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	device := new(tcpFeatureVersionAgent)
	device.halfClose.Store(true)
	server := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(server, device)
	listener := bufconn.Listen(64 << 10)
	defer listener.Close()
	go server.Serve(listener)
	defer server.Stop()

	var dials atomic.Int32
	dialManagement := func(features *cloudTCPFeatures) agentpb.WendyAgentServiceClient {
		t.Helper()
		cc, err := grpc.NewClient("passthrough:///device", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(features.observeVersion),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				dials.Add(1)
				return listener.DialContext(ctx)
			}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cc.Close() })
		return agentpb.NewWendyAgentServiceClient(cc)
	}
	features := new(cloudTCPFeatures)
	management := dialManagement(features)
	flowCtx := WithCloudTCPFeatureProbe(ctx, func(ctx context.Context) error {
		_, err := management.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		return err
	})
	openFlow := func(scope *cloudTCPFeatures, want bool) {
		t.Helper()
		disposable := dialManagement(scope)
		if got := scope.flow(flowCtx, disposable).halfClose; got != want {
			t.Fatalf("half-close support = %t, want %t", got, want)
		}
		// Keep the flow and its disposable channel alive beyond the next
		// observation expiry, as a VPN forwarding connection would.
		local, peer := net.Pipe()
		t.Cleanup(func() { local.Close(); peer.Close() })
	}
	openFlow(features, true)
	features.last.Store(&cloudTCPFeatureObservation{halfClose: true, expires: time.Now().Add(-time.Second)})
	openFlow(features, true)
	if got := device.calls.Load(); got != 2 {
		t.Fatalf("management RPCs after expiry = %d, want 2", got)
	}
	// A connection selected under a different credential or asset binding
	// cannot use this management channel's version as its own proof.
	differentScope := new(cloudTCPFeatures)
	openFlow(differentScope, false)
	openFlow(differentScope, false)
	if got := device.calls.Load(); got != 3 {
		t.Fatalf("management RPCs after repeated unmatched probes = %d, want 3", got)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dialed %d management transports while flows were open, want 1", got)
	}
}
