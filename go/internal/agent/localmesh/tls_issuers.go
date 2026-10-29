package localmesh

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"slices"
)

const IssuerHintPrefix = "wendy-net-issuers/"

// withKnownIssuers supplies missing intermediates for a leaf issued by our own
// issuer. These are normal untrusted verification candidates, never new roots.
// Complete incoming chains keep their original shape and normal fallback.
func withKnownIssuers(chain [][]byte, own tls.Certificate) [][]byte {
	if len(chain) != 1 || len(own.Certificate) < 2 {
		return chain
	}
	peer, err := x509.ParseCertificate(chain[0])
	if err != nil || !sameIssuer(own, peer) {
		return chain
	}
	augmented := make([][]byte, 1, len(own.Certificate))
	augmented[0] = chain[0]
	return append(augmented, own.Certificate[1:]...)
}

func (c *Credentials) knownIssuerCandidates(chain [][]byte) [][]byte {
	return withKnownIssuers(chain, c.Certificate)
}

func sameIssuer(own tls.Certificate, peer *x509.Certificate) bool {
	leaf := own.Leaf
	if leaf == nil && len(own.Certificate) != 0 {
		leaf, _ = x509.ParseCertificate(own.Certificate[0])
	}
	return leaf != nil && peer != nil && bytes.Equal(leaf.RawIssuer, peer.RawIssuer) &&
		bytes.Equal(leaf.AuthorityKeyId, peer.AuthorityKeyId)
}

// ClientProtocols adds an auxiliary availability hint, never an application
// protocol selected by the server. Certificate material remains binary DER.
func (c *Credentials) ClientProtocols(protocols ...string) []string {
	result := slices.Clone(protocols)
	if digest := c.IssuerDigest(); digest != "" {
		result = append(result, IssuerHintPrefix+digest)
	}
	return result
}

// ConfigureTLSClient operates on a connection-specific config. Its certificate
// selection state must not be shared by cached TLS configs or concurrent dials.
// crypto/tls completes peer verification before asking for our client identity.
func (c *Credentials) ConfigureTLSClient(config *tls.Config, protocols ...string) {
	config.NextProtos = c.ClientProtocols(protocols...)
	if c.IssuerDigest() == "" {
		return
	}
	var verifiedServer []*x509.Certificate
	previous := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		verifiedServer = state.PeerCertificates
		return nil
	}
	config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		if len(verifiedServer) == 1 && sameIssuer(c.Certificate, verifiedServer[0]) {
			leaf := c.LeafCertificate()
			return &leaf, nil
		}
		return &c.Certificate, nil
	}
}

// ServerCertificate omits issuers only when the peer explicitly named the same
// exact issuer set. The peer must still validate our leaf to its own anchors.
func (c *Credentials) ServerCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if digest := c.IssuerDigest(); digest != "" && slices.Contains(hello.SupportedProtos, IssuerHintPrefix+digest) {
		leaf := c.LeafCertificate()
		return &leaf, nil
	}
	return &c.Certificate, nil
}

// ConfigureTLSServer ensures selection runs even when ClientHello has no SNI.
func (c *Credentials) ConfigureTLSServer(config *tls.Config) {
	config.Certificates = nil
	config.GetCertificate = c.ServerCertificate
}
