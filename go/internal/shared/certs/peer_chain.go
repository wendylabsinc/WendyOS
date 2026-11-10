package certs

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
)

// TLSKeyPair normalizes the trailing bytes some PKI issuer certificates carry
// outside their ASN.1 sequence, so Go TLS can parse the presented chain.
// Proven self-signed CA roots are omitted; callers retain the original trust
// material. Intermediates and issuers with unsupported signatures are retained.
// leafPEM may itself be a chain bundle; issuers from both inputs are retained
// in presentation order and duplicate certificates are sent only once.
func TLSKeyPair(leafPEM, chainPEM, keyPEM string) (tls.Certificate, error) {
	leaf, err := LeafCertificatePEM(leafPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	bundle := []byte(leaf)
	leafBlock, _ := pem.Decode(bundle) // LeafCertificatePEM guarantees a certificate block.
	embedded, err := ParseCertsFromPEM([]byte(leafPEM))
	if err != nil {
		return tls.Certificate{}, err
	}
	chain, err := ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return tls.Certificate{}, err
	}
	if chainPEM != "" && len(chain) == 0 {
		return tls.Certificate{}, fmt.Errorf("no parseable certificates in TLS issuer chain")
	}
	seen := map[string]bool{string(leafBlock.Bytes): true}
	for _, ca := range append(embedded, chain...) {
		if seen[string(ca.Raw)] {
			continue
		}
		seen[string(ca.Raw)] = true
		if ca.IsCA && bytes.Equal(ca.RawSubject, ca.RawIssuer) && ca.CheckSignatureFrom(ca) == nil {
			continue
		}
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})...)
	}
	return tls.X509KeyPair(bundle, []byte(keyPEM))
}

// VerifyPeerCertificateChain verifies signatures through peer-supplied issuers
// to a locally trusted CA. Peer certificates never become trust anchors.
// This supplements crypto/x509 for ML-DSA signatures. Unsupported constraints
// fail closed; the standard verifier handles such chains when it supports the
// signature algorithms. leafNow preserves the agent's NotBefore clock floor.
func VerifyPeerCertificateChain(leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time) error {
	_, err := VerifyPeerCertificateChainExpiry(leaf, peers, roots, usage, now, leafNow)
	return err
}

// VerifyPeerCertificateChainExpiry applies the same policy as
// VerifyPeerCertificateChain and returns the earliest expiry on its successful
// path. Unused issuers and trust anchors do not affect that path's lifetime.
func VerifyPeerCertificateChainExpiry(leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time) (time.Time, error) {
	window, err := VerifyPeerCertificateChainWindow(leaf, peers, roots, usage, now, leafNow)
	return window.NotAfter, err
}

// VerifyPeerCertificateChainWindow additionally returns the latest NotBefore
// on the exact successful path, for signed claim validity-window checks.
func VerifyPeerCertificateChainWindow(leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time) (CertificateWindow, error) {
	return verifyPeerCertificateChainWindow(leaf, peers, roots, usage, now, leafNow, false)
}

// VerifyLivePeerCertificateChainWindow also checks current issuer-signed CRLs.
// Historical receipts must use the pure VerifyPeerCertificateChainWindow.
func VerifyLivePeerCertificateChainWindow(leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time) (CertificateWindow, error) {
	return verifyPeerCertificateChainWindow(leaf, peers, roots, usage, now, leafNow, true)
}

func VerifyLivePeerCertificateChainExpiry(leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time) (time.Time, error) {
	window, err := VerifyLivePeerCertificateChainWindow(leaf, peers, roots, usage, now, leafNow)
	return window.NotAfter, err
}

func VerifyLivePeerCertificateChain(leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time) error {
	_, err := VerifyLivePeerCertificateChainWindow(leaf, peers, roots, usage, now, leafNow)
	return err
}

