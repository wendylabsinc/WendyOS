package mcp

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type enrollmentReadinessServer struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	state   *agentpbv2.NotProvisionedResponse
	started bool
}

func (s *enrollmentReadinessServer) IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{NotProvisioned: s.state}}, nil
}

func (s *enrollmentReadinessServer) StartACMEProvisioning(context.Context, *agentpbv2.StartACMEProvisioningRequest) (*agentpbv2.StartACMEProvisioningResponse, error) {
	s.started = true
	return &agentpbv2.StartACMEProvisioningResponse{}, nil
}

func TestCloudEnrollStopsBeforeCloudForUnreadyAgent(t *testing.T) {
	unsupported := false
	for _, tc := range []struct {
		name string
		flag *bool
		want string
	}{
		{"unsupported", &unsupported, "does not support"},
		{"unknown", nil, "update the agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &enrollmentReadinessServer{state: &agentpbv2.NotProvisionedResponse{AcmeEnrollmentSupported: tc.flag}}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			g := grpc.NewServer()
			agentpbv2.RegisterWendyProvisioningServiceServer(g, fake)
			go func() { _ = g.Serve(ln) }()
			t.Cleanup(g.Stop)
			conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			// Deliberately unusable Cloud session: readiness must stop before
			// config validation, authentication, dial or credential minting.
			srv := New(&config.Config{Auth: []config.AuthConfig{{CloudGRPC: "unused.invalid:443", Certificates: []config.CertificateInfo{{OrganizationID: 1}}}}}, nil)
			srv.SetConn(&grpcclient.AgentConnection{Conn: conn})
			result, err := srv.callTool(context.Background(), "cloud_enroll_device", map[string]any{"name": "fixture-device"})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.Contains(structuredMap(t, result)["message"].(string), tc.want) {
				t.Fatal("MCP did not stop at readiness preflight")
			}
			if fake.started {
				t.Fatal("unready agent received a mutating request")
			}
		})
	}
}
