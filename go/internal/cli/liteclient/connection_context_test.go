package liteclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func TestBLEDialCancellationClosesLateConnection(t *testing.T) {
	previous := openBLEStreamFn
	t.Cleanup(func() { openBLEStreamFn = previous })
	started, release := make(chan struct{}), make(chan struct{})
	local, remote := net.Pipe()
	defer remote.Close()
	openBLEStreamFn = func(context.Context, string, uint16) (net.Conn, error) {
		close(started)
		<-release
		return local, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := dialBLEStream(ctx, "board", DefaultL2CAPPSM)
		completed <- err
	}()
	<-started
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("BLE setup ignored cancellation")
	}
	close(release)
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Fatal("late connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("late connection was not closed")
	}
}

func TestBLEHandshakeCancellation(t *testing.T) {
	previous := openBLEStreamFn
	t.Cleanup(func() { openBLEStreamFn = previous })
	local, remote := net.Pipe()
	defer remote.Close()
	openBLEStreamFn = func(context.Context, string, uint16) (net.Conn, error) {
		return local, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		completed <- NewWendyLiteClient().ConnectViaBLEContext(ctx, "board", DefaultL2CAPPSM, nil)
	}()
	// Receive the ClientHello, then leave the TLS server silent.
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("cancelled BLE handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("BLE TLS handshake ignored cancellation")
	}
}

func TestPKILANHandshakeCancellation(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	root, rootKey := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	operator, operatorKey := issueRelayCertificate(t, 2, "operator", "spiffe://wendy.sh/tenant/"+tenant+"/operator/22222222-2222-4222-8222-222222222222", false, root, rootKey)
	cert := tls.Certificate{Certificate: [][]byte{operator.Raw}, PrivateKey: operatorKey}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		completed <- NewWendyLiteClient().ConnectWithPKIAuthenticationContext(ctx, listener.Addr().String(), cert, chain, liteTestDevice, nil)
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handshake error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("LAN TLS handshake ignored cancellation")
	}
}

func TestSensorTransportSetupContextCancellation(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	base := "spiffe://wendy.sh/tenant/" + tenant + "/device/"
	root, rootKey := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	consumer, consumerKey := issueRelayCertificate(t, 2, "consumer", base+"consumer", false, root, rootKey)
	source, sourceKey := issueRelayCertificate(t, 3, "source", base+"source", false, root, rootKey)
	cert := tls.Certificate{Certificate: [][]byte{consumer.Raw}, PrivateKey: consumerKey}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	roots := x509.NewCertPool()
	roots.AddCert(root)
	for _, transport := range []struct {
		name    string
		connect func(*WendyLiteClient, context.Context, string) error
	}{
		{"principal", func(c *WendyLiteClient, ctx context.Context, address string) error {
			return c.ConnectSensorSource(ctx, address, cert, chain, base+"source")
		}},
		{"legacy", func(c *WendyLiteClient, ctx context.Context, address string) error {
			return c.ConnectWithMutualAuthenticationContext(ctx, address, cert, *roots)
		}},
	} {
		for _, phase := range []string{"TLS", "WendyCom", "connected"} {
			t.Run(transport.name+"/"+phase, func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				completed := make(chan error, 1)
				client := NewWendyLiteClient()
				defer client.Close()
				go func() { completed <- transport.connect(client, ctx, listener.Addr().String()) }()
				raw, err := listener.Accept()
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(time.Second))
				var conn net.Conn = raw
				if phase != "TLS" {
					tlsConn := tls.Server(raw, &tls.Config{
						MinVersion:   tls.VersionTLS13,
						Certificates: []tls.Certificate{{Certificate: [][]byte{source.Raw}, PrivateKey: sourceKey}},
						ClientCAs:    roots,
						ClientAuth:   tls.RequireAndVerifyClientCert,
					})
					if err := tlsConn.Handshake(); err != nil {
						t.Fatal(err)
					}
					conn = tlsConn
				}
				if phase == "connected" {
					assertConnectedSensorSurvivesSetupCancellation(t, conn, client, completed, cancel)
					return
				}
				// Read the ClientHello or WendyCom handshake, then leave it unanswered.
				if _, err := conn.Read(make([]byte, 4096)); err != nil {
					t.Fatal(err)
				}
				cancel()
				select {
				case err := <-completed:
					if err == nil {
						t.Fatal("cancelled handshake succeeded")
					}
				case <-time.After(time.Second):
					t.Fatal("connection setup outlived cancellation")
				}
			})
		}
	}
}

func assertConnectedSensorSurvivesSetupCancellation(t *testing.T, conn net.Conn, client *WendyLiteClient, completed chan error, cancel context.CancelFunc) {
	t.Helper()
	server := newDirectLink(conn)
	handshake, err := server.recv(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.send(handshake); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("connection setup did not finish")
	}
	cancel()
	go func() { completed <- client.Ping() }()
	request, err := server.recv(time.Second)
	if err != nil {
		t.Fatalf("connection closed after setup cancellation: %v", err)
	}
	if err := server.send(&wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Response{Response: &wendypb.WendyComResponse{
		RequestId: request.GetCommand().GetRequestId(), Result: wendypb.WendyComResult_WENDY_COM_RESULT_OK,
	}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("Ping after setup cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Ping did not finish after setup cancellation")
	}
}
