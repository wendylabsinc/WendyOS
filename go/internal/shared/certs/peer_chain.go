package certs

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"
)

// TLSKeyPair includes the issuer chain, stripping the trailing bytes some PKI
// certificates carry outside their ASN.1 sequence. Those bytes, not ML-DSA
// public keys, prevent Go's TLS stack from parsing the presented chain.
func TLSKeyPair(leafPEM, chainPEM, keyPEM string) (tls.Certificate, error) {
	leaf, err := LeafCertificatePEM(leafPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	bundle := []byte(leaf)
	chain, err := ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return tls.Certificate{}, err
	}
	if chainPEM != "" && len(chain) == 0 {
		return tls.Certificate{}, fmt.Errorf("no parseable certificates in TLS issuer chain")
	}
	for _, ca := range chain {
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
	if len(peers) > 32 {
		return fmt.Errorf("too many peer certificates")
	}
	if leafNow.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("peer certificate not valid at current time")
	}
	if len(leaf.UnhandledCriticalExtensions) != 0 || !permitsUsage(leaf, usage) {
		return fmt.Errorf("peer certificate has unsupported extensions or authentication usage")
	}
	candidates := append(append([]*x509.Certificate(nil), roots...), peers...)
	seen := map[string]bool{}
	var walk func(*x509.Certificate, int) error
	walk = func(child *x509.Certificate, depth int) error {
		if depth >= 8 || seen[string(child.Raw)] {
			return fmt.Errorf("certificate chain is cyclic or too deep")
		}
		seen[string(child.Raw)] = true
		defer delete(seen, string(child.Raw))
		lastErr := fmt.Errorf("peer certificate issuer not found in trusted CA chain")
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
				continue
			}
			for _, root := range roots {
				if bytes.Equal(root.Raw, ca.Raw) {
					return nil
				}
			}
			if err := walk(ca, depth+1); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		return lastErr
	}
	return walk(leaf, 0)
}

func permitsUsage(cert *x509.Certificate, usage x509.ExtKeyUsage) bool {
	if len(cert.ExtKeyUsage) == 0 && len(cert.UnknownExtKeyUsage) == 0 {
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
