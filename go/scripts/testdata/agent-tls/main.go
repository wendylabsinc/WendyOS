// A local interoperability fixture for the agent's bundled Go TLS server.
package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func ecKey() crypto.Signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	return k
}

func issue(name string, key crypto.Signer, ca *x509.Certificate, caKey crypto.Signer, authority bool) *x509.Certificate {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	must(err)
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: authority, KeyUsage: x509.KeyUsageDigitalSignature}
	if authority {
		t.KeyUsage |= x509.KeyUsageCertSign
		ca, caKey = t, key
	} else {
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
		t.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, t, ca, key.Public(), caKey)
	must(err)
	c, err := x509.ParseCertificate(der)
	must(err)
	return c
}

func writePair(dir, name string, cert *x509.Certificate, key crypto.Signer) {
	must(os.WriteFile(filepath.Join(dir, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600))
	der, err := x509.MarshalPKCS8PrivateKey(key)
	must(err)
	must(os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
}

func main() {
	dir := os.Args[1]
	caKey := ecKey()
	ca := issue("classical CA", caKey, nil, nil, true)
	pqKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	must(err)
	pqCA := issue("ML-DSA CA", pqKey, nil, nil, true)
	serverKey := ecKey()
	server := issue("server", serverKey, ca, caKey, false)
	clientKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	must(err)
	writePair(dir, "mldsa", issue("operator", clientKey, pqCA, pqKey, false), clientKey)
	legacyKey := ecKey()
	writePair(dir, "ecdsa", issue("legacy", legacyKey, ca, caKey, false), legacyKey)
	badKey := ecKey()
	writePair(dir, "untrusted", issue("untrusted CA", badKey, nil, nil, true), badKey)
	writePair(dir, "server", server, serverKey)
	must(os.WriteFile(filepath.Join(dir, "root.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0600))
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	roots.AddCert(pqCA)
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"},
		Certificates: []tls.Certificate{{Certificate: [][]byte{server.Raw}, PrivateKey: serverKey}},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return fmt.Errorf("missing client certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			return err
		}}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	must(err)
	must(json.NewEncoder(os.Stdout).Encode(map[string]int{"port": listener.Addr().(*net.TCPAddr).Port}))
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			if conn.(*tls.Conn).Handshake() == nil {
				conn.Write([]byte("authenticated"))
			}
		}()
	}
}
