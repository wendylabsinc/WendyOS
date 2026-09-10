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
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/timefmt"
	"go.uber.org/zap"
)

// maxCertLifetime is the maximum accepted certificate validity window.
// Certificates valid for longer than this are rejected because they cannot be
// promptly revoked: Go's crypto/tls does not fetch CRL distribution points
// during the TLS handshake, and doing so from server code introduces SSRF
// vectors, cache-poisoning risk, and availability dependencies.
//
// The value is 732 days (2 × 365 + 2) to cover any real-world "2-year"
// certificate whose validity window includes a leap-year Feb 29 (max 731 days).
const maxCertLifetime = (2*365 + 2) * 24 * time.Hour

// maxClockSkewTolerance is the maximum amount by which the NotBefore floor may
// be advanced to accommodate a cert issued after provisioning. When the device
// clock is behind notBeforeFloor, effectiveNow is capped at
// notBeforeFloor+maxClockSkewTolerance so that a cert with NotBefore far in the
// future (e.g. years ahead) cannot be accepted by a device with a stuck clock.
const maxClockSkewTolerance = 24 * time.Hour

// checkRevocation enforces that leaf was issued with a validity window short
// enough that a compromised credential expires within maxCertLifetime even
// without an explicit CRL/OCSP revocation check.
func checkRevocation(leaf *x509.Certificate) error {
	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	if lifetime > maxCertLifetime {
		return fmt.Errorf("certificate lifetime %v exceeds maximum %v; reissue with a shorter validity window", lifetime, maxCertLifetime)
	}
	return nil
}

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
			// ML-DSA certs produce "trailing data" because pki-core appends extra
			// bytes after the outer SEQUENCE. Strip them by reading exactly one
			// ASN.1 element and re-parsing.
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

// maxTime returns the later of two times. Used to apply a NotBefore floor when
// the device clock has not yet been synchronised via NTP.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// effectiveVerificationTime returns the clock used for NotBefore checks.
// effectiveNow applies the NotBefore floor; when the device clock is behind
// notBeforeFloor (NTP not yet synced), it additionally advances up to the
// cert's NotBefore so a cert issued just after provisioning is not spuriously
// rejected — capped at notBeforeFloor+maxClockSkewTolerance so a cert whose
// NotBefore is further in the future is still rejected on a stuck clock.
// Shared by the full ML-DSA verifier and the session-ticket re-check
// (session_ticket.go) so the two can never drift apart.
func effectiveVerificationTime(realNow, notBeforeFloor, certNotBefore time.Time) time.Time {
	effectiveNow := maxTime(realNow, notBeforeFloor)
	if realNow.Before(notBeforeFloor) {
		advanced := certNotBefore
		if cap := notBeforeFloor.Add(maxClockSkewTolerance); advanced.After(cap) {
			advanced = cap
		}
		effectiveNow = maxTime(effectiveNow, advanced)
	}
	return effectiveNow
}

// buildVerifyPeerCertificate returns a VerifyPeerCertificate callback that
// handles both standard (RSA/ECDSA) and ML-DSA-signed certificate chains.
// logger may be nil, in which case no logging is performed.
// notBeforeFloor is used as a lower bound on the current time for NotBefore
// checks: if the device clock is behind the floor (e.g. RTC reset to epoch
// before NTP sync), effectiveNow = max(time.Now(), notBeforeFloor) is used
// so that certs issued at provisioning time are still accepted. Pass a zero
// time.Time to disable the floor.
func buildVerifyPeerCertificate(caPool *x509.CertPool, caCerts []*x509.Certificate, logger *zap.Logger, notBeforeFloor time.Time) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("no client certificate presented")
		}

		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parsing client certificate: %w", err)
		}

		realNow := time.Now()
		effectiveNow := effectiveVerificationTime(realNow, notBeforeFloor, leaf.NotBefore)

		// Pre-reject expired certs before any further processing. The floor must
		// not mask real-time expiry: checking here with realNow eliminates any
		// TOCTOU window between the clock snapshot and the verification call.
		if realNow.After(leaf.NotAfter) {
			expiredErr := fmt.Errorf("certificate expired (notAfter=%v)", leaf.NotAfter)
			// The floor is irrelevant once the cert is expired against the real
			// clock: pass realNow so the rejection log reflects the actual clock
			// and never mistakes an expired cert for a clock-skew case.
			logCertRejection(logger, leaf, expiredErr, realNow, realNow)
			return expiredErr
		}

		// Build an intermediates pool from the rest of the chain presented by the client.
		intermediates := x509.NewCertPool()
		var peerCAs []*x509.Certificate
		for _, rawCert := range rawCerts[1:] {
			if intermediate, parseErr := x509.ParseCertificate(rawCert); parseErr == nil {
				intermediates.AddCert(intermediate)
				peerCAs = append(peerCAs, intermediate)
			}
		}

		// Try standard Go verification first (handles RSA/ECDSA chains).
		// CurrentTime uses effectiveNow so that certs issued at provisioning are
		// accepted when the device clock has not yet synced via NTP.
		opts := x509.VerifyOptions{
			Roots:         caPool,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			CurrentTime:   effectiveNow,
		}
		// Try standard Go verification first (handles RSA/ECDSA chains).
		stdErr := func() error { _, e := leaf.Verify(opts); return e }()
		if stdErr != nil {
			// Only fall back to ML-DSA verification when the leaf cert uses an ML-DSA
			// signature algorithm; for all other failures return the standard error.
			sigOID, oidErr := certSigAlgOID(leaf)
			if oidErr != nil {
				logCertRejection(logger, leaf, stdErr, realNow, effectiveNow)
				return stdErr
			}
			if _, schemeErr := mldsaScheme(sigOID); schemeErr != nil {
				logCertRejection(logger, leaf, stdErr, realNow, effectiveNow)
				return stdErr
			}
			mldsaErr := verifyMLDSAClientCert(leaf, caCerts, realNow, effectiveNow)
			if mldsaErr != nil && len(peerCAs) > 0 {
				mldsaErr = certs.VerifyPeerCertificateChain(leaf, peerCAs, caCerts, x509.ExtKeyUsageClientAuth, realNow, effectiveNow)
			}
			if mldsaErr != nil {
				logCertRejection(logger, leaf, mldsaErr, realNow, effectiveNow)
				return mldsaErr
			}
		}

		// Single revocation check after successful verification by either path.
		// checkRevocation only inspects the leaf's validity window, so it needs no
		// CA list and cannot be bypassed by either the standard or ML-DSA branch.
		if revErr := checkRevocation(leaf); revErr != nil {
			logCertRejection(logger, leaf, revErr, realNow, effectiveNow)
			return revErr
		}
		return nil
	}
}

