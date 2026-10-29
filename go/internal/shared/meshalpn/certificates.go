package meshalpn

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"sync"
	"time"
)

const cachedIssuerLimit = 64
const cachedIssuerBytes = 512 << 10

// Certificates retains CA material as untrusted path candidates. It does not
// add roots or replace the TLS caller's principal, time or certificate policy.
type Certificates struct {
	certificate tls.Certificate
	anchors     map[Fingerprint]bool
	mu          sync.Mutex
	own         []*x509.Certificate
	path        []Fingerprint
	cached      []*x509.Certificate // Bounded FIFO; these never become trust anchors.
	bytes       int
}

func issuerParent(child, parent *x509.Certificate) bool {
	return bytes.Equal(child.RawIssuer, parent.RawSubject) &&
		(len(child.AuthorityKeyId) == 0 || len(parent.SubjectKeyId) == 0 || bytes.Equal(child.AuthorityKeyId, parent.SubjectKeyId))
}

func NewCertificates(certificate tls.Certificate, roots []*x509.Certificate) *Certificates {
	material := &Certificates{certificate: certificate, anchors: make(map[Fingerprint]bool)}
	seen := make(map[Fingerprint]bool)
	add := func(cert *x509.Certificate) {
		hash := Hash(cert.Raw)
		if cert.IsCA && !seen[hash] {
			material.own = append(material.own, cert)
			seen[hash] = true
		}
	}
	var path []*x509.Certificate
	for i := len(certificate.Certificate) - 1; i >= 1; i-- {
		cert, err := x509.ParseCertificate(certificate.Certificate[i])
		if err == nil && cert.IsCA {
			path = append(path, cert)
			add(cert)
		}
	}
	// The TLS presentation normally omits its root. Include that locally held
	// root in the availability prefix, without ever transmitting it as a cert.
	if len(path) > 0 {
		top := path[0]
		for _, root := range roots {
			if issuerParent(top, root) && !bytes.Equal(top.Raw, root.Raw) {
				path = append([]*x509.Certificate{root}, path...)
				break
			}
		}
	} else if len(certificate.Certificate) > 0 {
		leaf, _ := x509.ParseCertificate(certificate.Certificate[0])
		for _, root := range roots {
			if leaf != nil && issuerParent(leaf, root) {
				path = append(path, root)
				break
			}
		}
	}
	for _, root := range roots {
		material.anchors[Hash(root.Raw)] = true
		add(root)
	}
	for _, cert := range path {
		material.path = append(material.path, Hash(cert.Raw))
	}
	return material
}

func (m *Certificates) snapshot(now time.Time) []*x509.Certificate {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reap(now)
	result := append([]*x509.Certificate(nil), m.own...)
	return append(result, m.cached...)
}

func (m *Certificates) reap(now time.Time) {
	kept := m.cached[:0]
	for _, cert := range m.cached {
		if now.Before(cert.NotAfter) {
			kept = append(kept, cert)
		} else {
			m.bytes -= len(cert.Raw)
		}
	}
	clear(m.cached[len(kept):])
	m.cached = kept
}

func (m *Certificates) Remember(chain [][]byte, now time.Time) {
	if len(chain) < 2 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reap(now)
	known := make(map[Fingerprint]bool, len(m.own)+len(m.cached))
	for _, cert := range m.own {
		known[Hash(cert.Raw)] = true
	}
	for _, cert := range m.cached {
		known[Hash(cert.Raw)] = true
	}
	for _, raw := range chain[1:] {
		hash := Hash(raw)
		if known[hash] || len(raw) > cachedIssuerBytes {
			continue
		}
		cert, err := x509.ParseCertificate(bytes.Clone(raw))
		if err != nil || !cert.IsCA || !now.Before(cert.NotAfter) || now.Before(cert.NotBefore) {
			continue
		}
		for len(m.cached) > 0 && (len(m.cached) >= cachedIssuerLimit || m.bytes+len(raw) > cachedIssuerBytes) {
			m.bytes -= len(m.cached[0].Raw)
			m.cached[0] = nil
			m.cached = m.cached[1:]
		}
		m.cached = append(m.cached, cert)
		m.bytes += len(raw)
		known[hash] = true
	}
}

// Complete only missing parent candidates. Every supplied certificate remains
// untrusted; the ordinary verifier still builds a valid path to its original
// configured anchors. Unrelated cached CAs are not appended to each handshake.
func (m *Certificates) Candidates(wire [][]byte, now time.Time) [][]byte {
	return m.candidatesFrom(wire, m.snapshot(now))
}

func (m *Certificates) candidatesFrom(wire [][]byte, available []*x509.Certificate) [][]byte {
	result := append([][]byte(nil), wire...)
	seen := make(map[Fingerprint]bool, len(wire)+len(available))
	for _, raw := range wire {
		seen[Hash(raw)] = true
	}
	for index := 0; index < len(result) && len(result) < 32; index++ {
		child, err := x509.ParseCertificate(result[index])
		if err != nil {
			continue
		}
		for _, parent := range available {
			hash := Hash(parent.Raw)
			if !seen[hash] && !m.anchors[hash] && issuerParent(child, parent) {
				result = append(result, parent.Raw)
				seen[hash] = true
				if len(result) == 32 {
					break
				}
			}
		}
	}
	return result
}

// Clear forgets foreign CA metadata. Active handshake snapshots keep their DER
// alive until verification has completed; immutable own material is retained.
func (m *Certificates) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = nil
	m.bytes = 0
}
