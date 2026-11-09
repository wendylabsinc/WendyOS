// Package delivery connects the common operator grant to agent transports and
// the shared image store. It owns no image content or independent image index.
package delivery

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type Snapshot struct {
	Provisioned      bool
	Scope            certs.Scope
	Store            *builddelivery.Store
	CertificateChain [][]byte
	Target           meshid.Device
	ImagePrefix      string
}
type Source func() (Snapshot, error)
type Manager struct {
	mu          sync.Mutex
	dir, prefix string
	identity    func() (string, string)
	store       *builddelivery.Store
	trust       [32]byte
	target      meshid.Device
	closed      bool
}

func NewManager(dir, prefix string, identity func() (string, string)) *Manager {
	return &Manager{dir: filepath.Clean(dir), prefix: prefix, identity: identity}
}
func (m *Manager) Get() (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result Snapshot
	if m.closed {
		return result, errors.New("delivery manager closed")
	}
	result.ImagePrefix = m.prefix
	certPEM, chainPEM := m.identity()
	result.Provisioned = certPEM != ""
	if certPEM == "" {
		if m.store != nil {
			m.store.Close()
			m.store = nil
		}
		return result, nil
	}
	certificates, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(certificates) == 0 {
		return result, errors.New("invalid target credential")
	}
	identity, found, err := certs.IdentityFromCert(certificates[0])
	if err != nil {
		return result, err
	}
	// Legacy enrollment retains ambient operator delivery, but cannot mint a
	// modern delegated grant without an exact tenant/device PKI identity.
	if found {
		result.Scope = identity.Scope()
	}
	if !found || identity.Principal == "" || m.prefix == "" {
		if m.store != nil {
			m.store.Close()
			m.store = nil
		}
		return result, nil
	}
	target, err := meshid.DeviceFromPrincipal(identity.Principal)
	if err != nil {
		return result, err
	}
	rawRoots, err := mtls.PeerTrustAnchors(chainPEM)
	if err != nil {
		return result, err
	}
	roots := make([]*x509.Certificate, len(rawRoots))
	var trust bytes.Buffer
	for i, der := range rawRoots {
		roots[i], err = x509.ParseCertificate(der)
		if err != nil {
			return result, err
		}
		trust.Write(der)
	}
	fingerprint := sha256.Sum256(trust.Bytes())
	if m.store != nil && (m.trust != fingerprint || m.target != target) {
		m.store.Close()
		m.store = nil
	}
	if m.store == nil {
		m.store, err = builddelivery.Open(m.dir, roots, target, time.Now)
		if err != nil {
			return result, err
		}
		m.trust = fingerprint
		m.target = target
	}
	chain, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, c := range append(certificates, chain...) {
		if !seen[string(c.Raw)] {
			result.CertificateChain = append(result.CertificateChain, bytes.Clone(c.Raw))
			seen[string(c.Raw)] = true
		}
	}
	result.Store = m.store
	result.Target = target
	result.ImagePrefix = m.prefix
	return result, nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.store != nil {
		m.store.Close()
	}
}
