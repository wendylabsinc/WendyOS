package meshoperator

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// VerifiedOperator is a primary client-auth credential-path snapshot. It cannot
// be constructed by role strings. Reverify against current local trust after a
// trust/credential change; the snapshot is not an online revocation service.
type VerifiedOperator struct {
	operator  Operator
	leaf      *x509.Certificate
	notBefore time.Time
	notAfter  time.Time
	hashes    [][]byte
}

func (v VerifiedOperator) Operator() Operator   { return v.operator }
func (v VerifiedOperator) NotAfter() time.Time  { return v.notAfter }
func (v VerifiedOperator) NotBefore() time.Time { return v.notBefore }
func (v VerifiedOperator) LeafDER() []byte {
	if v.leaf == nil {
		return nil
	}
	return bytes.Clone(v.leaf.Raw)
}

// CertificateDescriptor uses the existing principal/hash-array shape. Hashes are
// SHA256 of exact DER, not PEM, and descriptors carry no trust by themselves.
type CertificateDescriptor struct {
	Principal string   `json:"principal" cbor:"p"`
	Hashes    [][]byte `json:"hashes" cbor:"h"`
}

func (v VerifiedOperator) Descriptor() CertificateDescriptor {
	d := CertificateDescriptor{Principal: v.operator.Principal(), Hashes: make([][]byte, len(v.hashes))}
	for i, hash := range v.hashes {
		d.Hashes[i] = bytes.Clone(hash)
	}
	return d
}

// VerifyPrimary reuses the established primary certificate-chain verifier with
// locally selected anchors, current time, clientAuth usage and the exact tenant.
// Do not pass a peer-advertised Lite authority twin as a new primary root. A
// P-256 TLS attachment is authenticated independently by its existing policy;
// it does not replace this primary route-proof credential merely by sharing a
// principal. P-256 primary paths remain usable when locally trusted as such.
func VerifyPrimary(chain [][]byte, roots []*x509.Certificate, tenant meshid.Tenant, now time.Time) (VerifiedOperator, error) {
	return VerifyPrimaryWithPolicy(chain, roots, tenant, now, certs.CertificateTimePolicy{})
}

// VerifyPrimaryWithPolicy retains primary trust and operator scope, and uses
// only the receiver's explicitly local leaf-time policy.
func VerifyPrimaryWithPolicy(chain [][]byte, roots []*x509.Certificate, tenant meshid.Tenant, now time.Time, policy certs.CertificateTimePolicy) (VerifiedOperator, error) {
	var result VerifiedOperator
	if !tenant.IsValid() || len(chain) == 0 || len(chain) > 16 || len(roots) == 0 {
		return result, errors.New("missing operator tenant, chain or primary trust")
	}
	parsed := make([]*x509.Certificate, len(chain))
	total := 0
	for i, der := range chain {
		total += len(der)
		if len(der) == 0 || len(der) > 32768 || total > 128*1024 {
			return result, errors.New("oversized operator certificate chain")
		}
		certificate, err := x509.ParseCertificate(bytes.Clone(der))
		if err != nil {
			return result, err
		}
		parsed[i] = certificate
	}
	leaf := parsed[0]
	if leaf.NotAfter.Sub(leaf.NotBefore) > certs.MaxPeerCertificateLifetime {
		return result, errors.New("operator certificate validity window exceeds accepted maximum")
	}
	principal, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok {
		return result, errors.New("operator certificate requires exactly one tenant principal")
	}
	operator, err := ParsePrincipal(principal)
	if err != nil {
		return result, err
	}
	if operator.Tenant() != tenant {
		return result, errors.New("operator credential belongs to another tenant")
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return result, errors.New("operator credential does not permit signing")
	}
	window, err := certs.VerifyPeerCertificateChainWindow(leaf, parsed[1:], roots, x509.ExtKeyUsageClientAuth, now, policy.LeafTime(now, leaf.NotBefore))
	if err != nil {
		return result, err
	}
	if !now.Before(window.NotAfter) {
		return result, errors.New("operator primary path expired")
	}
	hashes := make([][]byte, len(parsed))
	for i, certificate := range parsed {
		hash := sha256.Sum256(certificate.Raw)
		hashes[i] = bytes.Clone(hash[:])
	}
	return VerifiedOperator{operator: operator, leaf: leaf, notBefore: window.NotBefore, notAfter: window.NotAfter, hashes: hashes}, nil
}
