package interceptor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const rpcTestTenant = "11111111-1111-4111-8111-111111111111"

// These strings intentionally do not use generated constants: this test is the
// independently reviewed list of device capabilities, including the API version.
var rpcTestDeviceMethods = []string{
	"/wendy.agent.services.v1.WendyContainerService/QueryChunks",
	"/wendy.agent.services.v1.WendyContainerService/QueryLayers",
	"/wendy.agent.services.v1.WendyContainerService/WriteChunks",
	"/wendy.agent.services.v1.WendyContainerService/PrepareImage",
	"/wendy.agent.services.v2.WendySensorService/GetSensorManifest",
	"/wendy.agent.services.v2.WendySensorService/StreamSensors",
}

func rpcIdentityLeaf(t *testing.T, cn string, uris ...string) *x509.Certificate {
	t.Helper()
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		leaf.URIs = append(leaf.URIs, u)
	}
	return leaf
}

func rpcIdentityContext(leaf *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf}},
	}})
}

type rpcContextStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s rpcContextStream) Context() context.Context { return s.ctx }

type rpcNonTLSAuth struct{}

func (rpcNonTLSAuth) AuthType() string { return "not-tls" }

// Check both wrappers for each decision; a rejection must prevent any handler
// effect, while successful authentication must preserve the handler's response.
func assertRPCDecision(t *testing.T, ctx context.Context, method string, scope certs.Scope, want codes.Code) {
	t.Helper()
	t.Run("unary", func(t *testing.T) {
		called := false
		marker := new(int)
		response, err := UnaryRPCIdentityInterceptor(scope)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) {
			called = true
			return marker, nil
		})
		if status.Code(err) != want || called != (want == codes.OK) {
			t.Fatalf("code=%v handlerCalled=%v; want %v", status.Code(err), called, want)
		}
		if want == codes.OK && response != marker {
			t.Fatal("successful interceptor did not preserve handler response")
		}
	})
	t.Run("stream", func(t *testing.T) {
		called := false
		err := StreamRPCIdentityInterceptor(scope)(nil, rpcContextStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: method, IsClientStream: true, IsServerStream: true}, func(any, grpc.ServerStream) error {
			called = true
			return nil
		})
		if status.Code(err) != want || called != (want == codes.OK) {
			t.Fatalf("code=%v handlerCalled=%v; want %v", status.Code(err), called, want)
		}
	})
}

