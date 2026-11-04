package services

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type legacyAppContract struct {
	pb.UnimplementedWendyMeshServiceServer
}

func (legacyAppContract) MeshDial(stream pb.WendyMeshService_MeshDialServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetOpen().GetPort() != 28091 {
		return fmt.Errorf("wrong app port")
	}
	// Historical agents send no ready frame and wait for application input.
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(msg.GetData()); err != nil {
			return err
		}
	}
}
func TestLegacyAppLANPinsNumericPeerAndNeedsNoReadyFrame(t *testing.T) {
	ca, caKey, chain := brokerTestCA(t)
	mint := func(asset int32) (string, string) {
		leaf, key := legacyBrokerTestLeaf(t, false)
		parsed, err := storedDeviceLeaf(leaf)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(certs.AssetURN(64, asset))
		parsed.URIs = []*url.URL{uri}
		parsed.Subject.CommonName = "sh/wendy/64/" + strconv.Itoa(int(asset))
		der, err := x509.CreateCertificate(rand.Reader, parsed, ca, parsed.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), key
	}
	self, key := mint(674)
	remote, remoteKey := mint(675)
	cfg, err := mtls.NewTLSConfig(remote, chain, remoteKey, zap.NewNop(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	pb.RegisterWendyMeshServiceServer(server, legacyAppContract{})
	go server.Serve(listener)
	defer server.Stop()
	peers := NewLegacyPeerDialer(zap.NewNop(), &ProvisioningService{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	conn, err := peers.dialLegacyLAN(ctx, listener.Addr().String(), self, chain, key, 64, 675, 28091)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	defer conn.Close()
	if _, err := conn.Write([]byte("app echo")); err != nil {
		t.Fatal(err)
	}
	conn.(interface{ CloseWrite() error }).CloseWrite()
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "app echo" {
		t.Fatalf("half-close response=%q %v", got, err)
	}
	wrongCtx, wrongCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer wrongCancel()
	bad, err := peers.dialLegacyLAN(wrongCtx, listener.Addr().String(), self, chain, key, 64, 676, 28091)
	if err == nil {
		bad.Close()
		t.Fatal("accepted another numeric asset")
	}
}

type legacyRosterContract struct {
	cloudpb.UnimplementedMeshRosterServiceServer
}

func (legacyRosterContract) GetMeshRoster(ctx context.Context, _ *cloudpb.GetMeshRosterRequest) (*cloudpb.GetMeshRosterResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("x-wendy-certificate-uri"); len(got) != 1 || got[0] != certs.AssetURN(64, 674) {
		return nil, fmt.Errorf("wrong roster identity")
	}
	if len(md.Get("x-wendy-certificate-signature")) != 1 {
		return nil, fmt.Errorf("missing signed proof")
	}
	return &cloudpb.GetMeshRosterResponse{OrgSlug: "Lab", Entries: []*cloudpb.MeshRosterEntry{{AssetId: 675, Name: "Pi"}, {AssetId: 676, Name: "Duplicate"}, {AssetId: 677, Name: "duplicate"}}}, nil
}
func TestLegacyRosterProofLANScopeAndEnrollmentIsolation(t *testing.T) {
	ca, key, chain := brokerTestCA(t)
	serverCert := brokerTestServer(t, ca, key, "broker", nil, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})))
	cloudpb.RegisterMeshRosterServiceServer(server, legacyRosterContract{})
	go server.Serve(listener)
	defer server.Stop()
	leaf, private := legacyBrokerTestLeaf(t, true)
	provisioning := &ProvisioningService{enrolled: true, cloudHost: listener.Addr().String(), orgID: 64, assetID: 674, certPEM: leaf, keyPEM: []byte(private), chainPEM: chain}
	peers := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	peers.browse = func(context.Context) ([]models.LANDevice, error) {
		return []models.LANDevice{{OrgID: 65, AssetID: 900, MeshName: "Pi"}, {OrgID: 64, AssetID: 678, MeshName: "Pi"}}, nil
	}
	roster := NewLegacyMeshResolver(peers)
	if err := roster.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := roster.ResolveLegacy("pi", "lab"); !ok || got != 678 {
		t.Fatalf("LAN first=%d %v", got, ok)
	}
	if _, ok := roster.ResolveLegacy("pi", "other"); ok {
		t.Fatal("foreign slug resolved")
	}
	if _, ok := roster.ResolveLegacy("duplicate", "lab"); ok {
		t.Fatal("ambiguous roster resolved")
	}
	peers.browse = func(context.Context) ([]models.LANDevice, error) {
		return []models.LANDevice{{OrgID: 64, AssetID: 678, MeshName: "Pi"}, {OrgID: 64, AssetID: 679, MeshName: "Pi"}}, nil
	}
	if _, ok := roster.ResolveLegacy("pi", "lab"); ok {
		t.Fatal("ambiguous LAN resolved through roster")
	}
	peers.browse = func(context.Context) ([]models.LANDevice, error) { return nil, nil }
	if got, ok := roster.ResolveLegacy("pi", "lab"); !ok || got != 675 {
		t.Fatalf("roster fallback=%d %v", got, ok)
	}
	old, _ := provisioning.CloudEnrollment()
	provisioning.mu.Lock()
	provisioning.orgID = 65
	provisioning.mu.Unlock()
	roster.apply(old, &cloudpb.GetMeshRosterResponse{OrgSlug: "lab", Entries: []*cloudpb.MeshRosterEntry{{Name: "Pi", AssetId: 800}}})
	if _, ok := roster.ResolveLegacy("pi", "lab"); ok {
		t.Fatal("old enrollment roster survived")
	}
	provisioning.mu.Lock()
	provisioning.principalURI = testMeshDevice.Principal()
	provisioning.mu.Unlock()
	if roster.LegacyOrgID() != 0 {
		t.Fatal("PKI enrollment enabled numeric DNS")
	}
}
func TestLegacyAppDiscoveryCachesAndRejectsLoopbackOrForeignOrg(t *testing.T) {
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "cloud:443", orgID: 64, assetID: 674}
	peers := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	calls := 0
	peers.browse = func(context.Context) ([]models.LANDevice, error) {
		calls++
		return []models.LANDevice{{OrgID: 65, AssetID: 675, IsMTLS: true, IPAddress: "192.0.2.1", Port: 50052}, {OrgID: 64, AssetID: 675, IsMTLS: true, IPAddress: "::1", Port: 50052}, {OrgID: 64, AssetID: 675, IsMTLS: true, IPAddress: "fe80::1%eth0", Port: 50052}}, nil
	}
	e, _ := provisioning.CloudEnrollment()
	for i := 0; i < 2; i++ {
		if got, ok := peers.legacyAppLANAddr(context.Background(), e, 675); !ok || got != "[fe80::1%eth0]:50052" {
			t.Fatalf("address=%q %v", got, ok)
		}
	}
	if calls != 1 {
		t.Fatalf("cache missed: %d", calls)
	}
	e.Host = "other-cloud:443"
	peers.legacyAppLANAddr(context.Background(), e, 675)
	if calls != 2 {
		t.Fatal("cache crossed cloud binding")
	}
}
