package agentmodule

import (
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamcerts"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

// Native peers act as both client and server. The ordinary device verifier
// establishes clientAuth, canonical principal and ML-DSA identity; this adds
// serverAuth on a path valid throughout the same authenticated interval.
func (m *Manager) replicaCertificate(chain [][]byte, w trustedtime.Interval) (wendycert.DeviceIdentity, error) {
	id, err := m.certificates.Verify(chain, w.Earliest, w.Latest)
	if err != nil {
		return id, err
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return wendycert.DeviceIdentity{}, ErrDenied
	}
	server := false
	for _, usage := range leaf.ExtKeyUsage {
		server = server || usage == x509.ExtKeyUsageServerAuth
	}
	if !server {
		return wendycert.DeviceIdentity{}, ErrDenied
	}
	eligible := func(raws [][]byte) ([]*x509.Certificate, error) {
		var out []*x509.Certificate
		for _, raw := range raws {
			c, e := x509.ParseCertificate(raw)
			if e != nil {
				return nil, e
			}
			if !w.Earliest.Before(c.NotBefore) && w.Latest.Before(c.NotAfter) {
				out = append(out, c)
			}
		}
		return out, nil
	}
	roots, err := eligible(m.cfg.Roots)
	if err != nil {
		return wendycert.DeviceIdentity{}, ErrDenied
	}
	peers, err := eligible(chain[1:])
	if err != nil {
		return wendycert.DeviceIdentity{}, ErrDenied
	}
	// These are ML-DSA-65 device leaves already verified by the device verifier.
	// The private upstream verifier preserves the native constraints policy.
	until, err := upstreamcerts.VerifyPeerCertificateChainExpiry(leaf, peers, roots, x509.ExtKeyUsageServerAuth, w.Earliest, w.Earliest)
	if err != nil || !w.Latest.Before(until) {
		return wendycert.DeviceIdentity{}, ErrDenied
	}
	id.ValidUntil = earlier(id.ValidUntil, until)
	return id, nil
}
