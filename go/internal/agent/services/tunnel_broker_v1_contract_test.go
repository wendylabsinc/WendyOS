package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// This server registers only the deployed v1 service. A client which merely
// starts successfully but speaks wendycloud.v2 cannot pass these tests.
type v1BrokerContract struct {
	cloudpb.UnimplementedTunnelBrokerServiceServer
	request *cloudpb.DialRequest
	check   func(cloudpb.TunnelBrokerService_AgentTunnelServer) error
	result  chan error
}

func (b *v1BrokerContract) RegisterPresence(s cloudpb.TunnelBrokerService_RegisterPresenceServer) error {
	if err := s.Send(b.request); err != nil {
		return err
	}
	<-s.Context().Done()
	return s.Context().Err()
}

func (b *v1BrokerContract) AgentTunnel(s cloudpb.TunnelBrokerService_AgentTunnelServer) error {
	join, err := s.Recv()
	if err == nil && join.GetSessionId() != "contract" {
		err = fmt.Errorf("session join=%q", join.GetSessionId())
	}
	if err == nil {
		err = b.check(s)
	}
	b.result <- err
	return err
}

func legacyBrokerTestLeaf(t *testing.T, dual bool) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("urn:wendy:org:64:asset:674")
	leaf := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "sh/wendy/64/674"}, URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	if dual {
		uri, _ := url.Parse("spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/service/asset-674")
		leaf.URIs = append(leaf.URIs, uri)
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func runV1BrokerContract(t *testing.T, b *v1BrokerContract, dual bool) {
	t.Helper()
	ca, key, roots := brokerTestCA(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCert := brokerTestServer(t, ca, key, "broker", nil, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if info.FullMethod != "/wendycloud.v1.TunnelBrokerService/RegisterPresence" && info.FullMethod != "/wendycloud.v1.TunnelBrokerService/AgentTunnel" {
			return fmt.Errorf("unexpected method %s", info.FullMethod)
		}
		md, _ := metadata.FromIncomingContext(stream.Context())
		if got := md.Get("x-wendy-certificate-uri"); len(got) != 1 || got[0] != "urn:wendy:org:64:asset:674" || len(md.Get("x-wendy-certificate-signature")) != 1 {
			return fmt.Errorf("missing numeric identity and signed proof on %s", info.FullMethod)
		}
		return next(srv, stream)
	}))
	cloudpb.RegisterTunnelBrokerServiceServer(server, b)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	leaf, private := legacyBrokerTestLeaf(t, dual)
	client := NewTunnelBrokerClient(zap.NewNop(), listener.Addr().String(), leaf, private, roots, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { client.Run(ctx); close(done) }()
	select {
	case err := <-b.result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("v1 broker did not receive an authenticated tunnel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broker client did not stop")
	}
}

func TestLegacyBrokerV1TCPAndTransitionalIdentity(t *testing.T) {
	for _, dual := range []bool{false, true} {
		t.Run(fmt.Sprint(dual), func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					_, _ = io.Copy(conn, conn)
				}
			}()
			b := &v1BrokerContract{request: &cloudpb.DialRequest{SessionId: "contract", Host: "127.0.0.1", Port: uint32(listener.Addr().(*net.TCPAddr).Port)}, result: make(chan error, 1)}
			b.check = func(s cloudpb.TunnelBrokerService_AgentTunnelServer) error {
				if err := s.Send(&cloudpb.TunnelData{Payload: []byte("v1 TCP")}); err != nil {
					return err
				}
				msg, err := s.Recv()
				if err == nil && string(msg.GetPayload()) != "v1 TCP" {
					return fmt.Errorf("TCP echo=%q", msg.GetPayload())
				}
				return err
			}
			runV1BrokerContract(t, b, dual)
		})
	}
}

func TestLegacyBrokerV1DatagramPing(t *testing.T) {
	b := &v1BrokerContract{request: &cloudpb.DialRequest{SessionId: "contract", Host: "localhost", Protocol: cloudpb.TunnelProtocol_TUNNEL_PROTOCOL_DATAGRAM}, result: make(chan error, 1)}
	b.check = func(s cloudpb.TunnelBrokerService_AgentTunnelServer) error {
		if err := s.Send(&cloudpb.TunnelData{IcmpRequest: &cloudpb.IcmpEchoRequest{Identifier: 7, Sequence: 42, Payload: []byte("v1 ping")}}); err != nil {
			return err
		}
		msg, err := s.Recv()
		if err == nil && (msg.GetIcmpReply().GetSequence() != 42 || string(msg.GetIcmpReply().GetPayload()) != "v1 ping") {
			return fmt.Errorf("incorrect datagram echo: %v", msg)
		}
		return err
	}
	runV1BrokerContract(t, b, true)
}

func TestLegacyBrokerV1UDP(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		data := make([]byte, 2048)
		n, peer, err := listener.ReadFrom(data)
		if err == nil {
			_, _ = listener.WriteTo(data[:n], peer)
		}
	}()
	port := uint32(listener.LocalAddr().(*net.UDPAddr).Port)
	b := &v1BrokerContract{request: &cloudpb.DialRequest{SessionId: "contract", Host: "localhost", Protocol: cloudpb.TunnelProtocol_TUNNEL_PROTOCOL_DATAGRAM}, result: make(chan error, 1)}
	b.check = func(stream cloudpb.TunnelBrokerService_AgentTunnelServer) error {
		if err := stream.Send(&cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{FlowId: 42, Port: port, Payload: []byte("v1 UDP")}}); err != nil {
			return err
		}
		reply, err := stream.Recv()
		if err != nil {
			return err
		}
		if datagram := reply.GetDatagram(); datagram.GetFlowId() != 42 || datagram.GetPort() != port || string(datagram.GetPayload()) != "v1 UDP" {
			return fmt.Errorf("UDP echo=%v", reply)
		}
		return nil
	}
	runV1BrokerContract(t, b, false)
}
