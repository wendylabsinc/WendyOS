//go:build !windows

package sessionbroker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"
)

func remoteTopologyFixture(t *testing.T) (*pb.LocalMeshTopology, meshid.Device, meshid.Device) {
	t.Helper()
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	adjacent, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	target, _ := meshid.ParseDevice(tenant, "33333333-3333-4333-8333-333333333333")
	prefix := netip.PrefixFrom(target.MeshAddress(), 128).String()
	expires := time.Now().Add(time.Minute).UnixMilli()
	view := &pb.LocalMeshTopology{SelfPrincipal: adjacent.Principal(), SelfTenantId: tenant.String(), SelfDeviceId: adjacent.UUID().String(), SelfMeshAddress: adjacent.MeshAddress().String(), RuntimeAvailable: true, CapturedAtUnixMs: time.Now().UnixMilli(), Announcements: []*pb.LocalMeshAnnouncement{{Principal: target.Principal(), DeviceId: target.UUID().String(), ExpiresAtUnixMs: expires, Revision: 1, Prefix: prefix, OriginRouterId: 7, SequenceNumber: 9, TrustState: "verified-at-snapshot", AgentPort: 50052, AgentTransport: "mtls-v1", Platform: "WendyOS"}}, Routes: []*pb.LocalMeshRoute{{Prefix: prefix, OriginPrincipal: target.Principal(), OriginRouterId: 7, SequenceNumber: 9, OriginAuthorized: true, SignedRouteExpiresAtUnixMs: expires}}}
	return view, adjacent, target
}
func TestOperatorRemoteNormalAnnouncementMoveAndOffline(t *testing.T) {
	view, adjacent, target := remoteTopologyFixture(t)
	address, err := operatorAgentEndpoint(view, adjacent, target, time.Now())
	if err != nil || address != net.JoinHostPort(target.MeshAddress().String(), "50052") {
		t.Fatalf("endpoint %q %v", address, err)
	}
	view.Announcements[0].AgentPort = 50053
	view.Announcements[0].Revision++
	address, err = operatorAgentEndpoint(view, adjacent, target, time.Now())
	if err != nil || address != net.JoinHostPort(target.MeshAddress().String(), "50053") {
		t.Fatalf("moved endpoint %q %v", address, err)
	}
	view.Routes = nil
	if _, err = operatorAgentEndpoint(view, adjacent, target, time.Now()); status.Code(err) != codes.Unavailable {
		t.Fatalf("powered-off route: %v", err)
	}
	view, adjacent, target = remoteTopologyFixture(t)
	view.Announcements[0].Platform = "Lite"
	if _, err = operatorAgentEndpoint(view, adjacent, target, time.Now()); status.Code(err) != codes.Unimplemented {
		t.Fatalf("Lite needs WendyCom adapter: %v", err)
	}
}

// Exercise real gRPC client lifetimes; TLS/routing are independently exercised
// by the two-kernel-router positive fixture. Active cache borrowers are never
// displaced, and a released slot permits another ordinary target immediately.
func TestOperatorRemoteNormalClientCacheBoundAndRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	view, adjacent, target := remoteTopologyFixture(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("spiffe://wendy.sh/tenant/" + target.Tenant().String() + "/operator/cache")
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	credential := config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	base, stop := startUpstream(t)
	defer stop()
	r := newOperatorTargets(nil, base, credential, t.TempDir(), Spec{Host: MeshSelector(adjacent), Operator: &OperatorSpec{RouterUUID: adjacent.UUID().String()}})
	defer r.Close()
	r.dial = func(ctx context.Context, _ string, _ *config.CertificateInfo, _ meshid.Device) (*grpcclient.AgentConnection, error) {
		conn, err := grpc.NewClient(base.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return grpcclient.NewFromConn(conn), nil
	}
	current := target
	r.targetTopology = nil // This fixture supplies coherent observations in-process.
	r.topology = func(context.Context) (*pb.LocalMeshTopology, error) {
		view := proto.Clone(view).(*pb.LocalMeshTopology)
		prefix := netip.PrefixFrom(current.MeshAddress(), 128).String()
		view.Announcements[0].Principal = current.Principal()
		view.Announcements[0].DeviceId = current.UUID().String()
		view.Announcements[0].Prefix = prefix
		view.Routes[0].Prefix = prefix
		view.Routes[0].OriginPrincipal = current.Principal()
		return view, nil
	}
	resolve := func() (*grpc.ClientConn, func(), error) {
		return r.Resolve(metadata.NewIncomingContext(ctx, metadata.Pairs(operatorTargetHeader, MeshSelector(current))))
	}
	var releases []func()
	var first *grpc.ClientConn
	for i := 0; i < operatorRemoteClients; i++ {
		current = meshid.NewDevice(target.Tenant(), uuid.New())
		conn, release, err := resolve()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = conn
		}
		releases = append(releases, release)
	}
	current = meshid.NewDevice(target.Tenant(), uuid.New())
	if _, _, err := resolve(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("busy retained clients: %v", err)
	}
	releases[0]()
	releases = releases[1:]
	conn, release, err := resolve()
	if err != nil {
		t.Fatal(err)
	}
	if first.GetState() != connectivity.Shutdown {
		t.Fatal("evicted idle client retained transport")
	}
	reused, again, err := resolve()
	if err != nil || reused != conn {
		t.Fatalf("warm remote client was not reused: %v", err)
	}
	again()
	var borrowers sync.WaitGroup
	for i := 0; i < 8; i++ {
		borrowers.Add(1)
		go func() {
			defer borrowers.Done()
			got, release, err := resolve()
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if got != conn {
				t.Error("concurrent caller received a new client")
			}
		}()
	}
	borrowers.Wait()
	release()
	for _, release := range releases {
		release()
	}
	r.Close()
	if conn.GetState() != connectivity.Shutdown {
		t.Fatal("attachment shutdown retained remote connection")
	}
}
