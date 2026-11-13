package clouddefaults

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestTunnelDialerAuthorizationFailure(t *testing.T) {
	for _, code := range []codes.Code{codes.FailedPrecondition, codes.InvalidArgument, codes.PermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			refusal := status.Error(code, "pki-core refused the tunnel principal attestation request")
			conn, err := grpc.NewClient("passthrough:///cloud-tunnel",
				TunnelDialer(func(context.Context) (net.Conn, error) {
					return nil, fmt.Errorf("authorizing Cloud tunnel: %w", refusal)
				}),
				grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("failed tunnel dial returned %v; want Unavailable", err)
			}
			message := status.Convert(err).Message()
			if !strings.Contains(message, refusal.Error()) || strings.Contains(message, "illegal status") {
				t.Fatalf("tunnel refusal was obscured: %v", err)
			}
		})
	}
}
