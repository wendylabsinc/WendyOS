package commands

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// These are real lazy gRPC/TLS connection constructors, not manually populated
// AgentConnections. Relay RPC and peer-authentication behavior are exercised by
// the existing Cloud tunnel/TLS suites; this pins the address metadata they use.
func TestCloudConnectionConstructorSeparatesLegacyMeshHostFromV2(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"
	for _, test := range []struct {
		name, display, wantHost, wantMesh string
		v2                                bool
	}{
		{"legacy numeric asset", "legacy-board", "legacy-board", "device-42.mesh.wendy.internal", false},
		{"UUID with numeric display name", "42", "42", "", true},
		{"UUID without display name", "", uuid, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			auth, _ := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
			auth.CloudGRPC = listener.Addr().String()
			legacy := &cloudpb.Asset{Id: 42, Name: test.display}
			asset := cloudDiscoveryDevice{cloudAssetMetadata: legacy, legacy: legacy, key: "42"}
			broker := listener.Addr().String()
			if test.v2 {
				certificates, _ := liteRelayTestPKI(t)
				auth = &config.AuthConfig{CloudGRPC: listener.Addr().String(), Certificates: certificates}
				v2 := &cloudpbv2.Asset{Id: uuid, Name: test.display}
				asset = cloudDiscoveryDevice{cloudAssetMetadata: v2, v2: v2, key: uuid}
				broker = ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			connection, err := connectCloudDiscoveryDevice(ctx, auth, asset, broker)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if connection.MeshHost != test.wantMesh {
				t.Fatalf("wrong legacy/v2 mesh routing: got %q, want %q", connection.MeshHost, test.wantMesh)
			}
			if connection.Host != test.wantHost || connection.Addr != "" || connection.IsCloudV2 != test.v2 {
				t.Fatalf("wrong Cloud connection identity: host=%q addr=%q v2=%t", connection.Host, connection.Addr, connection.IsCloudV2)
			}
			if !connection.IsMTLS || connection.CertInfo == nil || connection.RegistryDialer == nil || connection.Reconnect == nil || connection.AgentService == nil {
				t.Fatal("constructor failed to install the authenticated Cloud connection")
			}
			if _, verified := connection.ObservedServerIdentity(); verified {
				t.Fatal("address construction claimed a verified peer without a handshake")
			}
		})
	}
}
