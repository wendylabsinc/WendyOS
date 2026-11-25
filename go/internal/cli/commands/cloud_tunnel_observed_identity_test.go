package commands

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type cloudIdentityVersionServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (*cloudIdentityVersionServer) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p, _ := peer.FromContext(ctx)
	version := "full"
	if p.AuthInfo.(credentials.TLSInfo).State.DidResume {
		version = "resumed"
	}
	return &agentpb.GetAgentVersionResponse{Version: version}, nil
}

// Exercise the actual Cloud TLS configuration and prebuilt-connection wrapper.
// A successful RPC must expose its verified reporter, including when TLS resumes;
// constructing a connection or rejecting the peer must never manufacture proof.
func TestCloudTLSObservedReporterIdentity(t *testing.T) {
	if !isolatedCloudTLSProcess(t) {
		return
	}
	const org = 123
	auth, serverTLS := legacyLiteTestPKI(t, org, certs.AssetURN(org, 42))
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	agentpb.RegisterWendyAgentServiceServer(server, &cloudIdentityVersionServer{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Setenv("WENDY_TLS_SESSION_STORE", "file")
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name        string
		asset       string
		wantVersion string
	}{
		{"verified full handshake", "42", "full"},
		{"verified resumed handshake", "42", "resumed"},
		{"rejected identity", "43", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := certs.WendyIdentity{OrgID: org, EntityType: certs.EntityAsset, EntityID: tc.asset}
			agent, err := connectCloudAgentTransport(context.Background(), auth.Certificates[0], expected, clouddefaults.TunnelDialer(func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			if got, ok := agent.ObservedServerIdentity(); ok {
				t.Fatalf("lazy connection already claims a verified identity: %+v", got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			response, err := agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if tc.wantVersion == "" {
				if err == nil {
					t.Fatal("wrong identity completed an RPC")
				}
				if got, ok := agent.ObservedServerIdentity(); ok {
					t.Fatalf("rejected TLS peer published identity: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.GetVersion() != tc.wantVersion {
				t.Fatalf("handshake = %q, want %q", response.GetVersion(), tc.wantVersion)
			}
			if got, ok := agent.ObservedServerIdentity(); !ok || !got.SameEntity(expected) {
				t.Fatalf("verified reporter missing: (%+v, %v)", got, ok)
			}
		})
	}
}

func TestCloudTLSTenantReporterIdentity(t *testing.T) {
	auth, serverTLS := localLiteTestPKI(t)
	device, err := certs.ParsePrincipal(certs.DeviceSPIFFEURI(liteTestTenant, liteTestAssetID))
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	agentpb.RegisterWendyAgentServiceServer(server, &cloudIdentityVersionServer{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	for _, tc := range []struct {
		name      string
		principal string
		untrusted bool
		want      bool
	}{
		{"verified tenant identity", device.Principal, false, true},
		{"wrong device", certs.DeviceSPIFFEURI(liteTestTenant, "different-device"), false, false},
		{"wrong tenant", certs.DeviceSPIFFEURI("11111111-1111-4111-8111-111111111111", liteTestAssetID), false, false},
		{"untrusted chain", device.Principal, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := auth.Certificates[0]
			if tc.untrusted {
				other, _ := localLiteTestPKI(t)
				client.PemCertificateChain = other.Certificates[0].PemCertificateChain
			}
			expected, err := certs.ParsePrincipal(tc.principal)
			if err != nil {
				t.Fatal(err)
			}
			agent, err := connectCloudAgentTransport(context.Background(), client, expected, clouddefaults.TunnelDialer(func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			if identity, ok := agent.ObservedServerIdentity(); ok {
				t.Fatalf("unverified identity: %+v", identity)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err = agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			identity, ok := agent.ObservedServerIdentity()
			if tc.want {
				if err != nil || !ok || !identity.SameEntity(device) {
					t.Fatalf("verified reporter = (%+v, %v), RPC error = %v", identity, ok, err)
				}
			} else if err == nil || ok {
				t.Fatalf("rejected peer returned RPC error %v and identity (%+v, %v)", err, identity, ok)
			}
		})
	}
}