// logCertRejection logs a WARN when a client cert is rejected, with a clock
// skew hint when the error indicates the cert is not yet valid. realNow is the
// device clock; effectiveNow is the floored clock verification actually used, and
// the two differ by however far the floor carried the device.
func logCertRejection(logger *zap.Logger, leaf *x509.Certificate, err error, realNow, effectiveNow time.Time) {
	if logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("subject", leaf.Subject.CommonName),
		zap.Time("notBefore", leaf.NotBefore),
		zap.Time("notAfter", leaf.NotAfter),
		zap.Time("clock", realNow),
		zap.Error(err),
	}
	msg := "mTLS client certificate rejected"
	// Only hint at clock skew when the verification clock is behind the cert's
	// NotBefore. String-matching on the error text would also fire for expired
	// certs, pointing operators at the wrong remediation (NTP sync won't help an
	// expired cert).
	if effectiveNow.Before(leaf.NotBefore) {
		// Report the device clock's own lag, which is what the operator has to fix.
		// effectiveNow may already have been carried forward by the floor, so the
		// gap verification failed on understates how far back the clock is.
		clockLag := leaf.NotBefore.Sub(realNow)
		msg += ": certificate not yet valid — this device's clock is " +
			timefmt.Skew(clockLag) +
			" behind the time the certificate was issued. Check the clock (timedatectl status);" +
			" push a verified time from a host on this network with: wendy device sync-time"
		fields = append(fields,
			zap.Duration("clockLag", clockLag),
			zap.Duration("verifyLag", leaf.NotBefore.Sub(effectiveNow)))
	}
	logger.Warn(msg, fields...)
}

// verifyMLDSAClientCert verifies a client leaf cert against the trusted CA certs
// using ML-DSA signature verification. It checks validity and that the leaf was
// signed by a trusted CA. effectiveNow is max(realNow, notBeforeFloor) and is
// used only for the leaf NotBefore check so that certs issued at provisioning are
// accepted when the device clock has not yet synced via NTP. realNow is the
// unmodified clock snapshot; all NotAfter and CA NotBefore checks use it so the
// floor cannot mask expiry or make an immature CA appear valid.
func verifyMLDSAClientCert(leaf *x509.Certificate, trustedCAs []*x509.Certificate, realNow, effectiveNow time.Time) error {
	if effectiveNow.Before(leaf.NotBefore) || realNow.After(leaf.NotAfter) {
		return fmt.Errorf("certificate not valid at current time (NotBefore=%v NotAfter=%v)", leaf.NotBefore, leaf.NotAfter)
	}

	// Mirror the standard verifier's EKU check: the cert must allow clientAuth
	// (or be unrestricted, i.e. have no ExtKeyUsage set).
	if len(leaf.ExtKeyUsage) > 0 {
		hasClientAuth := false
		for _, eku := range leaf.ExtKeyUsage {
			if eku == x509.ExtKeyUsageClientAuth || eku == x509.ExtKeyUsageAny {
				hasClientAuth = true
				break
			}
		}
		if !hasClientAuth {
			return fmt.Errorf("certificate is not valid for client authentication")
		}
	}

	var lastErr error
	foundSubject := false
	for _, ca := range trustedCAs {
		if !bytes.Equal(ca.RawSubject, leaf.RawIssuer) {
			continue
		}
		foundSubject = true
		if realNow.Before(ca.NotBefore) || realNow.After(ca.NotAfter) {
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
		return fmt.Errorf("client certificate issuer not found in trusted CA pool")
	}
	return lastErr
}
