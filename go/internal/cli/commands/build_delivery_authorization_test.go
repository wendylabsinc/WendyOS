//go:build darwin || linux

package commands

import (
	"context"
	"crypto/x509"
	"net"
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
)

type absentDeliveryImage struct{}

func (absentDeliveryImage) ImageDigest(context.Context, string) (string, error) {
	return "", errdefs.ErrNotFound
}

// Real operator mTLS connects the CLI to two independently enrolled agents.
// No Cloud session, certificate issuer or delivery-credential cache is present.
// The CLI must sign using the existing management operator certificate.
func TestAuthorizeRemoteBuildWithExistingOperatorCertificate(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	f := deliverypki.New(t)
	primary := f.Mint(t, deliverypki.OperatorPrincipal)
	operator := &config.CertificateInfo{PemCertificate: primary.PEM, PemCertificateChain: f.Chain, PemPrivateKey: primary.Key, PrincipalURI: deliverypki.OperatorPrincipal}
	targetManager := f.Manager(t)
	workerManager := delivery.NewManager(t.TempDir(), "localhost:5000/", func() (string, string) { return f.Builder.PEM, f.Chain })
	t.Cleanup(workerManager.Close)
	connect := func(credential deliverypki.Credential, manager *delivery.Manager, worker bool) *grpcclient.AgentConnection {
		t.Helper()
		server, err := mtls.NewServer(credential.PEM, f.Chain, credential.Key, zap.NewNop(), time.Time{}, certs.Scope{TenantUUID: deliverypki.Tenant}, interceptor.OrgModeStrict)
		if err != nil {
			t.Fatal(err)
		}
		agentpbv2.RegisterWendyBuildDeliveryServiceServer(server, services.NewBuildDeliveryService(manager.Get, absentDeliveryImage{}))
		if worker {
			agentpbv2.RegisterWendyBuildServiceServer(server, services.NewBuildService(zap.NewNop(), services.BuildServiceOptions{ConfigPath: t.TempDir(), DeliverySource: manager.Get}))
		}
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
		conn, err := grpcclient.ConnectWithTLSExpecting(context.Background(), listener.Addr().String(), operator, nil, &expected)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	builder, target := connect(f.Builder, workerManager, true), connect(f.Target, targetManager, false)
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
