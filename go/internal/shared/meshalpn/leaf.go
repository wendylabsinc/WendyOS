package meshalpn

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"time"
)

const (
	leafMarker      = "\x00WCL\x01"
	classicalClient = byte(1)
	regularClient   = byte(2)
)

// The offer names the regular sender's immediate issuing CA. A selection
// elides both presentations to their leaves. TLS still verifies each peer.
func leafOffer(protocol string, kind byte, issuer Fingerprint) string {
	return protocol + leafMarker + string([]byte{kind}) + string(issuer[:])
}

func decodeLeafOffer(protocol string) (string, byte, Fingerprint) {
	var issuer Fingerprint
	base, suffix, ok := strings.Cut(protocol, leafMarker)
	if !ok || base == "" || len(protocol) > 255 || len(suffix) != 1+len(issuer) {
		return protocol, 0, issuer
	}
	kind := suffix[0]
	if kind != classicalClient && kind != regularClient {
		return protocol, 0, issuer
	}
	copy(issuer[:], suffix[1:])
	return base, kind, issuer
}

func certificateIssuer(certificate tls.Certificate, available []*x509.Certificate) (Fingerprint, bool) {
	if len(certificate.Certificate) == 0 {
		return Fingerprint{}, false
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return Fingerprint{}, false
	}
	// Use the exact issuing certificate from this presentation when available.
	if len(certificate.Certificate) > 1 {
		if issuer, err := x509.ParseCertificate(certificate.Certificate[1]); err == nil &&
			issuerParent(leaf, issuer) && leaf.CheckSignatureFrom(issuer) == nil {
			return Hash(issuer.Raw), true
		}
	}
	for _, issuer := range available {
		if issuerParent(leaf, issuer) && leaf.CheckSignatureFrom(issuer) == nil {
			return Hash(issuer.Raw), true
		}
	}
	return Fingerprint{}, false
}

func leafCertificate(certificate tls.Certificate) *tls.Certificate {
	if len(certificate.Certificate) > 1 {
		certificate.Certificate = certificate.Certificate[:1:1]
	}
	return &certificate
}

// SetLiteIssuers installs externally validated, tenant-scoped CA candidates.
// The validity window is for the complete issuer path, including its anchor.
func (m *Certificates) SetLiteIssuers(issuers []*x509.Certificate, notBefore, notAfter time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.liteIssuers = append([]*x509.Certificate(nil), issuers...)
	m.liteNotBefore, m.liteNotAfter = notBefore, notAfter
}

func (m *Certificates) acceptsLite(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.validLiteIssuers(now)
}

func (m *Certificates) validLiteIssuers(now time.Time) bool {
	return len(m.liteIssuers) != 0 && !now.Before(m.liteNotBefore) && now.Before(m.liteNotAfter)
}

// LiteCandidates is the tenant-provisioned path used for signed records. It
// never adds peer-learned cache entries or changes the advertised record bytes.
func (m *Certificates) LiteCandidates(wire [][]byte, now time.Time) [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.validLiteIssuers(now) {
		return wire
	}
	return m.candidatesFrom(wire, m.liteIssuers)
}

func (m *Certificates) liteServerOffer(protocols, offered []string, available []*x509.Certificate, now time.Time) string {
	if !m.acceptsLite(now) {
		return ""
	}
	issuer, ok := certificateIssuer(m.certificate, available)
	if !ok {
		return ""
	}
	for _, protocol := range protocols {
		for _, offer := range offered {
			base, kind, hash := decodeLeafOffer(offer)
			if base == protocol && kind == classicalClient && hash == issuer {
				return offer
			}
		}
	}
	return ""
}
