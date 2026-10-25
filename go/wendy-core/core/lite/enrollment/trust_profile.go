package liteenroll

import (
	"bytes"
	"encoding/pem"
	"fmt"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

// TrustProfile describes the certificate algorithms advertised by the board.
// Its zero value preserves the mbedTLS restrictions used by older firmware.
type TrustProfile struct {
	Inspector            platform.LiteCertificateInspector
	mldsa                bool
	existingDeviceIssuer bool
}

// TrustProfileFromChallenge permits ML-DSA only when firmware explicitly reports
// that its optional verifier is enabled. Missing or unknown legacy capabilities
// must not infer support from the enrolling computer's crypto implementation.
func TrustProfileFromChallenge(challenge *litepb.WendyComEnrollmentChallenge, inspector platform.LiteCertificateInspector) TrustProfile {
	return TrustProfile{Inspector: inspector, mldsa: challenge.GetMldsaVerificationSupported()}
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
	return cloud.EnrollmentChainP256
}

func validateTrustBundleForProfile(data []byte, profile TrustProfile) error {
	if err := validateTrustBundle(data, profile.Inspector); err != nil {
		return err
	}
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		block, tail := pem.Decode(rest)
		cert, err := profile.Inspector.InspectCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("invalid certificate: %w", err)
		}
		if !profile.supportsCertificateAlgorithms(cert) {
			if profile.mldsa {
				return fmt.Errorf("CA public key %s and signature %s are not supported by the board's advertised verifier; use a certificate chain with RSA, ECDSA or ML-DSA algorithms; no cloud asset was reserved", cert.PublicKeyAlgorithm, cert.SignatureAlgorithm)
			}
			remedy := "the PKI deployment needs an mbedTLS-compatible certificate chain using RSA or ECDSA"
			if cert.PublicKeyAlgorithm == "ML-DSA" || cert.SignatureAlgorithm == "ML-DSA-44" || cert.SignatureAlgorithm == "ML-DSA-65" || cert.SignatureAlgorithm == "ML-DSA-87" {
				remedy += ", or firmware built with CONFIG_WENDY_PKI_WOLFSSL enabled for ML-DSA verification"
			}
			return fmt.Errorf("CA public key %s and signature %s are not supported by the current Wendy Lite mbedTLS firmware; %s; no cloud asset was reserved", cert.PublicKeyAlgorithm, cert.SignatureAlgorithm, remedy)
		}
		rest = bytes.TrimSpace(tail)
	}
	return nil
}

func (profile TrustProfile) supportsCertificateAlgorithms(cert *platform.LiteCertificate) bool {
	switch cert.PublicKeyAlgorithm {
	case "RSA", "ECDSA":
	case "ML-DSA":
		if !profile.mldsa {
			return false
		}
	default:
		return false
	}
	switch cert.SignatureOID {
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
		return profile.mldsa
	default:
		return false
	}
}

// ValidateTrustBundle checks explicit CA material against firmware capabilities.
func ValidateTrustBundle(data []byte, profile TrustProfile) error {
	return validateTrustBundleForProfile(data, profile)
}

// ChainAlgorithm selects the enrollment authority advertised by the firmware.
func (profile TrustProfile) ChainAlgorithm() string { return profile.chainAlgorithm() }
