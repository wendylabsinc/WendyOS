package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

const relayIdleChild = "WENDY_TEST_RELAY_IDLE_CHILD"

// Keep the production 30s ping interval. This is intentionally an opt-in,
// once-per-network-job test: shortening it would not prove that a real idle
// transport remains alive through the original idle cutoff. Positive and
// no-keepalive controls run concurrently in isolated processes (~40s total).
func TestRelayIdleKeepalive(t *testing.T) {
	if os.Getenv("WENDY_E2E_RELAY_IDLE") != "1" {
		t.Skip("run once in the network job with WENDY_E2E_RELAY_IDLE=1")
	}
	if mode := os.Getenv(relayIdleChild); mode != "" {
		testRelayIdleKeepaliveChild(t, mode)
		return
	}
	type result struct {
		mode   string
		output []byte
		err    error
	}
	results := make(chan result, 2)
	for _, mode := range []string{"production", "disabled"} {
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRelayIdleKeepalive$", "-test.v", "-test.timeout=55s")
			// A process-private root permits the actual production DialBroker
			// without modifying machine trust or global trust in other tests.
			cmd.Env = append(os.Environ(), relayIdleChild+"="+mode,
				"GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
			cmd.WaitDelay = 2 * time.Second
			output, err := cmd.CombinedOutput()
			results <- result{mode, output, err}
		}()
	}
	for range 2 {
		r := <-results
		t.Logf("%s child:\n%s", r.mode, r.output)
		if r.mode == "production" {
			if r.err != nil || !strings.Contains(string(r.output), "IDLE_KEEPALIVE_PRESERVED") {
				t.Errorf("production keepalive failed: %v", r.err)
			}
		} else {
			var exit *exec.ExitError
			if !errors.As(r.err, &exit) || !strings.Contains(string(r.output), "IDLE_CONTROL_TRANSPORT_EXPIRED") ||
				!strings.Contains(string(r.output), "same session after idle cutoff:") ||
				!strings.Contains(string(r.output), "--- FAIL: TestRelayIdleKeepalive") {
				t.Errorf("disabled keepalive did not fail at the idle data-path assertion: %v", r.err)
			}
		}
	}
}

func testRelayIdleKeepaliveChild(t *testing.T, mode string) {
	if mode != "production" && mode != "disabled" {
		t.Fatalf("unknown idle fixture mode %q", mode)
	}
	want := keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: true}
	if brokerKeepalive != want {
		t.Fatalf("fixture requires production keepalive contract %+v, got %+v", want, brokerKeepalive)
	}
	if mode == "disabled" {
		// Same public dial, TLS, admission and sockets, with only the option
		// omitted. The child must fail when reusing its original data stream.
		withKeepalive = func(keepalive.ClientParameters) grpc.DialOption { return grpc.EmptyDialOption{} }
	}
	certificate, roots := relayIdleCertificate(t)
	x509.SetFallbackRoots(roots)
	var expired atomic.Int32
	var relay *testRelay
	var agent *Agent
	var path *relayTCPPath
	lifecycle := &relayLifecycle{
		timeout: 50 * time.Second,
		serverOptions: []grpc.ServerOption{
			grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}})),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		},
		dial: func(endpoint string) (*grpc.ClientConn, error) {
			return grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "127.0.0.1",
			})))
		},
	}
	lifecycle.configure = func(r *testRelay, a *Agent) {
		relay, agent = r, a
		path = startRelayTCPPath(t, r.endpoint, func(c net.Conn) net.Conn {
			idle := &relayIdleConn{Conn: c, expired: &expired}
			idle.touch()
			return idle
		})
		a.Verifier.relayDial = func(string) (*grpc.ClientConn, error) {
			return DialBroker(path.listener.Addr().String())
		}
	}
	lifecycle.exercise = func(exchange func(string)) {
		resume := readRelayResume(t, agent)
		relay.mu.Lock()
		initialIssues, initialJoins, initialRegistrations := relay.issues, relay.joins, relay.registrations
		relay.mu.Unlock()
		if initialIssues != 1 || initialJoins != 2 || initialRegistrations != 1 || path.accepted.Load() != 3 {
			t.Fatal("fixture did not admit exactly one presence and the two original session streams")
		}
		// No application writes, renewal, probes, or server pings during this
		// interval. Only the real client's HTTP/2 keepalive can preserve the
		// TLS sockets past the proxy's 35s bidirectional inactivity deadline.
		time.Sleep(40 * time.Second)
		if mode == "disabled" {
			if expired.Load() == 0 {
				t.Fatal("disabled control did not reach the proxy idle cutoff")
			}
			t.Log("IDLE_CONTROL_TRANSPORT_EXPIRED")
		} else if expired.Load() != 0 {
			t.Fatalf("production keepalive lost %d idle transports", expired.Load())
		}
		exchange("same session after idle cutoff")
		if mode == "disabled" {
			t.Fatal("disabled keepalive unexpectedly preserved the original session")
		}
		relay.mu.Lock()
		issues, joins, registrations := relay.issues, relay.joins, relay.registrations
		relay.mu.Unlock()
		if issues != initialIssues || joins != initialJoins || registrations != initialRegistrations ||
			path.accepted.Load() != 3 || readRelayResume(t, agent) != resume {
			t.Fatal("idle survival silently replaced the presence, signed lease, transport or session")
		}
		t.Log("IDLE_KEEPALIVE_PRESERVED")
	}
	testAuthorizedRelay(t, "agent", false, lifecycle)
}

// The proxy sees opaque TLS bytes. Activity in either direction renews the
// same client-read deadline; expiration closes both forwarding directions.
// The 35s cutoff is above the unchanged 30s client ping interval.
type relayIdleConn struct {
	net.Conn
	expired *atomic.Int32
}

func (c *relayIdleConn) touch() { _ = c.Conn.SetReadDeadline(time.Now().Add(35 * time.Second)) }

func (c *relayIdleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		c.expired.Add(1)
	}
	return n, err
}

func (c *relayIdleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func relayIdleCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated idle relay fixture"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}
