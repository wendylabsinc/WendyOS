package localmesh

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// Credentials reuses the agent's provisioned signer and ML-DSA-aware trust
// verifier. Reconstruct this object (and the session/cache) when provisioning or
// trust changes. No mesh-specific private key or trust-on-first-use is created.
type Credentials struct {
	Org, Asset     int32
	Certificate    tls.Certificate
	Signer         crypto.Signer
	Verify         IdentityVerifier
	peerTLS        func(int32) (*tls.Config, error)
	peerTLSTickets func(int32, string, string) (*tls.Config, error)
}

func NewCredentials(org, asset int32, certPEM, chainPEM, keyPEM string) (*Credentials, error) {
	if _, _, err := Addresses(org, asset); err != nil {
		return nil, err
	}
	base, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, nil, strconv.Itoa(int(asset)))
	if err != nil {
		return nil, err
	}
	if len(base.Certificates) != 1 {
		return nil, errors.New("missing local-mesh identity")
	}
	cert := base.Certificates[0]
	if len(cert.Certificate) == 0 {
		return nil, errors.New("missing local-mesh leaf certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	self, found, err := certs.IdentityFromCert(leaf)
	if err != nil || !found || self.EntityType != certs.EntityAsset || self.OrgID != org || self.EntityID != strconv.Itoa(int(asset)) {
		return nil, errors.New("local-mesh identity does not match enrolled org and asset")
	}
	if err = base.VerifyPeerCertificate(cert.Certificate, nil); err != nil {
		return nil, err
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("identity key cannot sign")
	}
	verifyChain, err := mtls.NewClientTLSConfig(certPEM, chainPEM, keyPEM, nil)
	if err != nil {
		return nil, err
	}
	trustChain, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return nil, err
	}
	verify := func(chain [][]byte, now time.Time) (Identity, error) {
		if len(chain) == 0 || len(chain) > 16 {
			return Identity{}, errors.New("invalid identity bundle")
		}
		if err := verifyChain.VerifyPeerCertificate(chain, nil); err != nil {
			return Identity{}, err
		}
		leaf, err := x509.ParseCertificate(chain[0])
		if err != nil {
			return Identity{}, err
		}
		id, found, err := certs.IdentityFromCert(leaf)
		if err != nil {
			return Identity{}, err
		}
		if !found || id.EntityType != "asset" || id.OrgID != org || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return Identity{}, errors.New("invalid local-mesh origin")
		}
		peer, err := strconv.ParseInt(id.EntityID, 10, 32)
		if err != nil {
			return Identity{}, err
		}
		if _, _, err = Addresses(org, int32(peer)); err != nil {
			return Identity{}, err
		}
		expires := leaf.NotAfter
		for _, issuer := range trustChain {
			if issuer.NotAfter.Before(expires) {
				expires = issuer.NotAfter
			}
		}
		return Identity{org, int32(peer), expires}, nil
	}
	// The enrolled PEM material and ML-DSA-aware trust chain are immutable for
	// this Credentials incarnation. Parse each peer's pinned TLS config once;
	// callers receive a clone so carrier-specific ALPN and settings cannot
	// change another connection. Provisioning/trust refresh creates a new
	// Credentials object and therefore a fresh cache.
	var peerMu sync.Mutex
	peerConfigs := make(map[int32]*tls.Config)
	type ticketScope struct {
		cache tls.ClientSessionCache
		store *TicketStore
	}
	ticketCaches := make(map[string]ticketScope)
	credentials := &Credentials{Org: org, Asset: asset, Certificate: cert, Signer: signer, Verify: verify, peerTLS: func(peer int32) (*tls.Config, error) {
		peerMu.Lock()
		defer peerMu.Unlock()
		if cached := peerConfigs[peer]; cached != nil {
			return cached.Clone(), nil
		}
		c, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, nil, strconv.Itoa(int(peer)))
		if err != nil {
			return nil, err
		}
		c.MinVersion = tls.VersionTLS13
		c.MaxVersion = tls.VersionTLS13
		c.NextProtos = []string{LinkALPN}
		c.ClientAuth = tls.RequireAnyClientCert
		c.ClientSessionCache = nil
		c.SessionTicketsDisabled = true
		if len(peerConfigs) >= 128 {
			for old := range peerConfigs {
				delete(peerConfigs, old)
				break
			}
		}
		peerConfigs[peer] = c
		return c.Clone(), nil
	}}
	credentials.peerTLSTickets = func(peer int32, alpn, transport string) (*tls.Config, error) {
		if alpn == "" || transport == "" {
			return nil, errors.New("mesh ticket scope needs ALPN and transport")
		}
		cfg, err := credentials.PeerTLS(peer)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%d/%d:%s/%d:%s", peer, len(alpn), alpn, len(transport), transport)
		peerMu.Lock()
		scope, found := ticketCaches[key]
		if !found {
			if len(ticketCaches) >= 128 {
				for old := range ticketCaches {
					delete(ticketCaches, old)
					break
				}
			}
			scope.cache = tls.NewLRUClientSessionCache(2)
			scope.store = NewTicketStore()
			ticketCaches[key] = scope
		}
		peerMu.Unlock()
		cfg.NextProtos = []string{alpn}
		cfg.ServerName = fmt.Sprintf("asset-%d.mesh.wendy.invalid", peer)
		cfg.ClientSessionCache = scope.cache
		// NAN and configured QUIC create a fresh listener per link. The
		// shared store keeps compact server ticket handles valid across links.
		scope.store.Configure(cfg)
		// PeerTLS's VerifyConnection rechecks the current chain, org, peer
		// asset and expiry even when Go skips Certificate on resumption.
		return cfg, nil
	}
	return credentials, nil
}

func (c *Credentials) PeerTLS(peer int32) (*tls.Config, error) {
	if peer == c.Asset {
		return nil, errors.New("self link")
	}
	if _, _, err := Addresses(c.Org, peer); err != nil {
		return nil, err
	}
	return c.peerTLS(peer)
}

// PeerTLSWithTickets enables in-memory TLS 1.3 resumption for a specific peer,
// ALPN and transport. QUIC and raw TLS never share tickets. The cache is
// discarded with this Credentials instance when provisioning rotates.
func (c *Credentials) PeerTLSWithTickets(peer int32, alpn, transport string) (*tls.Config, error) {
	if c == nil || c.peerTLSTickets == nil {
		return nil, errors.New("missing mesh credential ticket scope")
	}
	return c.peerTLSTickets(peer, alpn, transport)
}