func TestRPCDeviceExactMethods(t *testing.T) {
	ctx := rpcIdentityContext(rpcIdentityLeaf(t, "", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/device/peer"))
	scope := certs.Scope{TenantUUID: rpcTestTenant}
	for _, method := range rpcTestDeviceMethods {
		t.Run(method, func(t *testing.T) { assertRPCDecision(t, ctx, method, scope, codes.OK) })
	}
	denied := []string{
		"/wendy.agent.services.v1.WendyShellService/HostShell",
		"/wendy.agent.services.v1.WendyContainerService/RunContainer",
		"/wendy.agent.services.v1.WendyContainerService/CreateContainer",
		"/wendy.agent.services.v1.WendyContainerService/ExecContainer",
		"/wendy.agent.services.v1.WendyContainerService/ListContainers",
		"/wendy.agent.services.v1.WendyContainerService/WriteLayer",
		"/wendy.agent.services.v1.WendyAgentService/GetAgentVersion",
		"/wendy.agent.services.v1.WendyAgentService/MeshDial",
		"/wendy.agent.services.v1.WendyWiFiService/Connect",
		"/wendy.agent.services.v1.WendyCameraService/StreamCamera",
		"/wendy.agent.services.v1.WendyAudioService/StreamAudio",
		"/wendy.agent.services.v2.WendyContainerService/QueryChunks",
		"/wendy.agent.services.v1.WendySensorService/StreamSensors",
		"/wendy.agent.services.v2.WendySensorService/PairSensor",
		"/future.AdminService/NewMethod",
		"/wendy.agent.services.v1.WendyContainerService/PrepareImageAndRun",
		"/wendy.agent.services.v1.WendyContainerService/PrepareImage/Run",
		"/wendy.agent.services.v1.WendyContainerService/prepareImage",
		"/wendy.agent.services.v1.WendyContainerServiceExtra/PrepareImage",
		"/wendy.agent.services.v1.OtherService/PrepareImage",
		"wendy.agent.services.v1.WendyContainerService/PrepareImage",
		"/wendy.agent.services.v1.WendyContainerService/PrepareImage ",
		"",
	}
	for _, method := range denied {
		t.Run(method, func(t *testing.T) { assertRPCDecision(t, ctx, method, scope, codes.PermissionDenied) })
	}
}

func TestRPCIdentityRoles(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/" + rpcTestTenant + "/"
	tests := []struct {
		name   string
		cn     string
		uris   []string
		scope  certs.Scope
		device bool
		denied bool
	}{
		{name: "direct device", uris: []string{principal + "device/peer"}, scope: certs.Scope{TenantUUID: rpcTestTenant}, device: true},
		{name: "relayed asset", uris: []string{principal + "service/asset-12"}, scope: certs.Scope{TenantUUID: rpcTestTenant}, device: true},
		{name: "legacy asset URN", uris: []string{"urn:wendy:org:7:asset:12"}, scope: certs.Scope{OrgID: 7}, device: true},
		{name: "legacy asset CN", cn: "sh/wendy/7/12", scope: certs.Scope{OrgID: 7}, device: true},
		{name: "operator", uris: []string{principal + "operator/human"}, scope: certs.Scope{TenantUUID: rpcTestTenant}},
		{name: "relayed user", uris: []string{principal + "service/user-human"}, scope: certs.Scope{TenantUUID: rpcTestTenant}},
		{name: "service machine user", uris: []string{principal + "service/build-account"}, scope: certs.Scope{TenantUUID: rpcTestTenant}},
		{name: "legacy user", uris: []string{"urn:wendy:org:7:user:human"}, scope: certs.Scope{OrgID: 7}},
		{name: "signer", uris: []string{principal + "signer/code"}, scope: certs.Scope{TenantUUID: rpcTestTenant}, denied: true},
		{name: "unknown role", uris: []string{principal + "admin/root"}, scope: certs.Scope{TenantUUID: rpcTestTenant}, denied: true},
		{name: "unknown certificate", cn: "some legacy name", scope: certs.Scope{OrgID: 7}, denied: true},
		{name: "legacy unrecognized user CN", cn: "wendy/user/human", scope: certs.Scope{OrgID: 7}, denied: true},
		{name: "malformed org", uris: []string{"urn:wendy:org:0:user:human"}, scope: certs.Scope{OrgID: 7}, denied: true},
		{name: "malformed tenant", uris: []string{"spiffe://wendy.sh/tenant/not-a-uuid/operator/human"}, scope: certs.Scope{OrgID: 7}, denied: true},
		{name: "duplicate principal", uris: []string{principal + "operator/human", principal + "device/peer"}, scope: certs.Scope{TenantUUID: rpcTestTenant}, denied: true},
		{name: "duplicate legacy URN", uris: []string{"urn:wendy:org:7:user:human", "urn:wendy:org:7:asset:12"}, scope: certs.Scope{OrgID: 7}, denied: true},
		{name: "principal device overrides legacy user", uris: []string{principal + "device/peer", "urn:wendy:org:7:user:human"}, scope: certs.Scope{TenantUUID: rpcTestTenant}, device: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := rpcIdentityContext(rpcIdentityLeaf(t, tt.cn, tt.uris...))
			for _, method := range []string{rpcTestDeviceMethods[0], "/wendy.agent.services.v1.WendyShellService/HostShell", "/future.AdminService/NewMethod"} {
				want := codes.OK
				if tt.denied || tt.device && method != rpcTestDeviceMethods[0] {
					want = codes.PermissionDenied
				}
				t.Run(method, func(t *testing.T) { assertRPCDecision(t, ctx, method, tt.scope, want) })
			}
		})
	}
}

func TestRPCIdentityIgnoresMetadataRoles(t *testing.T) {
	leaf := rpcIdentityLeaf(t, "", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/device/peer")
	ctx := metadata.NewIncomingContext(rpcIdentityContext(leaf), metadata.Pairs("x-wendy-role", "user", "x-wendy-entity-type", "user", "authorization", "operator"))
	assertRPCDecision(t, ctx, "/wendy.agent.services.v1.WendyShellService/HostShell", certs.Scope{TenantUUID: rpcTestTenant}, codes.PermissionDenied)
}

func TestRPCIdentityRequiresCompletedTLS(t *testing.T) {
	leaf := rpcIdentityLeaf(t, "", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/operator/human")
	contexts := map[string]context.Context{
		"missing peer":         context.Background(),
		"missing auth":         peer.NewContext(context.Background(), &peer.Peer{}),
		"non TLS auth":         peer.NewContext(context.Background(), &peer.Peer{AuthInfo: rpcNonTLSAuth{}}),
		"empty certificate":    peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{HandshakeComplete: true}}}),
		"incomplete handshake": peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}}}),
	}
	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			assertRPCDecision(t, ctx, rpcTestDeviceMethods[0], certs.Scope{TenantUUID: rpcTestTenant}, codes.Unauthenticated)
		})
	}
}

