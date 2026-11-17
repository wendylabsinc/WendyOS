package mtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func TestRPCCompletedChain(t *testing.T) {
	p := crltest.New(t, false)
	m := useCRLManager(t, p, -1)
	own := p.Issue(t, 10, crlPrincipal+"device/22222222-2222-4222-8222-222222222222")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(20), Subject: pkix.Name{CommonName: "operator issuer"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, p.CA, key.Public(), p.Key)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	issuerPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	operatorPKI := &crltest.PKI{CA: issuer, Key: key}
	operator := operatorPKI.Issue(t, 42, crlPrincipal+"operator/alice")
	pair, err := certs.TLSKeyPair(operator.PEM, issuerPEM+p.PEM, operator.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg, check, err := newFloorTLSConfig(own.PEM, p.PEM, own.KeyPEM, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.NextProtos = []string{"h2"}
	transport := newPeerCredentials(cfg, []*x509.Certificate{p.CA}).(*peerCredentials)
	clientMaterial := meshalpn.NewCertificates(pair, []*x509.Certificate{p.CA})
	roots := x509.NewCertPool()
	roots.AddCert(p.CA)
	for round := 0; round < 2; round++ {
		clientCfg := &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost"}
		clientMaterial.ConfigureClient(clientCfg, "h2")
		var sent int
		getCertificate := clientCfg.GetClientCertificate
		clientCfg.GetClientCertificate = func(request *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			pair, err := getCertificate(request)
			if pair != nil {
				sent = len(pair.Certificate)
			}
			return pair, err
		}
		a, b := net.Pipe()
		_ = a.SetDeadline(time.Now().Add(3 * time.Second))
		_ = b.SetDeadline(time.Now().Add(3 * time.Second))
		client := tls.Client(b, clientCfg)
		done := make(chan error, 1)
		go func() { done <- client.Handshake() }()
		_, auth, err := transport.ServerHandshake(a)
		if err != nil {
			a.Close()
			b.Close()
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			a.Close()
			b.Close()
			t.Fatal(err)
		}
		state := auth.(credentials.TLSInfo).State
		if sent != 2-round {
			t.Fatalf("round %d sent %d certificates", round, sent)
		}
		if len(state.PeerCertificates) != 2 {
			t.Fatalf("completed presentation lost: %d certificates", len(state.PeerCertificates))
		}
		// The admitted connection owns the issuers needed by later RPCs even after
		// the shared cache is replaced or evicted.
		if round == 1 {
			transport.material = meshalpn.NewCertificates(cfg.Certificates[0], []*x509.Certificate{p.CA})
		}
		ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: auth})
		err = livePeerCheck(check, revocation.Fresh)(ctx, "/regression.FutureService/NewMethod")
		a.Close()
		b.Close()
		if err != nil {
			t.Fatalf("RPC after successful TLS failed: %v", err)
		}
	}
	m.Refresh(context.Background())
}
