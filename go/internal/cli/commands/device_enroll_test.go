package commands

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type enrollmentTokenServer struct {
	cloudpb.UnimplementedCertificateServiceServer
	request *cloudpb.CreateAssetEnrollmentTokenRequest
	headers metadata.MD
}

func (s *enrollmentTokenServer) CreateAssetEnrollmentToken(ctx context.Context, req *cloudpb.CreateAssetEnrollmentTokenRequest) (*cloudpb.CreateAssetEnrollmentTokenResponse, error) {
	s.request = req
	s.headers, _ = metadata.FromIncomingContext(ctx)
	return &cloudpb.CreateAssetEnrollmentTokenResponse{
		OrganizationId: 42, AssetId: 99, EnrollmentToken: "enrollment-token",
	}, nil
}

type enrollmentProvisioningClient struct {
	agentpb.WendyProvisioningServiceClient
	request *agentpb.StartProvisioningRequest
}

func (c *enrollmentProvisioningClient) StartProvisioning(_ context.Context, req *agentpb.StartProvisioningRequest, _ ...grpc.CallOption) (*agentpb.StartProvisioningResponse, error) {
	c.request = req
	return &agentpb.StartProvisioningResponse{}, nil
}

func TestRunEnrollDeviceUsesSessionOrganization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		org      int
		override int32
		operator bool
		want     int32
	}{
		{name: "session org", org: 7, want: 7},
		{name: "explicit override", org: 7, override: 9, want: 9},
		{name: "operator tenant without legacy org ID", operator: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := resolveOrgFn
			resolveOrgFn = func(context.Context, *config.AuthConfig, bool) (OrgResolution, error) {
				t.Fatal("enrollment must not list or pick organizations")
				return OrgResolution{}, nil
			}
			t.Cleanup(func() { resolveOrgFn = orig })

			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			svc := &enrollmentTokenServer{}
			server := grpc.NewServer()
			cloudpb.RegisterCertificateServiceServer(server, svc)
			go server.Serve(lis) //nolint:errcheck
			t.Cleanup(server.Stop)

			auth := fakeAuth(t)
			auth.CloudGRPC = lis.Addr().String()
			auth.Certificates[0].OrganizationID = tc.org
			if tc.operator {
				key, err := parseECPrivateKeyPEM(auth.Certificates[0].PemPrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				auth.Certificates[0].PemCertificate = testLeafPEM(t, key)
				auth.Certificates[0].PemCertificateChain = ""
				auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject
			}
			provisioning := &enrollmentProvisioningClient{}
			conn := &grpcclient.AgentConnection{ProvisioningService: provisioning}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runEnrollDevice(ctx, conn, auth, "simulator", tc.override); err != nil {
				t.Fatal(err)
			}
			if svc.request.GetOrganizationId() != tc.want || svc.request.GetName() != "simulator" {
				t.Fatalf("unexpected token request: %v", svc.request)
			}
			if tc.operator && !strings.Contains(strings.Join(svc.headers.Get("x-wendy-client-cert"), ""), auth.Certificates[0].PrincipalURI) {
				t.Fatal("operator tenant identity missing from token request")
			}
			if provisioning.request.GetOrganizationId() != 42 || provisioning.request.GetAssetId() != 99 || provisioning.request.GetEnrollmentToken() != "enrollment-token" || provisioning.request.GetCloudHost() != auth.CloudGRPC {
				t.Fatalf("unexpected provisioning request: %v", provisioning.request)
			}
		})
	}
}
