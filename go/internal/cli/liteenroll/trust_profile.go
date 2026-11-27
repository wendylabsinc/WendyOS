package liteenroll

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// TrustProfile describes the certificate algorithms advertised by the board.
// Its zero value preserves the mbedTLS restrictions used by older firmware.
type TrustProfile struct {
	mldsa                bool
	peerMLDSA            bool
	meshTrust            bool
	extendedTrust        bool
	existingDeviceIssuer bool
}

// TrustProfileFromChallenge permits ML-DSA only when firmware explicitly reports
// that its optional verifier is enabled. Missing or unknown legacy capabilities
// must not infer support from the enrolling computer's crypto implementation.
func TrustProfileFromChallenge(challenge *litepb.WendyComEnrollmentChallenge) TrustProfile {
	if challenge == nil {
		return TrustProfile{}
	}
	return TrustProfile{mldsa: challenge.GetMldsaVerificationSupported(),
		peerMLDSA:     challenge.GetMldsaPeerVerificationSupported(),
		meshTrust:     challenge.GetMeshTrustSupported(),
		extendedTrust: challenge.MldsaPeerVerificationSupported != nil}
}

func (profile TrustProfile) supportsMLDSAPeers() bool {
	return profile.mldsa || profile.peerMLDSA
}

// WithExistingDeviceIssuer preserves an issuer configured with explicit root
// files or firmware-pinned roots. Firmware algorithm restrictions still apply.
func (profile TrustProfile) WithExistingDeviceIssuer() TrustProfile {
	profile.existingDeviceIssuer = true
	return profile
}

// chainAlgorithm selects the same issuer for CA discovery and the signed
// enrollment request. Legacy firmware gets a P-256 chain by default.
func (profile TrustProfile) chainAlgorithm() string {
	if profile.mldsa || profile.existingDeviceIssuer {
		return ""
	}
	return cloudrequest.EnrollmentChainP256
}

func validateMbedTLSTrustBundle(data []byte) error {
	return validateTrustBundleForProfile(data, TrustProfile{})
}

func validateTrustBundleForProfile(data []byte, profile TrustProfile) error {
	return validateTrustDomain(data, profile, true)
}

func validateTrustDomain(data []byte, profile TrustProfile, device bool) error {
	limit := maxTrustBundle
	if device && profile.extendedTrust {
		limit = maxDeviceTrustBundle
	}
	pemLimit := limit
	if profile.extendedTrust {
		// The firmware receives concatenated DER; PEM framing is host-only.
		pemLimit *= 2
	}
	if err := validateTrustBundleSize(data, pemLimit); err != nil {
		return err
	}
	derSize := 0
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		block, tail := pem.Decode(rest)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("invalid certificate: %w", err)
		}
		derSize += len(block.Bytes)
		if derSize > limit {
			return fmt.Errorf("trust bundle exceeds %d DER bytes", limit)
		}
		if !profile.supportsCertificateAlgorithms(cert) {
			if profile.supportsMLDSAPeers() {
				return fmt.Errorf("CA public key %s and signature %s are not supported by the board's advertised verifier; use a certificate chain with RSA, ECDSA or ML-DSA algorithms; no cloud asset was reserved", cert.PublicKeyAlgorithm, cert.SignatureAlgorithm)
			}
			remedy := "the PKI deployment needs an mbedTLS-compatible certificate chain using RSA or ECDSA"
			if cert.PublicKeyAlgorithm == x509.MLDSA || cert.SignatureAlgorithm == x509.MLDSA44 || cert.SignatureAlgorithm == x509.MLDSA65 || cert.SignatureAlgorithm == x509.MLDSA87 {
				remedy += ", or firmware built with ML-DSA verification"
			}
			return fmt.Errorf("CA public key %s and signature %s are not supported by the current Wendy Lite mbedTLS firmware; %s; no cloud asset was reserved", cert.PublicKeyAlgorithm, cert.SignatureAlgorithm, remedy)
		}
		rest = bytes.TrimSpace(tail)
	}
	return nil
}

func (profile TrustProfile) supportsCertificateAlgorithms(cert *x509.Certificate) bool {
	switch cert.PublicKeyAlgorithm {
	case x509.RSA, x509.ECDSA:
	case x509.MLDSA:
		if !profile.supportsMLDSAPeers() {
			return false
		}
	default:
		return false
	}
	// Match the RSA and ECDSA signature OIDs parsed by Mbed TLS 3.6, including
	// older root self-signatures. This is a parser compatibility check, not a
	// new certificate verification policy. In particular, SHA-224 OIDs are
	// supported by Mbed TLS even though Go reports an unknown signature enum.
	var outer struct {
		TBS       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Signature asn1.BitString
	}
	if rest, err := asn1.Unmarshal(cert.Raw, &outer); err != nil || len(rest) != 0 {
		return false
	}
	switch outer.Algorithm.Algorithm.String() {
	case "1.2.840.113549.1.1.4", // RSA with MD5
		"1.2.840.113549.1.1.5", "1.3.14.3.2.29", // RSA with SHA-1
		"1.2.840.113549.1.1.14", // RSA with SHA-224
		"1.2.840.113549.1.1.11", // RSA with SHA-256
		"1.2.840.113549.1.1.12", // RSA with SHA-384
		"1.2.840.113549.1.1.13", // RSA with SHA-512
		"1.2.840.113549.1.1.10", // RSA-PSS
		"1.2.840.10045.4.1",     // ECDSA with SHA-1
		"1.2.840.10045.4.3.1",   // ECDSA with SHA-224
		"1.2.840.10045.4.3.2",   // ECDSA with SHA-256
		"1.2.840.10045.4.3.3",   // ECDSA with SHA-384
		"1.2.840.10045.4.3.4":   // ECDSA with SHA-512
		return true
	case "2.16.840.1.101.3.4.3.17", // ML-DSA-44
		"2.16.840.1.101.3.4.3.18", // ML-DSA-65
		"2.16.840.1.101.3.4.3.19": // ML-DSA-87
		return profile.supportsMLDSAPeers()
	default:
		return false
	}
}
