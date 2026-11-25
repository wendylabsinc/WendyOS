// Extracted from Wendy f29c41dbb86e8d05a8562b52c86a2f5120ace076; TLS client configuration omitted, imports/package adjusted.
package upstreamcerts

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	circlSign "github.com/cloudflare/circl/sign"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/cloudflare/circl/sign/mldsa/mldsa87"
)

var (
	oidMLDSA65 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}
	oidMLDSA87 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 19}
)

type mldsaAlgID struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type mldsaCertOuter struct {
	TBSCertificate     asn1.RawValue
	SignatureAlgorithm mldsaAlgID
	Signature          asn1.BitString
}

type mldsaSPKI struct {
	Algorithm mldsaAlgID
	PublicKey asn1.BitString
}

func ParseCertsFromPEM(chainPEM []byte) ([]*x509.Certificate, error) {
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
			var raw asn1.RawValue
			if _, asn1Err := asn1.Unmarshal(block.Bytes, &raw); asn1Err == nil {
				cert, err = x509.ParseCertificate(raw.FullBytes)
			}
		}
		if err != nil {
			continue
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

// AppendChainToPool adds every certificate in chainPEM to pool and returns how
// many were added.
//
// It exists because x509.CertPool.AppendCertsFromPEM silently drops the
// certificates pki-core issues: those carry trailing bytes after the outer
// ASN.1 SEQUENCE, so x509.ParseCertificate rejects them (see
// ParseCertsFromPEM). AppendCertsFromPEM then reports false, and callers that
// treated false as fatal refused every ML-DSA chain outright rather than
// merely failing to trust it. Use this instead of AppendCertsFromPEM for any
// Wendy-issued chain, and treat a zero count — not a boolean — as the empty
// case.
func AppendChainToPool(pool *x509.CertPool, chainPEM string) int {
	if chainPEM == "" {
		return 0
	}
	// ParseCertsFromPEM skips what it cannot parse and never returns an error.
	parsed, _ := ParseCertsFromPEM([]byte(chainPEM))
	for _, cert := range parsed {
		pool.AddCert(cert)
	}
	return len(parsed)
}

func mldsaCertSigAlgOID(cert *x509.Certificate) (asn1.ObjectIdentifier, error) {
	var outer mldsaCertOuter
	if _, err := asn1.Unmarshal(cert.Raw, &outer); err != nil {
		return nil, fmt.Errorf("parsing certificate ASN.1: %w", err)
	}
	return outer.SignatureAlgorithm.Algorithm, nil
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

func mldsaIssuerPublicKeyBytes(issuer *x509.Certificate) ([]byte, error) {
	var s mldsaSPKI
	if _, err := asn1.Unmarshal(issuer.RawSubjectPublicKeyInfo, &s); err != nil {
		return nil, fmt.Errorf("parsing SubjectPublicKeyInfo: %w", err)
	}
	return s.PublicKey.Bytes, nil
}

func verifyMLDSASignature(issuer, cert *x509.Certificate) error {
	sigOID, err := mldsaCertSigAlgOID(cert)
	if err != nil {
		return err
	}
	scheme, err := mldsaScheme(sigOID)
	if err != nil {
		return err
	}
	pubKeyBytes, err := mldsaIssuerPublicKeyBytes(issuer)
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
