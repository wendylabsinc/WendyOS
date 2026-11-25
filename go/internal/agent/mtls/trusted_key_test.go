package mtls

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func clientCert(t *testing.T, key crypto.Signer) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "operator"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func spki(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// handshake dials srv with cert and pins the server key the way the CLI does.
func handshake(t *testing.T, srv *tls.Config, cert tls.Certificate, serverSPKI []byte) error {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	srvErr := make(chan error, 1)
	go func() {
		s, err := lis.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(10 * time.Second))
		srvErr <- tls.Server(s, srv).Handshake()
	}()
	c, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	cli := tls.Client(c, &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // pinned below, like an SSH host key
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil || !bytes.Equal(leaf.RawSubjectPublicKeyInfo, serverSPKI) {
				return errors.New("server key does not match the pin")
			}
			return nil
		},
	})
	cliErr := cli.Handshake()
	// The server's verdict on the client key is the one under test.
	if err := <-srvErr; err != nil {
		return err
	}
	return cliErr
}

func TestTrustedKeyTLS(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, err := ClosedServerIdentity(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The server key is kept: a second start presents the same key.
	again, keyAgain, err := ClosedServerIdentity(dir, time.Now())
	if err != nil || keyAgain != keyPEM {
		t.Fatalf("server key changed across starts: %v", err)
	}
	serverLeaf := func(p string) []byte {
		c, err := certs.ParseCertsFromPEM([]byte(p))
		if err != nil || len(c) != 1 {
			t.Fatalf("server cert: %v", err)
		}
		return c[0].RawSubjectPublicKeyInfo
	}
	serverSPKI := serverLeaf(certPEM)
	if !bytes.Equal(serverSPKI, serverLeaf(again)) {
		t.Fatal("server SPKI changed across starts")
	}

	keys, err := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	mldsaPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	ownerKey, err := certs.ParseSigningPrivateKeyPEM([]byte(mldsaPEM))
	if err != nil {
		t.Fatal(err)
	}
	viewerKey, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	strangerKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	owner, err := keys.Close(spki(t, ownerKey), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(spki(t, viewerKey), trustedkeys.RoleViewer, "", owner.Fingerprint(), time.Now()); err != nil {
		t.Fatal(err)
	}

	grpcTLS, err := NewTrustedKeyTLSConfig(certPEM, keyPEM, keys, trustedkeys.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	registryTLS, err := NewTrustedKeyTLSConfig(certPEM, keyPEM, keys, trustedkeys.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		srv  *tls.Config
		key  crypto.Signer
		ok   bool
	}{
		{"ML-DSA owner", grpcTLS, ownerKey, true},
		{"ML-DSA viewer", grpcTLS, viewerKey, true},
		{"untrusted key", grpcTLS, strangerKey, false},
		{"viewer on the registry", registryTLS, viewerKey, false},
		{"owner on the registry", registryTLS, ownerKey, true},
	} {
		if err := handshake(t, c.srv, clientCert(t, c.key), serverSPKI); (err == nil) != c.ok {
			t.Errorf("%s: handshake err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}
