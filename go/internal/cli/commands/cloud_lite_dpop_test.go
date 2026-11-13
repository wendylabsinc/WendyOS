package commands

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

type liteDPoPRequest struct {
	metadata   metadata.MD
	serverName string
}

type liteDPoPRelay struct {
	wcomrelaypb.UnimplementedWendyComRelayServiceServer
	received chan liteDPoPRequest
}

func (r *liteDPoPRelay) WendyComRelay(stream wcomrelaypb.WendyComRelayService_WendyComRelayServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	request := liteDPoPRequest{metadata: md}
	if remote, ok := peer.FromContext(stream.Context()); ok {
		if info, ok := remote.AuthInfo.(credentials.TLSInfo); ok {
			request.serverName = info.State.ServerName
		}
	}
	r.received <- request
	return stream.Send(&wcomrelaypb.WendyComRelayMessage{})
}

func TestLiteCloudBrokerDPoPMatchesTLSAuthority(t *testing.T) {
	_, key := newP256KeyPEM(t)
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)

	// localhost can resolve to both loopback families. Serve both so a failed
	// IPv6 connection cannot hide the TLS rejection from the IPv4 connection.
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	relay := &liteDPoPRelay{received: make(chan liteDPoPRequest, 1)}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})))
	wcomrelaypb.RegisterWendyComRelayServiceServer(server, relay)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	auth := dpopBoundSession(t)
	for _, tc := range []struct {
		name    string
		host    string
		roots   *x509.CertPool
		wantErr string
	}{
		{name: "matching authority and SNI", host: "localhost", roots: roots},
		{name: "wrong broker hostname", host: "127.0.0.1", roots: roots, wantErr: "IP SANs"},
		{name: "untrusted broker certificate", host: "localhost", roots: x509.NewCertPool(), wantErr: "unknown authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			authority := net.JoinHostPort(tc.host, port)
			conn, callCtx, err := dialLiteCloudBroker(ctx, auth, "https://"+authority, &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    tc.roots,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			stream, err := wcomrelaypb.NewWendyComRelayServiceClient(conn).WendyComRelay(callCtx)
			if err == nil {
				_, err = stream.Recv()
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected TLS rejection containing %q, got %v", tc.wantErr, err)
				}
				select {
				case <-relay.received:
					t.Fatal("sent a relay request to an unverified broker")
				default:
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			request := <-relay.received
			if request.serverName != tc.host {
				t.Errorf("TLS SNI = %q, want %q", request.serverName, tc.host)
			}
			gotAuthority := request.metadata.Get(":authority")
			if len(gotAuthority) != 1 || gotAuthority[0] != authority {
				t.Fatalf("request authority = %q, want %q", gotAuthority, authority)
			}
			if got := request.metadata.Get("authorization"); len(got) != 1 || got[0] != "DPoP "+auth.APIKey {
				t.Fatalf("authorization = %q, want the DPoP session token", got)
			}
			proofs := request.metadata.Get("dpop")
			if len(proofs) != 1 {
				t.Fatalf("received %d DPoP proofs", len(proofs))
			}
			var proof struct {
				Method string `json:"htm"`
				URI    string `json:"htu"`
			}
			decodeProofPart(t, proofs[0], 1, &proof)
			wantURI := "https://" + gotAuthority[0] + wcomrelaypb.WendyComRelayService_WendyComRelay_FullMethodName
			if proof.Method != "POST" || proof.URI != wantURI {
				t.Fatalf("DPoP target = %s %s, want POST %s", proof.Method, proof.URI, wantURI)
			}
		})
	}
}
