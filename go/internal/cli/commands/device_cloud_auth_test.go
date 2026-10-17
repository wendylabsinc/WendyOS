package commands

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/metadata"
)

func TestDialCloudReturnsAuthenticatedSession(t *testing.T) {
	const selectedEndpoint = "127.0.0.1:50051"
	const otherEndpoint = "127.0.0.1:50052"
	for _, tc := range []struct {
		name, target, deviceCloud string
	}{
		{"explicit endpoint", selectedEndpoint, otherEndpoint},
		{"device cloud fallback", "", selectedEndpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTempConfig(t, &config.Config{
				CurrentContext: "other",
				Auth: []config.AuthConfig{
					{Name: "other", CloudGRPC: otherEndpoint, Certificates: []config.CertificateInfo{
						{OrganizationID: 55, UserID: "other-session-user"},
					}},
					{Name: "selected", CloudGRPC: selectedEndpoint, Certificates: []config.CertificateInfo{
						{OrganizationID: 55, UserID: "selected-user"},
						{OrganizationID: 99, UserID: "other-certificate-user"},
					}},
				},
			})
			original := ensureFreshCertificateFn
			ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
			t.Cleanup(func() { ensureFreshCertificateFn = original })

			conn, ctx, auth, err := dialCloud(context.Background(), tc.target, tc.deviceCloud)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			if conn.Target() != selectedEndpoint || auth.CloudGRPC != selectedEndpoint || auth.Name != "selected" {
				t.Fatalf("connection and returned auth must use selected session: target=%q auth=%q endpoint=%q", conn.Target(), auth.Name, auth.CloudGRPC)
			}
			if len(auth.Certificates) == 0 || auth.Certificates[0].UserID != "selected-user" {
				t.Fatal("returned session must retain the first certificate's user ID")
			}
			headers, _ := metadata.FromOutgoingContext(ctx)
			for _, name := range []string{"x-wendy-client-cert", "x-forwarded-client-cert"} {
				values := headers.Get(name)
				if len(values) != 1 || values[0] != "URI=urn:wendy:org:55:user:selected-user" {
					t.Fatalf("%s must identify the returned session's first certificate, got %v", name, values)
				}
			}
		})
	}
}
