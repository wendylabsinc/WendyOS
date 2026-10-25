package platform

import (
	"context"
	"time"
)

// LegacyProofSigner supplies the legacy Cloud ingress's P-256 proof encoding.
// The host hashes message with SHA-256 and returns an ASN.1 ECDSA signature.
// The key must match the certificate validated by the host. No key enters Core.
type LegacyProofSigner interface {
	SignLegacyProof(ctx context.Context, message []byte) ([]byte, error)
}

// ProofCertificate carries public certificate facts parsed by the host.
// A principal is present only for the certificate's sole tenant SPIFFE identity.
type ProofCertificate struct {
	DER                 []byte
	Principal           string
	NotBefore, NotAfter time.Time
}

// ProofVerifier provides certificate and raw ML-DSA verification primitives.
// It performs no authorization, networking, request mutation or private-key work.
// VerifyMLDSA accepts raw public-key bytes when certificate is false, or a DER
// certificate when true, and refuses algorithm/key mismatches (65 or 87).
// Hosts honor cancellation. Trust in a raw Cloud key must be established by the
// caller over authenticated HTTPS, or protected local admission evidence.
type ProofVerifier interface {
	ParseProofCertificate(ctx context.Context, der []byte) (ProofCertificate, error)
	VerifyMLDSA(ctx context.Context, key, message, signature []byte, algorithm string, certificate bool) error
}

// CompletionSigner signs a canonical reset receipt. The host preserves the
// device's existing algorithm (pure ML-DSA, or SHA-256 ECDSA/RSA).
type CompletionSigner interface {
	SignCompletion(ctx context.Context, body []byte) ([]byte, error)
}

// RevocationEvidence contains issuer-authenticated OCSP facts for an exact leaf.
// Identity, signature, issuer and responder binding have already been checked.
type RevocationEvidence struct {
	Revoked, Delegated                            bool
	ThisUpdate, ProducedAt, RevokedAt, NextUpdate time.Time
}

// ReceiptVerifier supplies public certificate and OCSP verification primitives.
// CertificateIssuer must return an issuer that signed leaf, from the supplied
// PEM chain. VerifyCompletionSignature uses the certificate's actual key type.
// These operations authenticate bytes, never establish remote root/key trust.
type ReceiptVerifier interface {
	ProofVerifier
	CertificateIssuer(ctx context.Context, leaf []byte, chain string) ([]byte, error)
	VerifyCompletionSignature(ctx context.Context, certificate, body, signature []byte) error
	RevocationEvidence(ctx context.Context, raw, leaf, issuer []byte) (RevocationEvidence, error)
}
