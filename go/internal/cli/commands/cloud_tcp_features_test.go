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
	go func() { first <- features.halfClose(firstCtx, agent) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("capability check did not start a management transport")
	}
	cancelFirst()
	if <-first {
		t.Fatal("abandoned socket exposed half-close capability")
	}
	if features.halfClose(ctx, agent) {
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
		if !features.halfClose(ctx, agent) {
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
	if features.halfClose(ctx, agent) {
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
