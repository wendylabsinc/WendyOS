//go:build !windows

package sessionbroker

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// Return a formerly Ready transport with its departure observed BEFORE the
// watcher/serve is invoked. No scheduler sleeps or RPC traffic trigger eviction.
func departedProbedTransport(t *testing.T) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(server, testAgentServer{})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	upstream, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithIdleTimeout(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := agentpb.NewWendyAgentServiceClient(upstream).GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatal(err)
	}
	if upstream.GetState() != connectivity.Ready {
		t.Fatal("probe did not establish Ready")
	}
	server.Stop()
	if !upstream.WaitForStateChange(ctx, connectivity.Ready) {
		t.Fatal("transport did not leave Ready")
	}
	if upstream.GetState() == connectivity.Ready {
		t.Fatal("fixture reconnected after departure")
	}
	return upstream
}

func TestWatchRejectsProbedTransportDepartedBeforeStartup(t *testing.T) {
	upstream := departedProbedTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a := newActivity()
	watchUpstreamState(ctx, upstream, a, true)
	select {
	case <-a.upstreamBad:
	default:
		t.Fatal("lost verified transport was treated as unprobed startup")
	}
}

func TestWatchAllowsUnprobedInitialIdle(t *testing.T) {
	upstream, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	if upstream.GetState() != connectivity.Idle {
		t.Fatal("fixture must start unprobed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := newActivity()
	watchUpstreamState(ctx, upstream, a, false)
	select {
	case <-a.upstreamBad:
		t.Fatal("unprobed startup was incorrectly evicted")
	default:
	}
}

func TestServeRetiresProbedTransportLostBeforeEntry(t *testing.T) {
	upstream := departedProbedTransport(t)
	dir := shortTempDir(t)
	spec := Spec{Key: "lost-before-entry.local", CertFingerprint: "verified-cert", Expected: certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "48"}}
	socketPath, statePath := paths(dir, spec.Key, spec.Expected)
	lock, acquired, err := acquireIdentityLock(socketPath)
	if err != nil || !acquired {
		t.Fatalf("acquire initial identity lock: %v, %v", acquired, err)
	}
	// Mirror Run's lifetime: the identity flock spans serve and releases on
	// its return, even when the parent remains alive and idle expiry is distant.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	func() {
		defer lock.Close()
		defer releaseLock(lock)
		if err := serve(ctx, dir, spec, upstream, time.Hour, func() bool { return true }, true); err != nil {
			t.Fatal(err)
		}
	}()
	if ctx.Err() != nil {
		t.Fatal("lost verified transport waited for cancellation instead of retiring")
	}
	for _, p := range []string{socketPath, statePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("lost transport was published at %s: %v", p, err)
		}
	}
	replacement, acquired, err := acquireIdentityLock(socketPath)
	if err != nil || !acquired {
		t.Fatalf("lost broker retained identity lock: %v, %v", acquired, err)
	}
	defer replacement.Close()
	defer releaseLock(replacement)
}
