package meshsession

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

type nativeIngressIdentity struct {
	cert, chain, key string
	credentials      *localmesh.Credentials
}

func nativeIngressIdentities(t *testing.T, policy certs.CertificateTimePolicy, now time.Time) (nativeIngressIdentity, nativeIngressIdentity, nativeIngressIdentity) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	issue := func(device string, notBefore time.Time, meshPolicy bool) nativeIngressIdentity {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		principal, err := url.Parse(fixturePrincipal(device))
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(notBefore.UnixNano()), NotBefore: notBefore, NotAfter: now.Add(time.Hour), URIs: []*url.URL{principal}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		out := nativeIngressIdentity{cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), chain: chain, key: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))}
		if meshPolicy {
			out.credentials, err = localmesh.NewCredentialsWithTimePolicy(out.cert, out.chain, out.key, policy)
		} else {
			out.credentials, err = localmesh.NewCredentials(out.cert, out.chain, out.key)
		}
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// A's recently issued certificate is not yet valid at B's stale UTC. B's
	// older provisioned certificate and all issuer certificates already are.
	return issue(fixtureDeviceA, now.Add(-time.Minute), false), issue(fixtureDeviceB, now.Add(-time.Hour), true), issue(fixtureDeviceC, now.Add(-time.Minute), false)
}

func nativeIngressTLSConfig(t *testing.T, identity nativeIngressIdentity, policy certs.CertificateTimePolicy) *tls.Config {
	t.Helper()
	cfg, err := mtls.NewTLSConfigWithTimePolicy(identity.cert, identity.chain, identity.key, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS13, tls.VersionTLS13
	return cfg
}

func nativeIngressClientConfig(t *testing.T, identity nativeIngressIdentity, target meshid.Device) *tls.Config {
	t.Helper()
	cfg, err := mtls.NewClientTLSConfigExpectingDevice(identity.cert, identity.chain, identity.key, nil, target)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ClientSessionCache = nil
	return cfg
}

func acceptNativeTLS(t *testing.T, listener net.Listener, config *tls.Config) <-chan struct {
	state tls.ConnectionState
	err   error
} {
	t.Helper()
	result := make(chan struct {
		state tls.ConnectionState
		err   error
	}, 1)
	go func() {
		var state tls.ConnectionState
		raw, err := listener.Accept()
		if err == nil {
			defer raw.Close()
			secure := tls.Server(raw, config)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = secure.HandshakeContext(ctx)
			if err == nil {
				state = secure.ConnectionState()
				var request [1]byte
				_ = secure.SetDeadline(time.Now().Add(3 * time.Second))
				_, err = io.ReadFull(secure, request[:])
				if err == nil {
					_, err = secure.Write(request[:])
				}
			}
		}
		result <- struct {
			state tls.ConnectionState
			err   error
		}{state, err}
	}()
	return result
}

func TestMeshNativeTLSBootPolicyIsScopedAndKeepsInnerIdentity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	stale := now.Add(-598 * time.Second)
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return stale }, Floor: func() time.Time { return now.Add(-time.Hour) }, Uncertain: func() bool { return true }}
	a, b, c := nativeIngressIdentities(t, policy, now)
	listener := meshoperatorlink.NewMeshTLSListener()
	t.Cleanup(func() { listener.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var dials atomic.Int32
	_, address := startTLSAppTest(t, b.credentials, WithAgentServiceDial(nil, 50052, func() (net.Conn, error) { dials.Add(1); return listener.DialContext(ctx) }))
	client, err := NewClient(a.credentials)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	innerServerConfig := nativeIngressTLSConfig(t, b, policy)
	result := acceptNativeTLS(t, listener, innerServerConfig)
	flow, err := client.DialTLS(ctx, b.credentials.Device, address, 50052)
	if err != nil {
		t.Fatal("boot-uncertain authenticated outer session", err)
	}
	inner := tls.Client(flow, nativeIngressClientConfig(t, a, b.credentials.Device))
	if err := inner.HandshakeContext(ctx); err != nil {
		flow.Close()
		t.Fatal("boot-uncertain inner TLS", err)
	}
	tlsEcho(t, inner, []byte{42})
	_ = flow.Close()
	verified := <-result
	if verified.err != nil {
		t.Fatal(verified.err)
	}
	peer, err := meshid.PeerFromCert(verified.state.PeerCertificates[0])
	if err != nil || peer != a.credentials.Device || peer.IsOperator() {
		t.Fatal("inner device identity or role changed through mesh ingress", peer, err)
	}
	if dials.Load() != 1 {
		t.Fatal("unexpected local native handoffs", dials.Load())
	}

	// A new inner target must still match the exact device requested, despite
	// an already authenticated outer session to B and a trusted common issuer.
	result = acceptNativeTLS(t, listener, innerServerConfig)
	flow, err = client.DialTLS(ctx, b.credentials.Device, address, 50052)
	if err != nil {
		t.Fatal(err)
	}
	inner = tls.Client(flow, nativeIngressClientConfig(t, a, c.credentials.Device))
	if err := inner.HandshakeContext(ctx); err == nil {
		t.Fatal("inner exact-target check was bypassed")
	}
	_ = flow.Close()
	if verified = <-result; verified.err == nil {
		t.Fatal("wrong target completed inner request")
	}

	// Plaintext carried by an authenticated device session is never treated as
	// an operator RPC transport: the private ingress still requires inner TLS.
	result = acceptNativeTLS(t, listener, innerServerConfig)
	flow, err = client.DialTLS(ctx, b.credentials.Device, address, 50052)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = flow.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	_ = flow.Close()
	if verified = <-result; verified.err == nil || verified.state.HandshakeComplete {
		t.Fatal("outer device session inherited an authenticated inner identity")
	}

	// The private mesh policy is selected by a local listener, not by bytes a
	// peer can put on another connection. A separate strict-clock TLS ingress
	// with the same keys rejects A's not-yet-valid-at-stale-UTC certificate.
	strict := certs.CertificateTimePolicy{Now: func() time.Time { return stale }}
	lan, remote := net.Pipe()
	serverResult := make(chan error, 1)
	go func() {
		defer lan.Close()
		serverResult <- tls.Server(lan, nativeIngressTLSConfig(t, b, strict)).HandshakeContext(ctx)
	}()
	secure := tls.Client(remote, nativeIngressClientConfig(t, a, b.credentials.Device))
	_ = secure.HandshakeContext(ctx)
	_ = remote.Close()
	if err := <-serverResult; err == nil {
		t.Fatal("strict ingress inherited the mesh clock policy")
	}

	// Anonymous and wrong-target outer app sessions cannot even open the local
	// native ingress, regardless of their requested port or application bytes.
	before := dials.Load()
	if anonymous, err := tls.Dial("tcp", address.String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{TLSALPN}, MinVersion: tls.VersionTLS13}); err == nil {
		_ = anonymous.SetDeadline(time.Now().Add(time.Second))
		_, _ = anonymous.Write([]byte{'W', 'A', 'S', '1', 0xc3, 0x84})
		_, _ = anonymous.Read(make([]byte, 1))
		_ = anonymous.Close()
	}
	if wrong, err := client.DialTLS(ctx, c.credentials.Device, address, 50052); err == nil {
		wrong.Close()
		t.Fatal("wrong outer target accepted")
	}
	if dials.Load() != before {
		t.Fatal("unauthenticated outer session opened local native ingress")
	}
}