func verifyPeerCertificateChainWindow(leaf *x509.Certificate, peers, roots []*x509.Certificate, usage x509.ExtKeyUsage, now, leafNow time.Time, live bool) (CertificateWindow, error) {
	if len(peers) > 32 {
		return CertificateWindow{}, fmt.Errorf("too many peer certificates")
	}
	if leafNow.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return CertificateWindow{}, fmt.Errorf("peer certificate not valid at current time")
	}
	if len(leaf.UnhandledCriticalExtensions) != 0 || !permitsUsage(leaf, usage) {
		return CertificateWindow{}, fmt.Errorf("peer certificate has unsupported extensions or authentication usage")
	}
	revoked := func(window CertificateWindow, child, ca *x509.Certificate) (CertificateWindow, error) {
		if !live {
			return window, nil
		}
		deadline, err := revocation.Default.CheckPath([]*x509.Certificate{child, ca}, now)
		if err != nil {
			return CertificateWindow{}, err
		}
		return window.Intersect(CertificateWindow{NotBefore: window.NotBefore, NotAfter: deadline}), nil
	}
	candidates := append(append([]*x509.Certificate(nil), roots...), peers...)
	seen := map[string]bool{}
	var walk func(*x509.Certificate, int) (CertificateWindow, error)
	walk = func(child *x509.Certificate, depth int) (CertificateWindow, error) {
		if depth >= 8 || seen[string(child.Raw)] {
			return CertificateWindow{}, fmt.Errorf("certificate chain is cyclic or too deep")
		}
		seen[string(child.Raw)] = true
		defer delete(seen, string(child.Raw))
		var lastErr error = &IssuerPathError{Err: fmt.Errorf("peer certificate issuer not found in trusted CA chain")}
		for _, ca := range candidates {
			if !bytes.Equal(ca.RawSubject, child.RawIssuer) {
				continue
			}
			if err := checkPeerCA(ca, usage, now, depth); err != nil {
				lastErr = err
				continue
			}
			var err error
			if oid, oidErr := mldsaCertSigAlgOID(child); oidErr == nil {
				if _, schemeErr := mldsaScheme(oid); schemeErr == nil {
					err = verifyMLDSASignature(ca, child)
				} else {
					err = child.CheckSignatureFrom(ca)
				}
			} else {
				err = oidErr
			}
			if err != nil {
				lastErr = err
				if !errors.Is(err, x509.ErrUnsupportedAlgorithm) {
					lastErr = &IssuerPathError{Err: err}
				}
				continue
			}
			for _, root := range roots {
				if bytes.Equal(root.Raw, ca.Raw) {
					return revoked((CertificateWindow{child.NotBefore, child.NotAfter}).Intersect(CertificateWindow{ca.NotBefore, ca.NotAfter}), child, ca)
				}
			}
			if window, err := walk(ca, depth+1); err == nil {
				return revoked(window.Intersect(CertificateWindow{child.NotBefore, child.NotAfter}), child, ca)
			} else {
				lastErr = err
			}
		}
		return CertificateWindow{}, lastErr
	}
	expires, err := walk(leaf, 0)
	return expires, ClassifyIssuerFailure(err, leaf, peers, roots, usage, now, leafNow)
}

func permitsUsage(cert *x509.Certificate, usage x509.ExtKeyUsage) bool {
	if len(cert.UnknownExtKeyUsage) != 0 {
		return false
	}
	if len(cert.ExtKeyUsage) == 0 {
		return true
	}
	for _, eku := range cert.ExtKeyUsage {
		if eku == usage || eku == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func checkPeerCA(ca *x509.Certificate, usage x509.ExtKeyUsage, now time.Time, depth int) error {
	if !ca.BasicConstraintsValid || !ca.IsCA || ca.KeyUsage != 0 && ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("peer issuer %q is not permitted to sign certificates", ca.Subject.CommonName)
	}
	if now.Before(ca.NotBefore) || now.After(ca.NotAfter) || !permitsUsage(ca, usage) {
		return fmt.Errorf("peer issuer %q is not valid for this time or authentication usage", ca.Subject.CommonName)
	}
	if (ca.MaxPathLen > 0 || ca.MaxPathLenZero) && depth > ca.MaxPathLen {
		return fmt.Errorf("peer issuer %q exceeded its certificate path length", ca.Subject.CommonName)
	}
	if len(ca.UnhandledCriticalExtensions) != 0 {
		return fmt.Errorf("peer issuer %q has unsupported critical extensions", ca.Subject.CommonName)
	}
	// Do not silently ignore constraints the custom ML-DSA path cannot enforce.
	for _, ext := range ca.Extensions {
		if ext.Id.String() == "2.5.29.30" || ext.Id.String() == "2.5.29.33" || ext.Id.String() == "2.5.29.36" || ext.Id.String() == "2.5.29.54" {
			return fmt.Errorf("peer issuer %q has constraints unsupported by ML-DSA verification", ca.Subject.CommonName)
		}
	}
	return nil
}
