package mcp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestMCPCloudConnectionConstructorSeparatesLegacyMeshHostFromV2(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"
	for _, test := range []struct {
		name, wantMesh string
		v2             bool
		legacyID       int32
	}{
		{"legacy numeric asset", "device-42.mesh.wendy.internal", false, 42},
		{"UUID asset", "", true, 0},
		{"UUID ignores stale legacy field", "", true, 42},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			operator := ""
			device := mcpCloudDevice{name: "42", legacyID: test.legacyID, key: "42", isV2: test.v2}
			broker := listener.Addr().String()
			selectorSuffix := "/org/7/asset/42"
			if test.v2 {
				operator = "spiffe://wendy.sh/tenant/" + testV2Tenant + "/operator/test"
				device.key = uuid
				broker = ""
				selectorSuffix = "/tenant/" + testV2Tenant + "/asset/" + uuid
			}
			certificate, _ := mcpTLSIdentityFixture(t, operator, certs.AssetURN(7, 42), "")
			certificate.UserID = "test"
			auth := &config.AuthConfig{CloudGRPC: listener.Addr().String(), Certificates: []config.CertificateInfo{certificate}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			connection, selected, target, err := connectPinnedMCPCloudAgent(ctx, auth, device, broker)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if selected != device || !strings.HasSuffix(target.Selector, selectorSuffix) {
				t.Fatalf("constructor changed the selected device: %+v %+v", selected, target)
			}
			// MCP reconnect constructs another lazy connection pinned to this
			// same inventory row; it must not manufacture a numeric v2 address.
			reconnected, err := connection.Reconnect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer reconnected.Close()
			for _, candidate := range []*grpcclient.AgentConnection{connection, reconnected} {
				if candidate.MeshHost != test.wantMesh || candidate.Host != device.name {
					t.Fatalf("wrong legacy/v2 mesh routing: mesh=%q host=%q, want %q %q", candidate.MeshHost, candidate.Host, test.wantMesh, device.name)
				}
			}
			if !connection.IsMTLS || connection.CertInfo == nil || connection.RegistryDialer == nil || connection.AgentService == nil {
				t.Fatal("constructor failed to install the authenticated Cloud connection")
			}
			if _, verified := connection.ObservedServerIdentity(); verified {
				t.Fatal("address construction claimed a verified peer without a handshake")
			}
		})
	}
}
