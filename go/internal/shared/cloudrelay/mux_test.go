package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"testing"
	"time"

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
