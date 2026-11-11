//go:build darwin || linux

package commands

import (
	"context"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/buildassignment"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/testutil/deliverypki"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type absentDeliveryImage struct{}

func (absentDeliveryImage) ImageDigest(context.Context, string) (string, error) {
	return "", errdefs.ErrNotFound
}

func newBuildAuthorizationFixture(t *testing.T) (*deliverypki.Fixture, *config.CertificateInfo) {
	t.Helper()
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	f := deliverypki.New(t)
	primary := f.Mint(t, deliverypki.OperatorPrincipal)
	operator := &config.CertificateInfo{PemCertificate: primary.PEM, PemCertificateChain: f.Chain, PemPrivateKey: primary.Key, PrincipalURI: deliverypki.OperatorPrincipal}
	return f, operator
}

func connectBuildAuthorizationAgent(t *testing.T, f *deliverypki.Fixture, operator *config.CertificateInfo, credential deliverypki.Credential, register func(*grpc.Server)) *grpcclient.AgentConnection {
	t.Helper()
	server, err := mtls.NewServer(credential.PEM, f.Chain, credential.Key, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: deliverypki.Tenant}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	register(server)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	expected, err := certs.ParsePrincipal(credential.Leaf.URIs[0].String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpcclient.ConnectWithTLSExpecting(t.Context(), listener.Addr().String(), operator, nil, &expected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Both TLS connections use the same existing operator credential to authorize
// delivery. The fixture has no Cloud session or delivery-credential cache.
func TestAuthorizeRemoteBuildWithExistingOperatorCertificate(t *testing.T) {
	f, operator := newBuildAuthorizationFixture(t)
	targetManager := f.Manager(t)
	workerManager := delivery.NewManager(t.TempDir(), "localhost:5000/", func() (string, string) { return f.Builder.PEM, f.Chain })
	t.Cleanup(workerManager.Close)
	builder := connectBuildAuthorizationAgent(t, f, operator, f.Builder, func(server *grpc.Server) {
		agentpbv2.RegisterWendyBuildDeliveryServiceServer(server, services.NewBuildDeliveryService(workerManager.Get, absentDeliveryImage{}))
		agentpbv2.RegisterWendyBuildServiceServer(server, services.NewBuildService(zap.NewNop(), services.BuildServiceOptions{ConfigPath: t.TempDir(), DeliverySource: workerManager.Get}))
	})
	target := connectBuildAuthorizationAgent(t, f, operator, f.Target, func(server *grpc.Server) {
		agentpbv2.RegisterWendyBuildDeliveryServiceServer(server, services.NewBuildDeliveryService(targetManager.Get, absentDeliveryImage{}))
	})
	spec := &agentpbv2.BuildSpec{AppId: "app", Platform: "linux/arm64", PushTarget: &agentpbv2.PushTarget{AssetUuid: f.TargetID.UUID().String(), Repository: "app:latest", RegistryPort: 5000}, Definition: &agentpbv2.BuildSpec_DockerfileBuild{DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile", BuildArgs: map[string]string{"VERSION": "one"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := authorizeRemoteBuild(ctx, builder, spec, map[string]*grpcclient.AgentConnection{f.TargetID.UUID().String(): target}); err != nil {
		t.Fatal(err)
	}
	input, err := buildassignment.InputSHA256(spec)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := builddelivery.VerifyBuildBinding(spec.PushTarget.DeliveryAuthorizationJws, spec.PushTarget.DeliverySession, input, []*x509.Certificate{f.Root}, f.TargetID, f.Builder.Leaf, time.Now())
	if err != nil || claims.Reference != "app:latest" {
		t.Fatalf("binding: %+v %v", claims, err)
	}
	snapshot, err := targetManager.Get()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Store.Policy(spec.PushTarget.DeliverySession, f.Builder.Leaf); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Store.Policy(spec.PushTarget.DeliverySession, f.OtherBuilder.Leaf); err == nil {
		t.Fatal("grant accepted another device key")
	}
	// Different build arguments invalidate the same signed assignment.
	spec.GetDockerfileBuild().BuildArgs["VERSION"] = "two"
	changed, _ := buildassignment.InputSHA256(spec)
	if _, err := builddelivery.VerifyBuildBinding(spec.PushTarget.DeliveryAuthorizationJws, spec.PushTarget.DeliverySession, changed, []*x509.Certificate{f.Root}, f.TargetID, f.Builder.Leaf, time.Now()); err == nil {
		t.Fatal("changed input accepted")
	}
}

type buildHostCapabilityStub struct {
	agentpbv2.UnimplementedWendyBuildServiceServer
	authorizedDelivery bool
}

func (b buildHostCapabilityStub) GetBuildCapabilities(context.Context, *agentpbv2.GetBuildCapabilitiesRequest) (*agentpbv2.GetBuildCapabilitiesResponse, error) {
	return &agentpbv2.GetBuildCapabilitiesResponse{AuthorizedDelivery: b.authorizedDelivery}, nil
}

type deliveryCapabilityStub struct {
	agentpbv2.UnimplementedWendyBuildDeliveryServiceServer
	capabilities *agentpbv2.GetDeliveryCapabilitiesResponse
	err          error
}

func (d deliveryCapabilityStub) GetDeliveryCapabilities(context.Context, *agentpbv2.GetDeliveryCapabilitiesRequest) (*agentpbv2.GetDeliveryCapabilitiesResponse, error) {
	return d.capabilities, d.err
}

func TestAuthorizeRemoteBuildNegotiatesLegacyDeliveryOverMTLS(t *testing.T) {
	f, operator := newBuildAuthorizationFixture(t)
	builder := func(supportsDelivery bool) *grpcclient.AgentConnection {
		return connectBuildAuthorizationAgent(t, f, operator, f.Builder, func(server *grpc.Server) {
			agentpbv2.RegisterWendyBuildServiceServer(server, buildHostCapabilityStub{authorizedDelivery: supportsDelivery})
		})
	}
	oldBuilder, newBuilder := builder(false), builder(true)
	oldTarget := connectBuildAuthorizationAgent(t, f, operator, f.Target, func(*grpc.Server) {})
	serveCapabilities := func(version uint32, err error) *grpcclient.AgentConnection {
		return connectBuildAuthorizationAgent(t, f, operator, f.Target, func(server *grpc.Server) {
			agentpbv2.RegisterWendyBuildDeliveryServiceServer(server, deliveryCapabilityStub{
				capabilities: &agentpbv2.GetDeliveryCapabilitiesResponse{
					ContractVersion: version, TargetPrincipal: deliverypki.TargetPrincipal,
				},
				err: err,
			})
		})
	}
	newTarget := serveCapabilities(builddelivery.Version, nil)
	futureTarget := serveCapabilities(builddelivery.Version+1, nil)
	denyingTarget := serveCapabilities(0, status.Error(codes.PermissionDenied, "policy denied the probe"))
	unavailableTarget := serveCapabilities(0, status.Error(codes.Unavailable, "probe failed"))
	assetID := f.TargetID.UUID().String()
	for _, tc := range []struct {
		name    string
		builder *grpcclient.AgentConnection
		target  *grpcclient.AgentConnection
		wantErr string
	}{
		{"both legacy", oldBuilder, oldTarget, ""},
		{"old builder only", oldBuilder, newTarget, "update the build host"},
		{"old target only", newBuilder, oldTarget, "update target"},
		{"future contract", newBuilder, futureTarget, "unsupported contract"},
		{"permission denied", oldBuilder, denyingTarget, "policy denied the probe"},
		{"probe unavailable", oldBuilder, unavailableTarget, "probe failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			pushTarget := &agentpbv2.PushTarget{AssetUuid: assetID, Repository: "app:latest"}
			spec := &agentpbv2.BuildSpec{PushTarget: pushTarget}
			err := authorizeRemoteBuild(ctx, tc.builder, spec, map[string]*grpcclient.AgentConnection{assetID: tc.target})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want %q", err, tc.wantErr)
			}
			if pushTarget.GetDeliveryAuthorizationJws() != "" || pushTarget.GetDeliverySession() != "" {
				t.Fatal("legacy or rejected delivery acquired a grant")
			}
		})
	}
	t.Run("mixed fleet", func(t *testing.T) {
		const legacyID = "55555555-5555-4555-8555-555555555555"
		credential := f.Mint(t, "spiffe://wendy.sh/tenant/"+deliverypki.Tenant+"/device/"+legacyID)
		legacyTarget := connectBuildAuthorizationAgent(t, f, operator, credential, func(*grpc.Server) {})
		spec := &agentpbv2.BuildSpec{PushTargets: []*agentpbv2.PushTarget{
			{AssetUuid: assetID, Repository: "app:latest"},
			{AssetUuid: legacyID, Repository: "app:latest"},
		}}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		err := authorizeRemoteBuild(ctx, newBuilder, spec, map[string]*grpcclient.AgentConnection{
			assetID: newTarget, legacyID: legacyTarget,
		})
		if err == nil || !strings.Contains(err.Error(), "update target "+legacyID) {
			t.Fatalf("mixed fleet returned %v, want error naming the legacy target", err)
		}
		for _, target := range spec.GetPushTargets() {
			if target.GetDeliveryAuthorizationJws() != "" || target.GetDeliverySession() != "" {
				t.Fatal("issued a grant before checking the entire fleet")
			}
		}
	})
}
