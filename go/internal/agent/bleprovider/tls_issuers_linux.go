//go:build linux

package bleprovider

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// issuerHintPrefix marks an ALPN offer that names the CA certificates the
// client presents and also trusts (localmesh.Credentials.IssuerDigest). It is
// never selected, so a server that does not know it negotiates ALPN unchanged.
// Enrolled ML-DSA issuers are about 20 KB per direction, five times the
// 3.9 KB leaf. At the ~1 KB/s a busy CoC carries, two full chains took longer
// than meshTLSHandshakeTimeout.
const issuerHintPrefix = "wendy-net-issuers/"

// serverCertificate omits our issuers only for a client that named the same
// issuer set. That client trusts every certificate left out, so its
// verification has exactly the same candidates as with the full chain.
func serverCertificate(credentials *localmesh.Credentials, hello *tls.ClientHelloInfo) *tls.Certificate {
	certificate, _ := credentials.ServerCertificate(hello)
	return certificate
}

// clientCertificate omits our issuers only when the verified server presented
// just its leaf, issued by our own issuer, after we offered our digest. Such a
// server matched our issuer set and trusts it. A server that sent issuers, or
// whose leaf names another issuer, receives the full chain. If the guess is
// ever wrong, the server's verification fails; it never skips a certificate.
func clientCertificate(credentials *localmesh.Credentials, server []*x509.Certificate) *tls.Certificate {
	if len(server) == 1 && credentials.IssuerDigest() != "" && sameIssuer(credentials.Certificate, server[0]) {
		leaf := credentials.LeafCertificate()
		return &leaf
	}
	return &credentials.Certificate
}

func sameIssuer(own tls.Certificate, peer *x509.Certificate) bool {
	leaf := own.Leaf
	if leaf == nil && len(own.Certificate) > 0 {
		leaf, _ = x509.ParseCertificate(own.Certificate[0])
	}
	return leaf != nil && peer != nil && bytes.Equal(leaf.RawIssuer, peer.RawIssuer) &&
		bytes.Equal(leaf.AuthorityKeyId, peer.AuthorityKeyId)
}