func TestRPCDeviceTenantCannotUseMigrationGrace(t *testing.T) {
	tests := []struct {
		name  string
		leaf  *x509.Certificate
		scope certs.Scope
		want  codes.Code
		gate  [3]codes.Code // off, grace, strict
	}{
		{"matching", rpcIdentityLeaf(t, "sh/wendy/7/12"), certs.Scope{OrgID: 7}, codes.OK, [3]codes.Code{codes.OK, codes.OK, codes.OK}},
		{"other org", rpcIdentityLeaf(t, "sh/wendy/8/12"), certs.Scope{OrgID: 7}, codes.PermissionDenied, [3]codes.Code{codes.OK, codes.PermissionDenied, codes.PermissionDenied}},
		{"incomparable asset scope", rpcIdentityLeaf(t, "sh/wendy/7/12"), certs.Scope{TenantUUID: rpcTestTenant}, codes.PermissionDenied, [3]codes.Code{codes.OK, codes.OK, codes.PermissionDenied}},
		{"unknown target scope", rpcIdentityLeaf(t, "sh/wendy/7/12"), certs.Scope{}, codes.PermissionDenied, [3]codes.Code{codes.OK, codes.PermissionDenied, codes.PermissionDenied}},
		{"incomparable operator scope preserves grace", rpcIdentityLeaf(t, "", "urn:wendy:org:7:user:human"), certs.Scope{TenantUUID: rpcTestTenant}, codes.OK, [3]codes.Code{codes.OK, codes.OK, codes.PermissionDenied}},
	}
	for _, tt := range tests {
		for _, mode := range []OrgMode{OrgModeOff, OrgModeGrace, OrgModeStrict} {
			t.Run(tt.name+"/"+mode.String(), func(t *testing.T) {
				ctx := rpcIdentityContext(tt.leaf)
				// Independently assert the scope gate's configured decision, then
				// prove the new gate still refuses an unprovable asset tenant.
				if got := status.Code(CheckMTLS(ctx, zap.NewNop(), tt.scope, mode)); got != tt.gate[mode] {
					t.Fatalf("scope gate=%v; want %v", got, tt.gate[mode])
				}
				assertRPCDecision(t, ctx, rpcTestDeviceMethods[0], tt.scope, tt.want)
			})
		}
	}
}

func TestRPCIdentityPreservesHandlerErrors(t *testing.T) {
	ctx := rpcIdentityContext(rpcIdentityLeaf(t, "sh/wendy/7/12"))
	scope := certs.Scope{OrgID: 7}
	want := status.Error(codes.FailedPrecondition, "handler-specific authorization")
	_, err := UnaryRPCIdentityInterceptor(scope)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: rpcTestDeviceMethods[0]}, func(context.Context, any) (any, error) { return nil, want })
	if err != want {
		t.Fatalf("unary handler error changed: %v", err)
	}
	err = StreamRPCIdentityInterceptor(scope)(nil, rpcContextStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: rpcTestDeviceMethods[0]}, func(any, grpc.ServerStream) error { return want })
	if err != want {
		t.Fatalf("stream handler error changed: %v", err)
	}
}
