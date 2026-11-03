package services

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type legacyDeliveryRoutes struct {
	lan, relay string
	fallback   atomic.Int32
}

func (p *legacyDeliveryRoutes) DialTarget(ctx context.Context, _ DeliveryTarget, _ uint16) (net.Conn, string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.lan)
	return conn, "lan-direct", err
}
func (p *legacyDeliveryRoutes) DialRoutedTarget(ctx context.Context, _ DeliveryTarget, _ uint16) (net.Conn, string, error) {
	p.fallback.Add(1)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.relay)
	return conn, "relay", err
}

func TestLegacyBuildDeliveryPinsNumericIdentityAcrossLANFallback(t *testing.T) {
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
	client, err := mtls.NewClientTLSConfigExpectingIdentity(self, chain, key, zap.NewNop(), certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityAsset, EntityID: "675"})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(asset int32) (string, *deliveryTLSProbe) {
		leaf, key := mint(asset)
		cfg, err := mtls.NewTLSConfig(leaf, chain, key, zap.NewNop(), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
		probe := new(deliveryTLSProbe)
		agentpb.RegisterWendyContainerServiceServer(server, probe)
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(server.Stop)
		return listener.Addr().String(), probe
	}
	lan, impostor := serve(676)
	relay, expected := serve(675)
	peers := &legacyDeliveryRoutes{lan: lan, relay: relay}
	target := DeliveryTarget{LegacyOrgID: 64, LegacyAssetID: 675}
	dial := deliveryTargetDialer(peers, func(*pb.PushTarget) (DeliveryTarget, error) { return target, nil }, func(got DeliveryTarget) (*tls.Config, error) {
		if got != target {
			t.Error("pin target changed")
		}
		return client, nil
	}, 50052)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, &pb.PushTarget{AssetId: 675, LegacyOrganizationId: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := agentpb.NewWendyContainerServiceClient(conn).QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
		t.Fatal(err)
	}
	if peers.fallback.Load() != 1 || expected.calls.Load() != 1 || impostor.calls.Load() != 0 {
		t.Fatalf("fallback=%d expected=%d impostor=%d", peers.fallback.Load(), expected.calls.Load(), impostor.calls.Load())
	}
}
