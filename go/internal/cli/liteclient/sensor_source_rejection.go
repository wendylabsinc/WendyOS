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

// ErrSourceMayHaveRejectedCertificate reports an ambiguous TLS read failure.
var ErrSourceMayHaveRejectedCertificate = errors.New("the source may have rejected this device's certificate")

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

// sourceCertificateRejection classifies the first read after the TLS handshake.
// mbedTLS can send a rejection under handshake keys while Go reads with
// application keys. Go reports a MAC failure, which has other causes too.
func sourceCertificateRejection(err error) error {
	var op *net.OpError
	if !errors.As(err, &op) || op.Err == nil {
		return nil
	}
	switch op.Op {
	case "remote error":
		if certificateAlerts[op.Err.Error()] {
			return ErrSourceRejectedCertificate
		}
	case "local error":
		if op.Err.Error() == "tls: bad record MAC" {
			return ErrSourceMayHaveRejectedCertificate
		}
	}
	return nil
}

// describeRejection names what the source was asked to trust, and why a board
// on the classical Lite chain cannot.
func describeRejection(own *x509.Certificate, peer []*x509.Certificate) string {
	desc := fmt.Sprintf("this device's certificate is issued by %q and signed with %s", own.Issuer.CommonName, own.SignatureAlgorithm)
	if len(peer) > 0 {
		leaf := peer[0]
		key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
		if ok && key.Curve == elliptic.P256() && leaf.SignatureAlgorithm == x509.ECDSAWithSHA256 &&
			own.PublicKeyAlgorithm != x509.ECDSA {
			desc += "; the source is on the classical P-256 Lite chain and may not be able to verify it"
		}
	}
	return desc
}
