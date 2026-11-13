package commands

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// TestDialCloudGRPCDefaultsToTLS guards WDY-3756 at a real dial site: TestMain
// opts the package in to plaintext for its fake clouds, so this clears the
// opt-in and proves a non-:443 cloud address is dialed over TLS (the
// handshake against a plaintext server fails instead of sending in clear).
func TestDialCloudGRPCDefaultsToTLS(t *testing.T) {
	t.Setenv(clouddefaults.CloudInsecureEnv, "false")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := dialCloudGRPC(&config.AuthConfig{CloudGRPC: lis.Addr().String()})
	if err != nil {
		t.Fatalf("dialCloudGRPC() error = %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/wdy3756.Probe/Call", &emptypb.Empty{}, &emptypb.Empty{})
	if err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("Invoke() error = %v, want a TLS handshake failure against a plaintext server", err)
	}
}
