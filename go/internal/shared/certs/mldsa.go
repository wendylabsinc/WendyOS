package certs

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
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

// OrgMismatchError is returned by the VerifyConnection callback when the
// server certificate's org ID does not match ExpectedOrgID.
type OrgMismatchError struct {
	Want int32 // client's expected org; 0 if client carries no org identity
	Got  int32 // org found in the server certificate
}

func (e *OrgMismatchError) Error() string {
	return fmt.Sprintf("server certificate belongs to org %d, expected org %d", e.Got, e.Want)
}

// IdentityMismatchError is returned by the VerifyConnection callback when the
// server certificate does not carry the exact asset identity the caller
// required via ServerVerifyOpts.ExpectedIdentity. Unlike OrgMismatchError this
// is never subject to grace mode: a certificate with no Wendy identity is a
// mismatch, because the caller asked for a specific device and got something
// that cannot prove it is that device.
type IdentityMismatchError struct {
	WantOrg   int32
	WantAsset string
	GotOrg    int32  // 0 when the certificate carried no Wendy identity
	GotAsset  string // "" when the certificate carried no Wendy asset identity
}

func (e *IdentityMismatchError) Error() string {
	if e.GotAsset == "" {
		return fmt.Sprintf("device presented no wendy asset identity, expected asset %s in org %d", e.WantAsset, e.WantOrg)
	}
	return fmt.Sprintf("device presented asset %s in org %d, expected asset %s in org %d",
		e.GotAsset, e.GotOrg, e.WantAsset, e.WantOrg)
}

// PinChecker is satisfied by *devicepin.Store. Defined here as an interface
// so shared/certs does not import shared/devicepin (which would be circular).
type PinChecker interface {
	CheckAndUpdate(leaf *x509.Certificate, displayName string) error
}

// BlockingPinError marks the subset of PinChecker errors that must fail the TLS
// handshake. Only a rejection of the PEER — the pinned key changed while the
// pinned certificate was still valid — is one; everything else a pin store can
// fail at is about the store, not the device.
//
// The distinction has to be expressed here rather than by type-switching on
// devicepin's error, because shared/certs cannot import shared/devicepin (that
// is why PinChecker exists at all). Making it an interface the error opts into
// keeps the direction of the dependency intact — devicepin already imports
// certs — and states the rule in the one place that applies it: a PinChecker
// error aborts a verified connection only if it says it is about the peer.
//
// Anything that does NOT implement this is treated as best-effort. A read-only
// config directory or a full disk is an operational fault in local bookkeeping;
// failing every mTLS connection over it would take a fleet offline for a reason
// that has nothing to do with whether the device is who it claims to be.
type BlockingPinError interface {
	error
	// BlockingPinRejection distinguishes this error from a persistence
	// failure. It is a marker; it does nothing.
	BlockingPinRejection()
}

// ServerVerifyOpts configures the server certificate verification callback
// returned by BuildServerVerifyConnection.
type ServerVerifyOpts struct {
	ChainPEM      string     // required: PEM-encoded CA chain for ML-DSA-aware chain verification
	ExpectedOrgID int32      // 0 = accept any org (still extracted for pinning key)
	PinStore      PinChecker // nil = skip pinning
	// ExpectedIdentity, when non-nil, requires the server leaf to carry an
	// "asset" Wendy identity whose org and entity id match it exactly. This is
	// the CLI-side counterpart of agent/mtls.NewClientTLSConfigExpectingPeer:
	// chain validity alone only proves the peer holds a cert from a trusted CA,
	// not that it is the device the caller asked for, so any other same-CA cert
	// could otherwise answer at an mDNS-advertised address.
	//
	// Grace mode does not apply when this is set — a cert with no Wendy
	// identity is a mismatch, not a legacy device to be tolerated.
	ExpectedIdentity *WendyIdentity
	// OnServerIdentity, when non-nil, is called with the server leaf's Wendy
	// identity BEFORE chain verification and the org-mismatch check — so the
	// observed org is captured on every outcome (success, chain-verify failure,
	// org mismatch, and before any client-cert rejection). Best-effort: it is
	// not called when the cert carries no Wendy identity or identity parsing
	// fails, and it never affects the verification result.
	//
	// Because it fires before verification, what it reports is UNTRUSTED — any
	// host can assert any identity. It is for diagnostics only; use
	// OnVerifiedServerIdentity for anything that makes a trust decision.
	OnServerIdentity func(WendyIdentity)
	// OnVerifiedServerIdentity, when non-nil, is called with the server leaf's
	// Wendy identity only AFTER the chain and org checks have both passed —
	// i.e. only for a certificate this verifier accepted. That is what makes it
	// safe to pin against (see config.DevicePin): an impostor presenting an
	// unsigned or cross-org cert never reaches it. Not called when the cert
	// carries no Wendy identity. Like OnServerIdentity it never affects the
	// verification result.
	OnVerifiedServerIdentity func(WendyIdentity)
}

