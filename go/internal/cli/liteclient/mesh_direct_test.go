package liteclient

import (
	"context"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type nativeDialFunc func(context.Context, meshid.Device, byte) (net.Conn, *x509.Certificate, error)

func (f nativeDialFunc) DialNative(ctx context.Context, peer meshid.Device, service byte) (net.Conn, *x509.Certificate, error) {
	return f(ctx, peer, service)
}
func nativeTestPeer() meshid.Device {
	tenant, _ := meshid.ParseTenant("11111111-1111-1111-1111-111111111111")
	return meshid.NewDevice(tenant, uuid.MustParse("22222222-2222-2222-2222-222222222222"))
}

func TestDirectNativeWendyComWithoutTLSAndSetupCancellation(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel-handshake", true: "connected"}[connected], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a, b := net.Pipe()
			defer b.Close()
			client := NewWendyLiteClient()
			defer client.Close()
			leaf := &x509.Certificate{Raw: []byte("retained-certificate")}
			dial := nativeDialFunc(func(_ context.Context, peer meshid.Device, service byte) (net.Conn, *x509.Certificate, error) {
				if peer != nativeTestPeer() || service != directstream.ServiceWendyCom {
					t.Error("wrong native request")
				}
				return a, leaf, nil
			})
			completed := make(chan error, 1)
			go func() { completed <- client.ConnectDirectContext(ctx, dial, nativeTestPeer(), nil) }()
			if connected {
				// This helper decodes a WendyCom handshake as the first bytes, then
				// verifies Ping survives cancellation of the completed setup context.
				assertConnectedSensorSurvivesSetupCancellation(t, b, client, completed, cancel)
				if client.PeerCertificate() != leaf {
					t.Fatal("lost retained peer certificate")
				}
				return
			}
			server := newDirectLink(b)
			if _, err := server.recv(time.Second); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case err := <-completed:
				if err == nil {
					t.Fatal("cancelled handshake succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not stop handshake")
			}
		})
	}
}
