package cloudsession

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

func TestLoadAccountNeverFallsBackToAnotherSession(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	const org = "11111111-1111-4111-8111-111111111111"
	cfg := &config.Config{Auth: []config.AuthConfig{{Name: "chosen", CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + org + "/operator/one"}}}}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	auth, err := LoadAccount("chosen", org)
	if err != nil || auth.Name != "chosen" {
		t.Fatalf("auth=%v err=%v", auth, err)
	}
	for _, tc := range []struct{ account, tenant string }{{"missing", ""}, {"chosen", "other"}} {
		if _, err := LoadAccount(tc.account, tc.tenant); err == nil {
			t.Fatalf("accepted wrong session: %+v", tc)
		}
	}
}

func TestWithAuthenticatesAndClosesConnection(t *testing.T) {
	t.Setenv(clouddefaults.CloudInsecureEnv, "true")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer test-token" {
			t.Errorf("missing shared auth: %v", got)
		}
		return handler(ctx, req)
	}))
	healthpb.RegisterHealthServer(server, health.NewServer())
	go server.Serve(lis)
	t.Cleanup(server.Stop)
	auth := &config.AuthConfig{CloudGRPC: lis.Addr().String(), APIKey: "test-token"}
	var conn *grpc.ClientConn
	err = With(context.Background(), auth, func(context.Context, *config.AuthConfig) error { t.Fatal("refreshed legacy token"); return nil }, func(ctx context.Context, c grpc.ClientConnInterface) error {
		conn = c.(*grpc.ClientConn)
		_, err := healthpb.NewHealthClient(c).Check(ctx, &healthpb.HealthCheckRequest{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn.GetState() != connectivity.Shutdown {
		t.Fatal("connection leaked")
	}
}

func TestWithRefreshFailureDoesNotInvokeOperation(t *testing.T) {
	t.Setenv(clouddefaults.CloudInsecureEnv, "true")
	want := errors.New("refresh refused")
	auth := &config.AuthConfig{CloudGRPC: "localhost:12345", OAuthIssuer: "https://issuer.test", APIKey: "old"}
	err := With(context.Background(), auth, func(_ context.Context, local *config.AuthConfig) error { local.APIKey = "changed"; return want }, func(context.Context, grpc.ClientConnInterface) error {
		t.Fatal("operation after failed auth")
		return nil
	})
	if !errors.Is(err, want) || auth.APIKey != "old" {
		t.Fatalf("refresh isolation: %v %v", auth.APIKey, err)
	}
}
