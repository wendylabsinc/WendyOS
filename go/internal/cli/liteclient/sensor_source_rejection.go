package liteclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
)

// ErrSourceRejectedCertificate reports that a SensorLink source refused this
// consumer's certificate. TLS 1.3 lets the consumer finish its handshake first,
// so the refusal surfaces on the first read.
var ErrSourceRejectedCertificate = errors.New("the source rejected this device's certificate")

// Alerts a peer sends when it cannot accept a certificate, as Go names them.
var certificateAlerts = map[string]bool{
	"tls: bad certificate":               true,
	"tls: unsupported certificate":       true,
	"tls: revoked certificate":           true,
	"tls: expired certificate":           true,
	"tls: unknown certificate":           true,
	"tls: unknown certificate authority": true,
	"tls: certificate required":          true,
}

// peerRejectedCertificate classifies the first read after the TLS handshake.
// mbedTLS sends its rejection under the handshake keys while Go reads with the
// application keys, which Go reports as a MAC failure; the same failure can
// hide another server-side error, so the description stays tentative.
func peerRejectedCertificate(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Err == nil {
		return false
	}
	switch op.Op {
	case "remote error":
		return certificateAlerts[op.Err.Error()]
	case "local error":
		return op.Err.Error() == "tls: bad record MAC"
	}
	return false
}

// describeRejection names what the source was asked to trust, and why a board
// on the classical Lite chain cannot.
func describeRejection(own *x509.Certificate, peer []*x509.Certificate) string {
	desc := fmt.Sprintf("this device's certificate is issued by %q and signed with %s", own.Issuer.CommonName, own.SignatureAlgorithm)
	if len(peer) > 0 {
		key, ok := peer[0].PublicKey.(*ecdsa.PublicKey)
		if ok && key.Curve == elliptic.P256() && own.PublicKeyAlgorithm != x509.ECDSA {
			desc += "; the source is on the classical P-256 Lite chain and may not be able to verify it"
		}
	}
	return desc
}
