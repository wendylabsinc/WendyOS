package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

// forwardTestTLS is a server config for Agent.ForwardTLS and a client config
// that accepts it. The production policies are the agent's management TLS and
// the CLI's agent TLS; these tests cover the session around them.
func forwardTestTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"h2"}}
	client = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}} //nolint:gosec // test peer
	return server, client
}

// A caller that sends the mux preamble gets its connection as a stream of a
// multiplexed session: the agent runs the endpoint TLS handshake and dials the
// authorized port per stream.
func TestAgentServesMultiplexedForward(t *testing.T) {
	serverTLS, clientTLS := forwardTestTLS(t)
	testAuthorizedRelay(t, "tcp", false, &relayLifecycle{
		configure: func(_ *testRelay, a *Agent) {
			a.ForwardTLS = func() (*tls.Config, error) { return serverTLS, nil }
			a.MTLSPort = 0 // the harness's echo port stands in for the agent's own
		},
		tcpOpen: func(ctx, authCtx context.Context, cloudConn *grpc.ClientConn, verifier *Verifier, assetID, service string, sign func([]byte) ([]byte, error)) (net.Conn, error) {
			conn, expires, err := OpenTCPMux(ctx, authCtx, cloudConn, verifier, assetID, service, sign)
			if err != nil {
				return nil, err
			}
			secured := tls.Client(conn, clientTLS)
			if err := secured.HandshakeContext(ctx); err != nil {
				return nil, err
			}
			mux, err := NewMuxClient(secured, expires)
			if err != nil {
				return nil, err
			}
			return mux.Dial(ctx)
		},
		exercise: func(func(string)) {},
	})
}

// An agent that can multiplex still serves a caller that does not: its first
// frame is the start of one raw connection.
func TestMuxCapableAgentServesRawCaller(t *testing.T) {
	serverTLS, _ := forwardTestTLS(t)
	testAuthorizedRelay(t, "tcp", false, &relayLifecycle{
		configure: func(_ *testRelay, a *Agent) {
			a.ForwardTLS = func() (*tls.Config, error) { return serverTLS, nil }
			a.MTLSPort = 0 // the harness's echo port stands in for the agent's own
		},
		exercise: func(func(string)) {},
	})
}

// A device service that stops reading one connection's upload must not stall
// the other connections of the session.
func TestMuxStalledStreamLeavesOthersMoving(t *testing.T) {
	svc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := svc.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) == 1 {
				t.Cleanup(func() { c.Close() }) // accepts, never reads
				continue
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	port := strconv.Itoa(svc.Addr().(*net.TCPAddr).Port)
	device, caller := net.Pipe()
	go muxServer(64).ServeConn(device, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardStream(w, r, "127.0.0.1", port, zap.NewNop())
	})})
	m, err := NewMuxClient(caller, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	stalled, err := m.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	go func() { _, _ = stalled.Write(make([]byte, 256<<20)) }()
	time.Sleep(2 * time.Second) // let the stalled upload fill every buffer it can
	moving, err := m.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer moving.Close()
	got := make(chan string, 1)
	go func() {
		_, _ = io.WriteString(moving, "hello")
		buf := make([]byte, 5)
		_, _ = io.ReadFull(moving, buf)
		got <- string(buf)
	}()
	select {
	case g := <-got:
		if g != "hello" {
			t.Fatalf("got %q", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second connection stalled behind the first one's unread upload")
	}
}
