package localmesh

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestManifestHaveBatchCarrierNegotiation(t *testing.T) {
	for _, carrier := range []string{"quic", "stream"} {
		for _, offers := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
			t.Run(fmt.Sprint(carrier, offers), func(t *testing.T) {
				a, b := testCredentials(t)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				type result struct {
					enabled bool
					err     error
				}
				results := make(chan result, 1)
				release := make(chan struct{})
				defer close(release)
				serverTLS, err := b.PeerTLS(a.Device)
				if err != nil {
					t.Fatal(err)
				}
				clientTLS, err := a.PeerTLS(b.Device)
				if err != nil {
					t.Fatal(err)
				}
				var enabled bool
				if carrier == "quic" {
					listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, QUICConfig())
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					go func() {
						conn, err := listener.Accept(ctx)
						if err != nil {
							results <- result{err: err}
							return
						}
						defer conn.CloseWithError(0, "")
						_, _, batch, err := openRouterControlFeatures(ctx, conn, b.Device, a.Device, DatagramLimit, offers[1])
						results <- result{batch, err}
						select {
						case <-release:
						case <-ctx.Done():
						}
					}()
					conn, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, QUICConfig())
					if err != nil {
						t.Fatal(err)
					}
					defer conn.CloseWithError(0, "")
					_, _, enabled, err = openRouterControlFeatures(ctx, conn, a.Device, b.Device, WholeIPDatagramLimit, offers[0])
					if err != nil {
						t.Fatal(err)
					}
				} else {
					serverTLS.NextProtos = []string{StreamALPN}
					clientTLS.NextProtos = []string{StreamALPN}
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					go func() {
						raw, err := listener.Accept()
						if err != nil {
							results <- result{err: err}
							return
						}
						defer raw.Close()
						conn := tls.Server(raw, serverTLS)
						if err = conn.HandshakeContext(ctx); err != nil {
							results <- result{err: err}
							return
						}
						_, batch, err := openStreamHelloFeatures(ctx, conn, b.Device, a.Device, StreamALPN, offers[1])
						results <- result{batch, err}
						select {
						case <-release:
						case <-ctx.Done():
						}
					}()
					raw, err := net.Dial("tcp", listener.Addr().String())
					if err != nil {
						t.Fatal(err)
					}
					defer raw.Close()
					conn := tls.Client(raw, clientTLS)
					if err = conn.HandshakeContext(ctx); err != nil {
						t.Fatal(err)
					}
					_, enabled, err = openStreamHelloFeatures(ctx, conn, a.Device, b.Device, StreamALPN, offers[0])
					if err != nil {
						t.Fatal(err)
					}
				}
				other := <-results
				if want := offers[0] && offers[1]; other.err != nil || other.enabled != want || enabled != want {
					t.Fatalf("negotiation client=%t server=%t err=%v want=%t", enabled, other.enabled, other.err, want)
				}
			})
		}
	}
}
