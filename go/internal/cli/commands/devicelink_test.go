package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// These tests cover the device dials routed through deviceLinkDialFn, which
// finds the USB link a 169.254.x.x device is on when several links share it.

func stubDeviceLinkDial(t *testing.T, fn func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	orig := deviceLinkDialFn
	t.Cleanup(func() { deviceLinkDialFn = orig })
	deviceLinkDialFn = fn
}

// addrConn is a net.Conn reporting a fixed local address.
type addrConn struct {
	net.Conn
	local net.Addr
}

func (c addrConn) LocalAddr() net.Addr { return c.local }

func connFrom(t *testing.T, localIP string) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return addrConn{Conn: a, local: &net.TCPAddr{IP: net.ParseIP(localIP), Port: 40000}}
}

func stubLinkLocalSourceIP(t *testing.T, fn func(context.Context, string, ...int) (string, bool)) {
	t.Helper()
	orig := linkLocalSourceIPFn
	t.Cleanup(func() { linkLocalSourceIPFn = orig })
	linkLocalSourceIPFn = fn
}

// The artifact URL must carry this host's address on the link that reaches
// the device, on either agent port: an enrolled agent serves only mTLS.
func TestLocalIPForHostUsesTheLinkThatReachesTheDevice(t *testing.T) {
	var gotHost string
	var gotPorts []int
	stubLinkLocalSourceIP(t, func(_ context.Context, host string, ports ...int) (string, bool) {
		gotHost, gotPorts = host, ports
		return "169.254.39.227", true
	})

	got, err := localIPForHost(context.Background(), "169.254.198.132")
	if err != nil || got != "169.254.39.227" {
		t.Fatalf("localIPForHost = %q, %v; want 169.254.39.227", got, err)
	}
	if gotHost != "169.254.198.132" || len(gotPorts) != 2 || gotPorts[0] != 50051 || gotPorts[1] != 50052 {
		t.Fatalf("SourceIP asked for %q ports %v; want 169.254.198.132 ports [50051 50052]", gotHost, gotPorts)
	}
}

// The command's ctx has no deadline; a device that drops SYNs must not stall
// the update for the kernel's connect timeout.
func TestLocalIPForHostBoundsTheLinkProbe(t *testing.T) {
	stubLinkLocalSourceIP(t, func(ctx context.Context, _ string, _ ...int) (string, bool) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("link probe has no deadline")
		}
		return "", false
	})

	_, _ = localIPForHost(context.Background(), "169.254.198.132")
}

func TestLocalIPForHostFallsBackToTheRoutingTable(t *testing.T) {
	stubLinkLocalSourceIP(t, func(context.Context, string, ...int) (string, bool) { return "", false })

	got, err := localIPForHost(context.Background(), "127.0.0.1")
	if err != nil || got != "127.0.0.1" {
		t.Fatalf("localIPForHost = %q, %v; want 127.0.0.1", got, err)
	}
}

func TestServeLocalArtifactListensThroughTheLinkAwareListener(t *testing.T) {
	orig := artifactListenFn
	t.Cleanup(func() { artifactListenFn = orig })
	var listened string
	artifactListenFn = func(ctx context.Context, address string) (net.Listener, error) {
		listened = address
		var lc net.ListenConfig
		return lc.Listen(ctx, "tcp", address)
	}
	path := filepath.Join(t.TempDir(), "wendyos.wendy")
	if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}

	url, cleanup, err := serveLocalArtifact(path, "127.0.0.1")
	if err != nil {
		t.Fatalf("serveLocalArtifact: %v", err)
	}
	defer cleanup()
	if listened != "127.0.0.1:0" {
		t.Fatalf("listened on %q, want 127.0.0.1:0", listened)
	}
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); string(body) != "artifact" {
		t.Fatalf("served %q, want %q", body, "artifact")
	}
}

func TestRegistryProxyDialsTheDeviceThroughItsLink(t *testing.T) {
	device, toDevice := net.Pipe()
	t.Cleanup(func() { device.Close(); toDevice.Close() })
	var dialed string
	stubDeviceLinkDial(t, func(_ context.Context, addr string) (net.Conn, error) {
		dialed = addr
		return toDevice, nil
	})

	proxy, err := startRegistryProxy(context.Background(), "127.0.0.1:0", "169.254.198.132:5000")
	if err != nil {
		t.Fatalf("startRegistryProxy: %v", err)
	}
	defer proxy.Close()
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port())))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = device.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(device, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("device read %q, %v; want ping", buf, err)
	}
	if dialed != "169.254.198.132:5000" {
		t.Fatalf("dialed %q, want 169.254.198.132:5000", dialed)
	}
}

func TestLKGPrecheckDialsTheDeviceThroughItsLink(t *testing.T) {
	var dialed string
	stubDeviceLinkDial(t, func(_ context.Context, addr string) (net.Conn, error) {
		dialed = addr
		return connFrom(t, "169.254.39.227"), nil
	})

	if !lkgTCPAlive("169.254.198.132:50052") || dialed != "169.254.198.132:50052" {
		t.Fatalf("lkgTCPAlive dialed %q; want a live dial of 169.254.198.132:50052", dialed)
	}
}

func TestReadinessProbeDialsTheDeviceThroughItsLink(t *testing.T) {
	var dialed string
	stubDeviceLinkDial(t, func(_ context.Context, addr string) (net.Conn, error) {
		dialed = addr
		return connFrom(t, "169.254.39.227"), nil
	})

	_, probe, cleanup := makeReadinessProbe("169.254.198.132", 8080, false)
	defer cleanup()
	if err := probe(context.Background()); err != nil || dialed != "169.254.198.132:8080" {
		t.Fatalf("probe = %v, dialed %q; want a dial of 169.254.198.132:8080", err, dialed)
	}
}

// Enrolled devices push through this proxy, so it must dial the device's link.
func TestMTLSRegistryHTTPProxyDialsTheDeviceThroughItsLink(t *testing.T) {
	ca := generateTestCA(t)
	serverLeaf := generateTestLeaf(t, ca, x509.ExtKeyUsageServerAuth)
	clientLeaf := generateTestLeaf(t, ca, x509.ExtKeyUsageClientAuth)
	serverCert, err := tls.X509KeyPair([]byte(serverLeaf.pemStr), []byte(marshalKeyPEM(t, serverLeaf.key)))
	if err != nil {
		t.Fatal(err)
	}
	registry := startTestTLSServer(t, serverCert, ca)
	var dialed string
	stubDeviceLinkDial(t, func(ctx context.Context, addr string) (net.Conn, error) {
		dialed = addr
		var d net.Dialer
		return d.DialContext(ctx, "tcp", registry)
	})

	proxy, err := startMTLSRegistryHTTPProxy("169.254.198.132:5001", clientLeaf.pemStr, marshalKeyPEM(t, clientLeaf.key), ca.pemStr)
	if err != nil {
		t.Fatalf("startMTLSRegistryHTTPProxy: %v", err)
	}
	defer proxy.Close()
	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(proxy.Port()))) //nolint:noctx
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || dialed != "169.254.198.132:5001" {
		t.Fatalf("status %d, dialed %q; want 200 via 169.254.198.132:5001", resp.StatusCode, dialed)
	}
}
