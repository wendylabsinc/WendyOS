package commands

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	legacy "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestDesktopEnrollmentStatus(t *testing.T) {
	for _, tc := range []struct {
		name                string
		response            *pb.IsProvisionedResponse
		err                 error
		enrolled, wantError bool
		org, device         string
	}{
		{name: "unenrolled", response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_NotProvisioned{NotProvisioned: &pb.NotProvisionedResponse{}}}},
		{name: "enrolled", enrolled: true, org: "42", response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{OrganizationId: 42}}}},
		{name: "device principal supplies transport-independent identity", enrolled: true, org: testOperatorTenant, device: "fad6089a-06e8-4c7b-8c09-4bcbef67f387", response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{PrincipalUri: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/fad6089a-06e8-4c7b-8c09-4bcbef67f387"}}}},
		{name: "operator principal is not a device", enrolled: true, org: testOperatorTenant, response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{PrincipalUri: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/alice"}}}},
		{name: "missing organization is still enrolled", enrolled: true, response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{}}}},
		{name: "empty is unknown", response: &pb.IsProvisionedResponse{}, wantError: true},
		{name: "unreachable is unknown", err: status.Error(codes.Unavailable, "offline"), wantError: true},
		{name: "permission denied is unknown", err: status.Error(codes.PermissionDenied, "denied"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedConfig(t, &config.Config{})
			startUDSAgent(t, func(s *grpc.Server) {
				pb.RegisterWendyProvisioningServiceServer(s, infoProvisioningV2Server{response: tc.response, err: tc.err})
			})
			conn, err := DesktopConnect(context.Background(), "test")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			value, err := DesktopDeviceEnrollment(context.Background(), conn)
			if (err != nil) != tc.wantError {
				t.Fatalf("status = %+v, %v", value, err)
			}
			if err == nil && (value.Enrolled != tc.enrolled || value.OrganizationID != tc.org || value.DeviceID != tc.device) {
				t.Fatalf("status = %+v", value)
			}
		})
	}
}

func TestDesktopEnrollmentLegacyStatus(t *testing.T) {
	seedConfig(t, &config.Config{})
	startUDSAgent(t, func(s *grpc.Server) {
		legacy.RegisterWendyProvisioningServiceServer(s, infoProvisioningV1Server{response: &legacy.IsProvisionedResponse{Response: &legacy.IsProvisionedResponse_NotProvisioned{NotProvisioned: &legacy.NotProvisionedResponse{}}}})
	})
	conn, err := DesktopConnect(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	value, err := DesktopDeviceEnrollment(context.Background(), conn)
	if err != nil || value.Enrolled {
		t.Fatalf("legacy status = %+v, %v", value, err)
	}
}

type desktopEnrollingAgent struct {
	acmeProvisioningServer
	unverified bool
}

func (s *desktopEnrollingAgent) IsProvisioned(ctx context.Context, req *pb.IsProvisionedRequest) (*pb.IsProvisionedResponse, error) {
	if s.req == nil || s.unverified {
		return s.acmeProvisioningServer.IsProvisioned(ctx, req)
	}
	return &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{PrincipalUri: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/" + s.req.DeviceId}}}, nil
}

func TestDesktopEnrollmentUsesChosenOrganizationAndVerifies(t *testing.T) {
	for _, mode := range []string{"success", "unverified", "already enrolled", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			cloud := &oidcEnrollmentServer{response: &cloudpb.EnrollDeviceResponse{AssetId: "asset-uuid", CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
			agent := &desktopEnrollingAgent{unverified: mode == "unverified"}
			agent.enrolled = mode == "already enrolled"
			if mode == "unknown" {
				agent.preflightErr = status.Error(codes.Unavailable, "offline")
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			cloudpb.RegisterDeviceEnrollmentServiceServer(server, cloud)
			cloudpb.RegisterOperatorSessionServiceServer(server, cloud)
			pb.RegisterWendyProvisioningServiceServer(server, agent)
			pb.RegisterWendyDeviceInfoServiceServer(server, agent)
			go server.Serve(listener)
			defer server.Stop()
			client, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			auth := oidcEnrollmentAuth(t)
			auth.Name = "chosen"
			auth.CloudGRPC = listener.Addr().String()
			load := seedConfig(t, &config.Config{CurrentContext: "other", Auth: []config.AuthConfig{{Name: "other"}, *auth}})
			original := ensureFreshCertificateFn
			t.Cleanup(func() { ensureFreshCertificateFn = original })
			ensureFreshCertificateFn = func(_ context.Context, selected *config.AuthConfig) error {
				if selected.Name != "chosen" {
					return errors.New("wrong account")
				}
				return nil
			}
			conn := &grpcclient.AgentConnection{Conn: client, Host: "sim.local"}
			_, err = DesktopEnrollDevice(context.Background(), conn, "chosen", testOperatorTenant, "test-device")
			if (err == nil) != (mode == "success") {
				t.Fatalf("enroll = %v", err)
			}
			if mode == "already enrolled" || mode == "unknown" {
				if cloud.req != nil || agent.req != nil {
					t.Fatal("mutated Cloud/device after failed preflight")
				}
			} else {
				if cloud.req.GetName() != "test-device" || agent.req == nil {
					t.Fatal("did not enroll chosen device")
				}
				claims := verifyEnrollmentJWS(t, string(cloud.req.GetEnrollmentRequestJws()))
				if claims["tenant"] != testOperatorTenant {
					t.Fatal("wrong enrollment tenant")
				}
			}
			if load().CurrentContext != "other" {
				t.Fatal("changed global account selection")
			}
		})
	}
}

func TestDesktopEnrollmentRejectsStaleOrganizationChoice(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	auth.Name = "chosen"
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	for _, choice := range [][2]string{{"", ""}, {"removed", testOperatorTenant}, {"chosen", "different-tenant"}} {
		if _, err := DesktopEnrollDevice(context.Background(), nil, choice[0], choice[1], "test"); err == nil {
			t.Fatal("accepted stale choice")
		}
	}
}

func TestDesktopEnrollmentOrganizationsRetainEverySavedTenant(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	auth.Name = "first"
	// Expired display metadata avoids network reads and still offers sign-in recovery.
	auth.OAuthExpiresAt = "2000-01-01T00:00:00Z"
	second := *auth
	second.Name = "second"
	second.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
	second.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/00000000-0000-4000-8000-000000000002/operator/" + testOperatorSubject
	seedConfig(t, &config.Config{CurrentContext: "first", Auth: []config.AuthConfig{*auth, second, {Name: "signed-out"}}})
	choices, err := DesktopEnrollmentOrganizations(context.Background())
	if err != nil || len(choices) != 2 {
		t.Fatalf("choices = %+v, %v", choices, err)
	}
	if choices[0].ID == choices[1].ID || choices[1].Account != "second" {
		t.Fatalf("lost tenant selection: %+v", choices)
	}
}
