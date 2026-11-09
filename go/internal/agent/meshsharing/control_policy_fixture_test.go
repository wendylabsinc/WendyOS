package meshsharing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

type controlPolicyClock struct {
	mu        sync.Mutex
	offset    time.Duration
	floor     time.Time
	uncertain bool
	calls     atomic.Int64
}

func (c *controlPolicyClock) policy() certs.CertificateTimePolicy {
	return certs.CertificateTimePolicy{
		Now:       func() time.Time { c.calls.Add(1); c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.offset) },
		Floor:     func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.floor },
		Uncertain: func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.uncertain },
	}
}
func (c *controlPolicyClock) set(floor time.Time, offset time.Duration, uncertain bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.floor, c.offset, c.uncertain = floor, offset, uncertain
}

func policyControlPair(t *testing.T, clock *controlPolicyClock, selfNB, selfNA, peerNB, peerNA time.Time) (*localmesh.Credentials, *localmesh.Credentials) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	root := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	issue := func(device string, nb, na time.Time) *localmesh.Credentials {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse("spiffe://wendy.sh/tenant/" + controlTestTenant + "/device/" + device)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: nb, NotAfter: na, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := localmesh.NewCredentialsWithTimePolicy(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), root, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), clock.policy())
		if err != nil {
			t.Fatal(err)
		}
		return credentials
	}
	return issue(controlTestClient, selfNB, selfNA), issue(controlTestGateway, peerNB, peerNA)
}

func policyControlConn(t *testing.T, self, peer *localmesh.Credentials, onClose func()) (*expiryConn, net.Conn) {
	t.Helper()
	state := tls.ConnectionState{Version: tls.VersionTLS13, NegotiatedProtocol: leaseControlALPN}
	for _, der := range peer.Certificate.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		state.PeerCertificates = append(state.PeerCertificates, cert)
	}
	identity, err := controlPeer(self, state)
	if err != nil {
		t.Fatal(err)
	}
	expires, err := controlExpiry(self, identity)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	bounded := newControlExpiryConn(local, expires, self, state, identity, onClose)
	t.Cleanup(func() { bounded.Close(); remote.Close() })
	return bounded, remote
}
