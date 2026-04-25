package mtls

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"time"

	circlSign "github.com/cloudflare/circl/sign"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/cloudflare/circl/sign/mldsa/mldsa87"
)

var (
	oidMLDSA65 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}
	oidMLDSA87 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 19}
)

type algID struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type certOuter struct {
	TBSCertificate     asn1.RawValue
	SignatureAlgorithm algID
	Signature          asn1.BitString
}

type spkiOuter struct {
	Algorithm algID
	PublicKey asn1.BitString
}

func parseCertsFromPEM(chainPEM []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := chainPEM
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing certificate from PEM: %w", err)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

func certSigAlgOID(cert *x509.Certificate) (asn1.ObjectIdentifier, error) {
	var outer certOuter
	if _, err := asn1.Unmarshal(cert.Raw, &outer); err != nil {
		return nil, fmt.Errorf("parsing certificate ASN.1: %w", err)
	}
	return outer.SignatureAlgorithm.Algorithm, nil
}

func issuerPublicKeyBytes(issuer *x509.Certificate) (asn1.ObjectIdentifier, []byte, error) {
	var s spkiOuter
	if _, err := asn1.Unmarshal(issuer.RawSubjectPublicKeyInfo, &s); err != nil {
		return nil, nil, fmt.Errorf("parsing SubjectPublicKeyInfo: %w", err)
	}
	return s.Algorithm.Algorithm, s.PublicKey.Bytes, nil
}

func mldsaScheme(oid asn1.ObjectIdentifier) (circlSign.Scheme, error) {
	switch {
	case oid.Equal(oidMLDSA65):
		return mldsa65.Scheme(), nil
	case oid.Equal(oidMLDSA87):
		return mldsa87.Scheme(), nil
	default:
		return nil, fmt.Errorf("unsupported ML-DSA OID: %v", oid)
	}
}

// verifyMLDSASignature checks that issuer signed cert using ML-DSA.
func verifyMLDSASignature(issuer, cert *x509.Certificate) error {
	sigOID, err := certSigAlgOID(cert)
	if err != nil {
		return err
	}

	scheme, err := mldsaScheme(sigOID)
	if err != nil {
		return err
	}

	_, pubKeyBytes, err := issuerPublicKeyBytes(issuer)
	if err != nil {
		return err
	}

	pk, err := scheme.UnmarshalBinaryPublicKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("parsing ML-DSA public key: %w", err)
	}

	opts := &circlSign.SignatureOpts{Context: ""}
	if !scheme.Verify(pk, cert.RawTBSCertificate, cert.Signature, opts) {
		return fmt.Errorf("ML-DSA signature verification failed")
	}
	return nil
}

// buildVerifyPeerCertificate returns a VerifyPeerCertificate callback that
// handles both standard (RSA/ECDSA) and ML-DSA-signed certificate chains.
func buildVerifyPeerCertificate(caPool *x509.CertPool, caCerts []*x509.Certificate) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("no client certificate presented")
		}

		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parsing client certificate: %w", err)
		}

		// Try standard Go verification first (handles RSA/ECDSA chains).
		opts := x509.VerifyOptions{
			Roots:     caPool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}
		if _, err := leaf.Verify(opts); err == nil {
			return nil
		}

		// Fall back to manual ML-DSA chain verification.
		return verifyMLDSAClientCert(leaf, caCerts)
	}
}

// verifyMLDSAClientCert verifies a client leaf cert against the trusted CA certs
// using ML-DSA signature verification. It checks validity and that the leaf was
// signed by a trusted CA.
func verifyMLDSAClientCert(leaf *x509.Certificate, trustedCAs []*x509.Certificate) error {
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("certificate not valid at current time (NotBefore=%v NotAfter=%v)", leaf.NotBefore, leaf.NotAfter)
	}

	for _, ca := range trustedCAs {
		if !bytes.Equal(ca.RawSubject, leaf.RawIssuer) {
			continue
		}
		if err := verifyMLDSASignature(ca, leaf); err != nil {
			return fmt.Errorf("invalid signature from CA %q: %w", ca.Subject.CommonName, err)
		}
		return nil
	}

	return fmt.Errorf("client certificate issuer not found in trusted CA pool")
}
