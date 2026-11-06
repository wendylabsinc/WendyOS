package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestOwnVIPUsesLiveIngressAndRevokesOnStop(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "device-dialer", true: "local-mesh"}[local], func(t *testing.T) {
			d, probes := testDialer()
			registry := meshingress.NewRegistry()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			port := uint16(listener.Addr().(*net.TCPAddr).Port)
			d.SetSelfDialer(func(ctx context.Context, port uint16) (net.Conn, error) {
				return registry.DialAuthorized(port, func(address string) (net.Conn, error) {
					if address != net.JoinHostPort("10.3.0.2", fmt.Sprint(port)) {
						t.Fatalf("dial did not use admitted container endpoint: %s", address)
					}
					return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
				})
			})
			d.SetLocalMeshDialer(func(context.Context, meshid.Device, uint16) (net.Conn, error) {
				t.Fatal("own VIP attempted peer route")
				return nil, nil
			})
			own := mustDevice(t, rosterTenant, rosterDevice)
			certPEM, keyPEM := deviceIdentityCertPEM(t, rosterTenant, rosterDevice)
			d.UpdateIdentity(certPEM, keyPEM, "")
			dial := func(ctx context.Context) (net.Conn, string, error) {
				if local {
					return NewMeshAppDialer(d).DialDevice(ctx, own, port)
				}
				conn, mode, err, _ := d.dialSelf(ctx, own, port)
				return conn, mode, err
			}
			// A host listener alone cannot authorize an app port.
			if conn, _, err := dial(context.Background()); !errors.Is(err, meshingress.ErrPortDenied) {
				if conn != nil {
					conn.Close()
				}
				t.Fatalf("unpublished port: %v", err)
			}
			if err := registry.ClaimForApp("publisher", "publisher", port, "10.3.0.2", port); err != nil {
				t.Fatal(err)
			}
			conn, mode, err := dial(context.Background())
			if err != nil || mode != "local-app" {
				t.Fatalf("own VIP dial: %s %v", mode, err)
			}
			defer conn.Close()
			server, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			if _, err := server.Write([]byte("sibling app")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 11)
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "sibling app" {
				t.Fatalf("payload: %q %v", buf, err)
			}
			registry.Release("publisher")
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := conn.Read(buf); err == nil {
				t.Fatal("stopped owner left flow open")
			}
			if _, _, err := dial(context.Background()); !errors.Is(err, meshingress.ErrPortDenied) {
				t.Fatalf("revoked port: %v", err)
			}
			if probes.lookups != 0 || probes.lanDials != 0 || probes.brokerDials != 0 {
				t.Fatalf("own VIP leaked to discovery/relay: %+v", probes)
			}
		})
	}
}

func TestOwnVIPIdentityChangeDoesNotRetainOldSelfRoute(t *testing.T) {
	d, probes := testDialer()
	d.SetSelfDialer(func(context.Context, uint16) (net.Conn, error) {
		t.Fatal("previous asset retained self route")
		return nil, nil
	})
	d.UpdateIdentity("", "", "")
	conn, _, err := d.DialTarget(context.Background(), dialerTestTarget, 8080)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if probes.brokerDials != 1 {
		t.Fatal("old asset did not use peer path")
	}
}
