package localmesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func testCredentials(t *testing.T) (*Credentials, *Credentials) {
	return testCredentialsWithPEM(t, nil)
}

func testCredentialsWithPEM(t *testing.T, capture func(int32, string, string, string)) (*Credentials, *Credentials) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	leaf := func(asset int32) *Credentials {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(fmt.Sprintf("urn:wendy:org:64:asset:%d", asset))
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(asset) + 1), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kd, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
		if capture != nil {
			capture(asset, certPEM, chain, keyPEM)
		}
		c, err := NewCredentials(64, asset, certPEM, chain, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return leaf(445), leaf(460)
}

func TestLocalMeshQUICHandshakeAndControl(t *testing.T) {
	a, b := testCredentials(t)
	serverTLS, err := b.PeerTLS(a.Asset)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			result <- err
			return
		}
		defer conn.CloseWithError(0, "done")
		stream, err := OpenControl(ctx, conn, 64, b.Asset, a.Asset)
		if err != nil {
			result <- err
			return
		}
		m, err := ReadControl(stream)
		if err == nil && m.Kind != "bundle" {
			err = fmt.Errorf("unexpected message %s", m.Kind)
		}
		if err == nil {
			_, err = b.Verify(m.Bundle, time.Now())
		}
		result <- err
	}()
	clientTLS, err := a.PeerTLS(b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	stream, err := OpenControl(ctx, conn, 64, a.Asset, b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteControl(stream, ControlMessage{Kind: "bundle", Bundle: a.Certificate.Certificate}); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsRejectWrongOrigin(t *testing.T) {
	a, b := testCredentials(t)
	tlsConfig, err := a.PeerTLS(461)
	if err != nil {
		t.Fatal(err)
	}
	if err = tlsConfig.VerifyPeerCertificate(b.Certificate.Certificate, nil); err == nil {
		t.Fatal("wrong peer asset accepted")
	}
	verified, err := a.Verify(b.Certificate.Certificate, time.Now())
	if err != nil || verified.Asset != 460 {
		t.Fatal(verified, err)
	}
	if _, err = a.Verify(b.Certificate.Certificate, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired identity accepted")
	}
}

func TestCredentialsRejectMismatchedLocalIdentity(t *testing.T) {
	testCredentialsWithPEM(t, func(asset int32, cert, chain, key string) {
		if _, err := NewCredentials(65, asset, cert, chain, key); err == nil {
			t.Fatal("certificate from a different org accepted")
		}
		if _, err := NewCredentials(64, asset+1, cert, chain, key); err == nil {
			t.Fatal("certificate from a different asset accepted")
		}
	})
}