// ParseCertsFromPEM parses all CERTIFICATE blocks from a PEM bundle, handling
// ML-DSA certificates that produce "trailing data" errors from Go's standard
// x509 parser by stripping to the exact outer ASN.1 SEQUENCE.
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

// BuildServerVerifyConnection returns a VerifyConnection callback that:
//  1. Verifies the server cert chain with ML-DSA fallback (see mldsa.go)
//  2. Extracts the server's Wendy org identity (IdentityFromCert)
//  3. Returns OrgMismatchError if opts.ExpectedOrgID != 0 and orgs differ
//  4. Calls opts.PinStore.CheckAndUpdate if PinStore is non-nil, failing the
//     handshake only on a BlockingPinError
//
// InsecureSkipVerify must be true on the tls.Config — this callback is the
// actual verification. Go's built-in verifier cannot parse ML-DSA chain certs
// and there is no TLS hostname over L2CAP or passthrough gRPC targets.
func BuildServerVerifyConnection(opts ServerVerifyOpts) (func(tls.ConnectionState) error, error) {
	if opts.ChainPEM == "" {
		return nil, fmt.Errorf("chain PEM is required to verify device server certificate")
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM([]byte(opts.ChainPEM))
	caCerts, err := ParseCertsFromPEM([]byte(opts.ChainPEM))
	if err != nil {
		return nil, fmt.Errorf("parsing chain PEM: %w", err)
	}
	if len(caCerts) == 0 {
		return nil, fmt.Errorf("no valid CA certificates found in chain PEM")
	}

	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("device presented no TLS certificate")
		}
		leaf := cs.PeerCertificates[0]

		// Best-effort: surface the server's observed Wendy identity before any
		// verification step so callers can report a cross-org mismatch even when
		// the chain fails to verify or the peer later rejects our client cert.
		if opts.OnServerIdentity != nil {
			if id, ok, idErr := IdentityFromCert(leaf); ok && idErr == nil {
				func() {
					defer func() { _ = recover() }()
					opts.OnServerIdentity(id)
				}()
			}
		}

		// Step 1: ML-DSA-aware chain verification.
		intermediates := x509.NewCertPool()
		for _, cert := range cs.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		_, stdErr := leaf.Verify(x509.VerifyOptions{
			Roots:         caPool,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if stdErr != nil {
			sigOID, oidErr := mldsaCertSigAlgOID(leaf)
			if oidErr != nil {
				return stdErr
			}
			if _, schemeErr := mldsaScheme(sigOID); schemeErr != nil {
				return stdErr
			}
			if mldsaErr := verifyMLDSAServerCert(leaf, caCerts); mldsaErr != nil {
				return mldsaErr
			}
		}

		// Step 2: org identity check.
		identity, hasIdentity, idErr := IdentityFromCert(leaf)
		if idErr != nil {
			return fmt.Errorf("extracting server cert identity: %w", idErr)
		}
		// Grace mode: only reject when the server cert carries a Wendy identity AND it
		// belongs to a different org. A cert with no Wendy identity (e.g. a legacy
		// device not yet re-provisioned) is accepted, mirroring the server-side
		// OrgModeGrace behaviour in interceptor/mtls.go.
		if hasIdentity && opts.ExpectedOrgID != 0 && identity.OrgID != opts.ExpectedOrgID {
			return &OrgMismatchError{Want: opts.ExpectedOrgID, Got: identity.OrgID}
		}

		// Step 2b: exact device identity check. Deliberately after the org check
		// so a cross-org impostor still reports OrgMismatchError, whose remedy
		// (fetch that org's cert) differs from this one's (wrong device).
		if opts.ExpectedIdentity != nil {
			if !hasIdentity || identity.EntityType != "asset" {
				return &IdentityMismatchError{
					WantOrg:   opts.ExpectedIdentity.OrgID,
					WantAsset: opts.ExpectedIdentity.EntityID,
					GotOrg:    identity.OrgID,
				}
			}
			if identity.OrgID != opts.ExpectedIdentity.OrgID || identity.EntityID != opts.ExpectedIdentity.EntityID {
				return &IdentityMismatchError{
					WantOrg:   opts.ExpectedIdentity.OrgID,
					WantAsset: opts.ExpectedIdentity.EntityID,
					GotOrg:    identity.OrgID,
					GotAsset:  identity.EntityID,
				}
			}
		}

		// Step 3: SPKI pin check/update. Only a rejection of the peer aborts
		// the handshake — see BlockingPinError. A pin store that cannot record
		// what it just verified has failed at bookkeeping, and dropping an
		// otherwise fully verified connection over that would turn a read-only
		// config directory into a total loss of device access.
		if opts.PinStore != nil && hasIdentity && identity.EntityType == "asset" {
			displayName := leaf.Subject.CommonName
			if displayName == "" {
				displayName = identity.IdentityKey()
			}
			if pinErr := opts.PinStore.CheckAndUpdate(leaf, displayName); pinErr != nil {
				var blocking BlockingPinError
				if errors.As(pinErr, &blocking) {
					return pinErr
				}
			}
		}

		// Step 4: surface the VERIFIED identity. Everything that could reject
		// this certificate has already run, so unlike OnServerIdentity above,
		// callers may base a trust decision on what this reports.
		if opts.OnVerifiedServerIdentity != nil && hasIdentity {
			func() {
				defer func() { _ = recover() }()
				opts.OnVerifiedServerIdentity(identity)
			}()
		}

		return nil
	}, nil
}

// verifyMLDSAServerCert verifies a server leaf cert against trusted CAs using
// ML-DSA signature verification, requiring ExtKeyUsageServerAuth.
func verifyMLDSAServerCert(leaf *x509.Certificate, trustedCAs []*x509.Certificate) error {
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("server certificate not valid at current time (NotBefore=%v NotAfter=%v)", leaf.NotBefore, leaf.NotAfter)
	}

	if len(leaf.ExtKeyUsage) > 0 {
		hasServerAuth := false
		for _, eku := range leaf.ExtKeyUsage {
			if eku == x509.ExtKeyUsageServerAuth || eku == x509.ExtKeyUsageAny {
				hasServerAuth = true
				break
			}
		}
		if !hasServerAuth {
			return fmt.Errorf("device certificate is not valid for server authentication")
		}
	}

	var lastErr error
	foundSubject := false
	for _, ca := range trustedCAs {
		if !bytes.Equal(ca.RawSubject, leaf.RawIssuer) {
			continue
		}
		foundSubject = true
		if now.Before(ca.NotBefore) || now.After(ca.NotAfter) {
			lastErr = fmt.Errorf("CA certificate %q not valid at current time", ca.Subject.CommonName)
			continue
		}
		if !ca.BasicConstraintsValid || !ca.IsCA {
			lastErr = fmt.Errorf("certificate %q is not a CA", ca.Subject.CommonName)
			continue
		}
		if ca.KeyUsage != 0 && ca.KeyUsage&x509.KeyUsageCertSign == 0 {
			lastErr = fmt.Errorf("certificate %q is not permitted to sign certificates", ca.Subject.CommonName)
			continue
		}
		if err := verifyMLDSASignature(ca, leaf); err != nil {
			lastErr = fmt.Errorf("invalid signature from CA %q: %w", ca.Subject.CommonName, err)
			continue
		}
		return nil
	}

	if !foundSubject {
		return fmt.Errorf("device certificate issuer not found in trusted CA pool")
	}
	return lastErr
}
