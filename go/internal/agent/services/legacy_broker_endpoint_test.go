package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"testing"
	"time"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func TestLegacyRelayUsesConfiguredBrokerRatherThanAPI(t *testing.T) {
	ca, key, chain := brokerTestCA(t)
	serverCert := brokerTestServer(t, ca, key, "broker", nil, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	apiCalls := make(chan string, 1)
	serve := func(withBroker bool) string {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			method, _ := grpc.MethodFromServerStream(stream)
			select {
			case apiCalls <- method:
			default:
			}
			return status.Error(codes.Unimplemented, "API endpoint has no tunnel broker")
		}))
		if withBroker {
			cloudpb.RegisterTunnelBrokerServiceServer(server, v1ClientTunnelContract{result: make(chan error, 1)})
		}
		go server.Serve(ln)
		t.Cleanup(server.Stop)
		return ln.Addr().String()
	}
	apiMethod := func() string {
		select {
		case method := <-apiCalls:
			return method
		default:
			return "none"
		}
	}
	api, broker := serve(false), serve(true)
	t.Setenv("WENDY_BROKER_URL", broker)
	leaf, private := legacyBrokerTestLeaf(t, false)
	provisioning := &ProvisioningService{enrolled: true, cloudHost: api, orgID: 64, assetID: 674, certPEM: leaf, keyPEM: []byte(private), chainPEM: chain}
	peers := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := peers.DialRoutedTarget(ctx, DeliveryTarget{LegacyOrgID: 64, LegacyAssetID: 675}, 50052)
	if err != nil {
		t.Fatalf("wrong broker routing: API saw %s; configured broker=%s; err=%v", apiMethod(), broker, err)
	}
	defer conn.Close()
	_, err = conn.Write([]byte("legacy peer"))
	if err != nil {
		t.Fatalf("request went to API rather than WENDY_BROKER_URL: method=%s write=%v", apiMethod(), err)
	}
	payload := make([]byte, len("legacy peer"))
	_, err = io.ReadFull(conn, payload)
	if err != nil {
		t.Fatalf("ClientTunnel reached API %s instead of configured broker %s: method=%s err=%v", api, broker, apiMethod(), err)
	}
	if string(payload) != "legacy peer" {
		t.Fatalf("relay payload=%q", payload)
	}
	if method := apiMethod(); method != "none" {
		t.Fatalf("broker RPC reached API: %s", method)
	}

}
