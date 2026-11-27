package mcp

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

// A device that cannot reach PKI is refused before Cloud reserves its name:
// the reservation outlives the failure and keeps the name taken.
func TestCloudEnrollStopsBeforeCloudForOfflineDevice(t *testing.T) {
	supported := true
	fake := &enrollmentReadinessServer{state: &agentpbv2.NotProvisionedResponse{AcmeEnrollmentSupported: &supported}}
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

	const tenant = "2558fd76-afc7-466e-9613-6b715296a526"
	var directory string
	old := checkDeviceInternet
	t.Cleanup(func() { checkDeviceInternet = old })
	checkDeviceInternet = func(_ context.Context, _ grpc.ClientConnInterface, directoryURL string) error {
		directory = directoryURL
		return errors.New("this device has no route to the internet")
	}
	// The Cloud session is unusable: a refusal has to come before it is dialled.
	srv := New(&config.Config{Auth: []config.AuthConfig{{
		CloudGRPC:    "unused.invalid:443",
		PKIEndpoint:  "https://identity.pki.example/v1/identity/certificate",
		Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/fixture"}},
	}}}, nil)
	srv.SetConn(&grpcclient.AgentConnection{Conn: conn})
	result, err := srv.callTool(context.Background(), "cloud_enroll_device", map[string]any{"name": "fixture-device"})
	if err != nil {
		t.Fatal(err)
	}
	if got := structuredMap(t, result)["message"]; !result.IsError || got != "this device has no route to the internet" {
		t.Fatalf("result = %v, want the offline refusal", got)
	}
	if want := "https://acme.pki.example/" + tenant + "/acme/directory"; directory != want {
		t.Fatalf("preflight judged PKI %q, want the session's ACME directory %q", directory, want)
	}
	if fake.started {
		t.Fatal("an offline device received enrollment credentials")
	}
}

// After Cloud reserved the asset, a device-side failure has to name it and the
// command that removes it; the bare device error says neither.
func TestCloudEnrollFailureAfterReservationNamesCleanup(t *testing.T) {
	result := reservedEnrollmentFailure("7b0e6c0a-3c55-4f0e-9d7e-0f2d5a1c9b11",
		status.Error(codes.Internal, "enrolling with PKI: dial udp 9.9.9.9:53: connect: network is unreachable"))
	fields := structuredMap(t, result)
	want := "Cloud registered asset 7b0e6c0a-3c55-4f0e-9d7e-0f2d5a1c9b11 and reserved the device name, but device PKI enrollment failed: " +
		"enrolling with PKI: dial udp 9.9.9.9:53: connect: network is unreachable. " +
		"Before enrolling again, remove the reserved asset by running 'wendy cloud device unenroll --device 7b0e6c0a-3c55-4f0e-9d7e-0f2d5a1c9b11' in a terminal."
	if !result.IsError || fields["error_code"] != string(errCodeInternal) || fields["message"] != want {
		t.Fatalf("result = %v", fields)
	}
}
