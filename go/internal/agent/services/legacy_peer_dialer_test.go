package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type v1ClientTunnelContract struct {
	cloudpb.UnimplementedTunnelBrokerServiceServer
	result chan error
}

func (s v1ClientTunnelContract) ClientTunnel(stream cloudpb.TunnelBrokerService_ClientTunnelServer) error {
	open, err := stream.Recv()
	if err != nil {
		return err
	}
	target := open.GetOpen()
	if target.GetAssetId() != 675 || target.GetHost() != "localhost" || target.GetPort() != 50052 {
		return fmt.Errorf("wrong legacy destination: %v", target)
	}
	data, err := stream.Recv()
	if err != nil {
		return err
	}
	err = stream.Send(data.GetData())
	s.result <- err
	return err
}
func TestLegacyPeerUsesV1ClientTunnel(t *testing.T) {
	ca, key, chain := brokerTestCA(t)
	serverCert := brokerTestServer(t, ca, key, "broker", nil, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if info.FullMethod != "/wendycloud.v1.TunnelBrokerService/ClientTunnel" {
			return fmt.Errorf("wrong protocol")
		}
		md, _ := metadata.FromIncomingContext(stream.Context())
		if len(md.Get("x-wendy-certificate-signature")) != 1 {
			return fmt.Errorf("missing proof")
		}
		if got := md.Get("x-wendy-certificate-uri"); len(got) != 1 || got[0] != "urn:wendy:org:64:asset:674" {
			return fmt.Errorf("wrong identity")
		}
		return next(srv, stream)
	}))
	contract := v1ClientTunnelContract{result: make(chan error, 1)}
	cloudpb.RegisterTunnelBrokerServiceServer(server, contract)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	t.Setenv("WENDY_BROKER_URL", listener.Addr().String())
	leaf, private := legacyBrokerTestLeaf(t, true)
	provisioning := &ProvisioningService{enrolled: true, cloudHost: listener.Addr().String(), orgID: 64, assetID: 674, certPEM: leaf, keyPEM: []byte(private), chainPEM: chain}
	dialer := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for _, path := range []string{"admin", "app-fallback"} {
		t.Run(path, func(t *testing.T) {
			dial := func() (net.Conn, string, error) {
				return dialer.DialRoutedTarget(ctx, DeliveryTarget{LegacyOrgID: 64, LegacyAssetID: 675}, 50052)
			}
			if path == "app-fallback" {
				dialer.browse = func(context.Context) ([]models.LANDevice, error) { return nil, nil }
				dial = func() (net.Conn, string, error) { return dialer.DialLegacyApp(ctx, 64, 675, 50052) }
			}
			conn, mode, err := dial()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if mode != "relay" {
				t.Fatalf("mode=%s", mode)
			}
			if _, err := conn.Write([]byte("legacy peer")); err != nil {
				t.Fatal(err)
			}
			data := make([]byte, len("legacy peer"))
			if _, err := io.ReadFull(conn, data); err != nil {
				t.Fatal(err)
			}
			if string(data) != "legacy peer" {
				t.Fatalf("echo=%q", data)
			}
			select {
			case err := <-contract.result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("no v1 relay")
			}
		})
	}

}
func TestLegacyPeersRefuseCrossProtocolAndCrossOrganization(t *testing.T) {
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "legacy:443", orgID: 64, assetID: 674}
	dialer := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	for _, target := range []DeliveryTarget{{LegacyOrgID: 65, LegacyAssetID: 674}, {LegacyOrgID: 64, LegacyAssetID: -1}, {LegacyOrgID: 64, LegacyAssetID: 675, Device: testMeshDevice}, {LegacyOrgID: 64, LegacyAssetID: 675, AssetUUID: testAssetUUID}} {
		if _, _, err := dialer.DialTarget(context.Background(), target, 50052); err == nil {
			t.Fatalf("accepted %+v", target)
		}
	}
	provisioning.principalURI = testMeshDevice.Principal()
	if _, err := dialer.Resolve(64, 675); err == nil {
		t.Fatal("PKI device selected legacy transport")
	}
	// Numeric targets do not pass through the UUID resolver or UUID TLS hook.
	service := NewBuildService(zap.NewNop(), BuildServiceOptions{ResolveLegacyTarget: dialer.Resolve, ResolveTarget: func(string) (DeliveryTarget, error) {
		t.Fatal("legacy ID reached UUID resolver")
		return DeliveryTarget{}, nil
	}, PushTLS: func(meshid.Device) (*tls.Config, error) { t.Fatal("legacy ID reached UUID pin"); return nil, nil }})
	if _, err := service.resolvePushTarget(&pb.PushTarget{AssetId: 675, AssetUuid: testAssetUUID}); err == nil {
		t.Fatal("accepted mixed build target")
	}
}

func TestLegacyBuildTargetOrganizationComesFromEnrollment(t *testing.T) {
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "legacy:443", orgID: 64, assetID: 674}
	peers := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	service := NewBuildService(zap.NewNop(), BuildServiceOptions{ResolveLegacyTarget: peers.Resolve})
	target := &pb.PushTarget{AssetId: 675}
	resolved, err := service.resolvePushTarget(target)
	if err != nil || resolved.LegacyOrgID != 64 || resolved.LegacyAssetID != 675 {
		t.Fatalf("numeric target lost enrollment binding: %+v %v", resolved, err)
	}
	provisioning.principalURI = testMeshDevice.Principal()
	if _, err := service.resolvePushTarget(target); err == nil {
		t.Fatal("PKI build host accepted a numeric target")
	}
}
