package commands

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestDesktopLocalResetRequiresVerifiedIdentityAndMatchingAccount(t *testing.T) {
	for _, variant := range []string{"valid", "proxy", "plaintext", "old agent", "wrong device", "wrong organization", "missing account", "changed operator", "changed cloud"} {
		t.Run(variant, func(t *testing.T) {
			server := &localUnenrollTestServer{ack: true}
			conn, prov, remote := localUnenrollFixture(t, server)
			account, organization, device := "selected", unenrollTenant, unenrollDevice
			auth := config.AuthConfig{Name: account, CloudGRPC: prov.GetCloudHost(), Certificates: []config.CertificateInfo{*conn.CertInfo}}
			switch variant {
			case "proxy":
				conn.IsSessionProxy = true
			case "plaintext":
				conn.IsMTLS = false
			case "old agent":
				prov.LocalResetSupported = false
			case "wrong device":
				device = "other"
			case "wrong organization":
				organization = "other"
			case "missing account":
				account = "missing"
			case "changed operator":
				auth.Certificates = []config.CertificateInfo{*localUnenrollOperatorCert(t, "spiffe://wendy.sh/tenant/"+unenrollTenant+"/operator/other")}
			case "changed cloud":
				auth.CloudGRPC = "other.test:443"
			}
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{auth}})
			plan, err := desktopLocalResetPlan(conn, prov, remote, account, organization, device)
			if variant == "valid" {
				if err != nil || plan.Principal != prov.PrincipalUri || plan.Fingerprint == "" {
					t.Fatalf("valid plan: %+v %v", plan, err)
				}
			} else if err == nil {
				t.Fatal("unsafe preflight accepted")
			}
			if server.calls.Load() != 0 {
				t.Fatal("preflight sent reset")
			}
		})
	}
}
func TestDesktopLocalResetDoesNotRetryOrInferCompletionFromDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name string
		ack  bool
		err  error
	}{
		{"confirmed", true, nil}, {"no acknowledgement", false, nil}, {"lost reply", false, status.Error(codes.Unavailable, "disconnected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &localUnenrollTestServer{ack: tc.ack, err: tc.err}
			conn, prov, remote := localUnenrollFixture(t, server)
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Name: "selected", CloudGRPC: prov.CloudHost, Certificates: []config.CertificateInfo{*conn.CertInfo}}}})
			plan, err := desktopLocalResetPlan(conn, prov, remote, "selected", unenrollTenant, unenrollDevice)
			if err != nil {
				t.Fatal(err)
			}
			completed, err := desktopApplyLocalReset(context.Background(), conn, plan)
			if completed != tc.ack || (err == nil) != tc.ack {
				t.Fatalf("completed=%v err=%v", completed, err)
			}
			if server.calls.Load() != 1 {
				t.Fatal("reset must be sent exactly once")
			}
			req := <-server.requests
			if !req.LocalOnly || req.ExpectedPrincipalUri != plan.Principal || req.ExpectedCertificateSha256 != plan.Fingerprint || len(req.CloudDeletion) != 0 || len(req.RevocationProof) != 0 {
				t.Fatalf("incorrect request: %+v", req)
			}
		})
	}
}
