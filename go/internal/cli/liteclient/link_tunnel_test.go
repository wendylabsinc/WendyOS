package liteclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// relayOpen is what a fakeRelay saw when a stream opened.
type relayOpen struct {
	assetID       string
	authorization []string
}

// fakeRelay transports opaque bytes to a device TLS server.
type fakeRelay struct {
	wcomrelaypb.UnimplementedWendyComRelayServiceServer
	refuse error
	opens  chan relayOpen
	ended  chan struct{}
	device func(net.Conn)
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{opens: make(chan relayOpen, 1), ended: make(chan struct{}, 1)}
}
func (r *fakeRelay) WendyComRelay(stream wcomrelaypb.WendyComRelayService_WendyComRelayServer) error {
	defer func() { r.ended <- struct{}{} }()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	r.opens <- relayOpen{assetID: first.GetOpen().GetAssetId(), authorization: md.Get("authorization")}
	if r.refuse != nil {
		return r.refuse
	}
	proxy, device := net.Pipe()
	defer proxy.Close()
	defer device.Close()
	go r.device(device)
	done := make(chan error, 2)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if _, err = proxy.Write(msg.GetPayload().GetBytes()); err != nil {
				done <- err
				return
			}
		}
	}()
	go func() {
		buffer := make([]byte, 137) // Deliberately split TLS records across relay payloads.
		for {
			n, err := proxy.Read(buffer)
			if err != nil {
				done <- err
				return
			}
			err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: buffer[:n]}}})
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

// dialFakeRelay serves relay in-process and returns a connection to it.
func dialFakeRelay(t *testing.T, relay *fakeRelay) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	wcomrelaypb.RegisterWendyComRelayServiceServer(srv, relay)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///relay",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return cc
}

const relayTestAssetID = "0b6f7a52-5d1e-4c3b-9a8e-2f4d6c8b1a3e"

func relayPKI(t *testing.T, principal string, trusted bool) (tls.Certificate, string, *tls.Config) {
	t.Helper()
	root, key := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	intermediate, ik := issueRelayCertificate(t, 2, "operator CA", "", true, root, key)
	operator, ok := issueRelayCertificate(t, 3, "operator", "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice", false, intermediate, ik)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intermediate.Raw})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	if !trusted {
		root, key = issueRelayCertificate(t, 4, "untrusted", "", true, nil, nil)
	}
	device, dk := issueRelayCertificate(t, 5, "device", principal, false, root, key)
	return tls.Certificate{Certificate: [][]byte{operator.Raw}, PrivateKey: ok}, chain, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{device.Raw}, PrivateKey: dk}}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
}

const devicePrincipal = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/lite-441bf6804ff8"

func TestConnectViaRelayMTLS(t *testing.T) {
	for _, tc := range []struct {
		name, principal string
		trusted, accept bool
	}{
		{"matching device", devicePrincipal, true, true},
		{"wrong device", strings.Replace(devicePrincipal, "441bf6804ff8", "000000000000", 1), true, false},
		{"wrong tenant", strings.Replace(devicePrincipal, "11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333", 1), true, false},
		{"untrusted issuer", devicePrincipal, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, chain, cfg := relayPKI(t, tc.principal, tc.trusted)
			relay := newFakeRelay()
			verified := make(chan error, 1)
			relay.device = func(raw net.Conn) {
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(5 * time.Second))
				conn := tls.Server(raw, cfg)
				err := conn.Handshake()
				verified <- err
				if err != nil {
					return
				}
				link := newDirectLink(conn)
				msg, err := link.recv(5 * time.Second)
				if err != nil {
					return
				}
				if msg.GetHandshake() == nil {
					return
				}
				if err = link.send(msg); err != nil {
					return
				}
				io.Copy(io.Discard, conn)
			}
			cc := dialFakeRelay(t, relay)
			ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer session"), 10*time.Second)
			defer cancel()
			c := NewWendyLiteClient()
			err := c.ConnectViaRelay(ctx, cc, relayTestAssetID, cert, chain, "441bf6804ff8")
			if tc.accept {
				if err != nil {
					t.Fatal(err)
				}
				if err = <-verified; err != nil {
					t.Fatalf("operator mTLS: %v", err)
				}
				if c.PeerCertificate() == nil {
					t.Fatal("missing authenticated device certificate")
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "device mTLS") {
				t.Fatalf("untrusted device accepted: %v", err)
			}
			open := <-relay.opens
			if open.assetID != relayTestAssetID || len(open.authorization) != 1 || open.authorization[0] != "Bearer session" {
				t.Fatalf("lost caller context: %+v", open)
			}
			select {
			case <-relay.ended:
			case <-time.After(time.Second):
				t.Fatal("stream leaked")
			}
			if cc.GetState() != connectivity.Shutdown {
				t.Fatal("connection leaked")
			}
		})
	}
}

func TestConnectViaRelayReportsRefusalAndClosesConn(t *testing.T) {
	cert, chain, _ := relayPKI(t, devicePrincipal, true)
	relay := newFakeRelay()
	relay.refuse = status.Error(codes.PermissionDenied, "permission denied")
	cc := dialFakeRelay(t, relay)
	err := NewWendyLiteClient().ConnectViaRelay(context.Background(), cc, relayTestAssetID, cert, chain, "441bf6804ff8")
	if err == nil || !strings.Contains(err.Error(), "PermissionDenied") {
		t.Fatalf("lost broker refusal: %v", err)
	}
	if cc.GetState() != connectivity.Shutdown {
		t.Fatal("connection leaked")
	}
}

func TestRelayCancelWhileUnread(t *testing.T) {
	relay := newFakeRelay()
	relay.device = func(conn net.Conn) { defer conn.Close(); conn.Write(make([]byte, 65536)) }
	cc := dialFakeRelay(t, relay)
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := openRelayConn(ctx, cc, relayTestAssetID)
	if err != nil {
		t.Fatal(err)
	}
	<-relay.opens
	cancel()
	select {
	case <-relay.ended:
	case <-time.After(time.Second):
		t.Fatal("cancel did not release blocked relay")
	}
	conn.Close()
	if cc.GetState() != connectivity.Shutdown {
		t.Fatal("connection leaked")
	}
}

func TestRelayReadDeadline(t *testing.T) {
	relay := newFakeRelay()
	relay.device = func(conn net.Conn) { defer conn.Close(); io.Copy(io.Discard, conn) }
	cc := dialFakeRelay(t, relay)
	conn, err := openRelayConn(context.Background(), cc, relayTestAssetID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err = conn.Read(make([]byte, 1))
	if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatalf("deadline ignored: %v", err)
	}
}
