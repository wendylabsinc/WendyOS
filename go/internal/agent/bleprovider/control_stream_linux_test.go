//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// Exercise the production carrier TLS configuration through the routing hello.
// TLS alone can succeed even when the carrier negotiates an obsolete protocol.
func TestBLEProviderEstablishesControlStream(t *testing.T) {
	_, high, low := resumptionCredentials(t)
	clientRuntime := &runtime{cfg: Config{Credentials: high}}
	clientConfig, authenticated, err := clientRuntime.clientTLS(candidate{hint: deviceHint(low.Device.UUID())})
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := (&runtime{cfg: Config{Credentials: low}}).makeServerTLS()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	left, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	left.SetDeadline(time.Now().Add(3 * time.Second))
	right.SetDeadline(time.Now().Add(3 * time.Second))
	client, server := tls.Client(left, clientConfig), tls.Server(right, serverConfig)
	done := make(chan error, 1)
	go func() {
		err := server.HandshakeContext(ctx)
		if err == nil {
			err = localmesh.OpenStreamHello(ctx, server, low.Device, high.Device)
		}
		done <- err
	}()
	err = client.HandshakeContext(ctx)
	if err == nil {
		err = localmesh.OpenStreamHello(ctx, client, high.Device, low.Device)
	}
	serverErr := <-done
	if err != nil || serverErr != nil {
		t.Fatalf("authenticated BLE control stream: client=%v server=%v negotiated=%q required=%q", err, serverErr, client.ConnectionState().NegotiatedProtocol, localmesh.StreamALPN)
	}
	if authenticated == nil || *authenticated != low.Device {
		t.Fatal("control stream did not retain the authenticated peer")
	}
}
