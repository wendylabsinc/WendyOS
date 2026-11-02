package localmesh

import (
	"container/list"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// Credentials reuses the agent's provisioned signer and ML-DSA-aware trust
// verifier. Desktop callers reuse an organization operator certificate instead.
// Reconstruct this object (and the session/cache) when provisioning or
// trust changes. No mesh-specific private key or trust-on-first-use is created.
//
// Device identities use the leaf's device principal
// "spiffe://wendy.sh/tenant/<tenant-uuid>/device/<device-uuid>". Nothing here
// carries an organization number, an asset number, or a hash of either. Cloud
// uses the same device UUID for asset_id; certificate and tenant verification
// still establish the complete authenticated principal.
type Credentials struct {
	primaryRoots   []*x509.Certificate
	Device         meshid.Device
	Certificate    tls.Certificate
	Signer         crypto.Signer
	Verify         IdentityVerifier
	peerTLS        func(meshid.Device) (*tls.Config, error)
	peerTLSTickets func(meshid.Device, string, string) (*tls.Config, error)
	ticketOnce     sync.Once
	ticketMemory   *meshSessionCache
	issuerMaterial *meshalpn.Certificates
}

// SupportsPrimaryOperators reports whether this enrolled identity has locally
// selected primary trust anchors. Discovery does not supply these anchors.
func (c *Credentials) SupportsPrimaryOperators() bool { return c != nil && len(c.primaryRoots) > 0 }

// VerifyPrimaryOperator preserves the separate operator role and primary trust
// policy when another carrier accepts the ordinary operator attachment ALPN.
func (c *Credentials) VerifyPrimaryOperator(chain [][]byte, now time.Time) (meshoperator.VerifiedOperator, error) {
	if c == nil {
		return meshoperator.VerifiedOperator{}, errors.New("missing enrolled credentials")
	}
	return meshoperator.VerifyPrimary(c.knownIssuerCandidates(chain, now), c.primaryRoots, c.Device.Tenant(), now)
}

// NewCredentials loads the provisioned identity and derives the local-mesh
// identity from its leaf. Certificates without a device principal — asset, user,
// service, or legacy numeric principals — are refused outright; there is no
// fallback identity.
func NewCredentials(certPEM, chainPEM, keyPEM string) (*Credentials, error) {
	return newCredentials(certPEM, chainPEM, keyPEM, uuid.Nil, false)
}

// NewOperatorCredentials is an explicit primary operator role. It never enrolls
// the host or changes the principal into a device. The instance survives renewal.
func NewOperatorCredentials(certPEM, chainPEM, keyPEM string, instance uuid.UUID) (*Credentials, error) {
	return newCredentials(certPEM, chainPEM, keyPEM, instance, true)
}

func newCredentials(certPEM, chainPEM, keyPEM string, instance uuid.UUID, operator bool) (*Credentials, error) {
	base, err := mtls.NewClientTLSConfig(certPEM, chainPEM, keyPEM, nil)
	if err != nil {
		return nil, err
	}
	if len(base.Certificates) != 1 {
		return nil, errors.New("missing wendy net identity")
	}
	cert := base.Certificates[0]
	if len(cert.Certificate) == 0 || len(cert.Certificate) > 16 {
		return nil, errors.New("invalid wendy net identity bundle")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	self, err := meshid.PeerFromCert(leaf)
	if err != nil {
		return nil, err
	}
	if self.IsOperator() != operator {
		return nil, errors.New("mesh credential role does not match constructor")
	}
	if operator {
		self, err = self.WithInstance(instance)
		if err != nil {
			return nil, err
		}
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("identity key cannot sign")
	}
	verifyChain, err := mtls.NewPeerCertificateVerifier(chainPEM, nil, time.Time{})
	if err != nil {
		return nil, err
	}
	// Mesh requires a verified issuer path for its own identity too. The
	// general mTLS loopback shortcut must not extend an expired local issuer.
	if _, err := verifyChain(cert.Certificate, time.Now()); err != nil && !operator {
		return nil, err
	}
	anchors, err := mtls.PeerTrustAnchors(chainPEM)
	if err != nil {
		return nil, err
	}
	primaryRoots := make([]*x509.Certificate, 0, len(anchors))
	for _, der := range anchors {
		root, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		primaryRoots = append(primaryRoots, root)
	}
	material := meshalpn.NewCertificates(cert, primaryRoots)
	verify := func(chain [][]byte, now time.Time) (Identity, error) {
		if len(chain) == 0 || len(chain) > 16 {
			return Identity{}, errors.New("invalid identity bundle")
		}
		// Keep rejecting malformed bundle entries even when they are not
		// needed by the selected path.
		for _, der := range chain[1:] {
			if _, err := x509.ParseCertificate(der); err != nil {
				return Identity{}, err
			}
		}
		peerLeaf, err := x509.ParseCertificate(chain[0])
		if err != nil {
			return Identity{}, err
		}
		device, err := meshid.PeerFromCert(peerLeaf)
		if err != nil {
			return Identity{}, err
		}
		if device.Tenant() != self.Tenant() {
			return Identity{}, errors.New("wendy net peer is outside the tenant")
		}
		if now.Before(peerLeaf.NotBefore) || !now.Before(peerLeaf.NotAfter) {
			return Identity{}, errors.New("wendy net identity is not currently valid")
		}
		var expires time.Time
		if device.IsOperator() {
			verified, err := meshoperator.VerifyPrimary(chain, primaryRoots, self.Tenant(), now)
			if err != nil {
				return Identity{}, err
			}
			expires = verified.NotAfter()
		} else {
			expires, err = verifyChain(chain, now)
			if err != nil {
				return Identity{}, err
			}
		}
		material.Remember(chain, now)
		return Identity{Device: device, NotAfter: expires}, nil
	}
	if _, err := verify(cert.Certificate, time.Now()); err != nil {
		return nil, err
	}
	// The enrolled PEM material and ML-DSA-aware trust chain are immutable for
	// this Credentials incarnation. Parse each peer's pinned TLS config once;
	// callers receive a clone so carrier-specific ALPN and settings cannot
	// change another connection. Provisioning/trust refresh creates a new
	// Credentials object and therefore a fresh cache.
	var peerMu sync.Mutex
	type peerConfigEntry struct {
		peer   meshid.Device
		config *tls.Config
	}
	peerConfigs := make(map[meshid.Device]*list.Element)
	var peerRecency list.List

	credentials := &Credentials{primaryRoots: primaryRoots, Device: self, Certificate: cert, Signer: signer, Verify: verify, issuerMaterial: material, peerTLS: func(peer meshid.Device) (*tls.Config, error) {
		peerMu.Lock()
		defer peerMu.Unlock()
		if cached := peerConfigs[peer]; cached != nil {
			peerRecency.MoveToFront(cached)
			return cached.Value.(peerConfigEntry).config.Clone(), nil
		}
		c, err := mtls.NewClientTLSConfig(certPEM, chainPEM, keyPEM, nil)
		if err != nil {
			return nil, err
		}
		// Use the same verifier as carrier admission, including locally known
		// untrusted intermediates. The original anchors remain unchanged.
		c.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := verify(raw, time.Now())
			return err
		}
		c.VerifyConnection = nil // pinPeer rechecks the verifier on resumption.
		pinPeer(c, peer)
		c.MinVersion = tls.VersionTLS13
		c.MaxVersion = tls.VersionTLS13
		c.NextProtos = []string{LinkALPN}
		c.ClientAuth = tls.RequireAnyClientCert
		c.ClientSessionCache = nil
		c.SessionTicketsDisabled = true
		if len(peerConfigs) >= 256 {
			oldest := peerRecency.Back()
			delete(peerConfigs, oldest.Value.(peerConfigEntry).peer)
			peerRecency.Remove(oldest)
		}
		peerConfigs[peer] = peerRecency.PushFront(peerConfigEntry{peer, c})
		return c.Clone(), nil
	}}
	credentials.peerTLSTickets = func(peer meshid.Device, alpn, transport string) (*tls.Config, error) {
		if alpn == "" || transport == "" {
			return nil, errors.New("wendy net ticket scope needs ALPN and transport")
		}
		cfg, err := credentials.peerTLS(peer)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s/%d:%s/%d:%s", peer.Principal(), len(alpn), alpn, len(transport), transport)
		cfg.ServerName = fmt.Sprintf("device-%s.mesh.wendy.invalid", peer.UUID())
		cfg.ClientSessionCache = scopedClientSessionCache{cache: credentials.ticketCache(), scope: key}
		(&TicketStore{cache: credentials.ticketCache(), scope: key}).Configure(cfg)
		credentials.ConfigureTLSClient(cfg, alpn)

		return cfg, nil
	}
	return credentials, nil
}

// pinPeer requires the handshake leaf to carry exactly the configured peer's
// principal. mtls.NewClientTLSConfig already re-checks chains on resumed
// sessions; wrapping both hooks keeps that guarantee and adds the pin.
func pinPeer(config *tls.Config, peer meshid.Device) {
	chainVerify := config.VerifyPeerCertificate
	pinned := func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if err := chainVerify(rawCerts, verifiedChains); err != nil {
			return err
		}
		if len(rawCerts) == 0 {
			return errors.New("missing wendy net peer certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		device, err := meshid.PeerFromCert(leaf)
		if err != nil {
			return err
		}
		if device.Principal() != peer.Principal() {
			return errors.New("wendy net peer identity mismatch")
		}
		return nil
	}
	config.VerifyPeerCertificate = pinned
	resumeVerify := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if resumeVerify != nil {
			if err := resumeVerify(state); err != nil {
				return err
			}
		}
		if !state.DidResume {
			return nil
		}
		// A resumed handshake presents no fresh certificates: re-run the pin
		// against the ones restored from the session ticket.
		raw := make([][]byte, len(state.PeerCertificates))
		for i, certificate := range state.PeerCertificates {
			raw[i] = certificate.Raw
		}
		return pinned(raw, nil)
	}
}

// PeerTLS builds the client configuration for one configured peer, pinned to
// that peer's full device principal.
func (c *Credentials) PeerTLS(peer meshid.Device) (*tls.Config, error) {
	if peer == c.Device {
		return nil, errors.New("self link")
	}
	if peer.Tenant() != c.Device.Tenant() {
		return nil, errors.New("wendy net peer is outside the tenant")
	}
	config, err := c.peerTLS(peer)
	if err == nil {
		c.ConfigureTLSClient(config, LinkALPN)
	}
	return config, err
}

// PeerTLSWithTickets enables in-memory TLS 1.3 resumption for a specific peer,
// ALPN and transport. QUIC and raw TLS never share tickets. The cache is
// discarded with this Credentials instance when provisioning rotates.
func (c *Credentials) PeerTLSWithTickets(peer meshid.Device, alpn, transport string) (*tls.Config, error) {
	if c == nil || c.peerTLSTickets == nil {
		return nil, errors.New("missing wendy net credential ticket scope")
	}
	return c.peerTLSTickets(peer, alpn, transport)
}

// LeafCertificate is this identity without its issuers. Normal TLS selection
// uses the negotiated per-certificate availability instead of assuming trust.
func (c *Credentials) LeafCertificate() tls.Certificate {
	leaf := c.Certificate
	leaf.Certificate = leaf.Certificate[:min(len(leaf.Certificate), 1)]
	return leaf
}

func (c *Credentials) ticketCache() *meshSessionCache {
	c.ticketOnce.Do(func() { c.ticketMemory = newMeshSessionCache() })
	return c.ticketMemory
}

// ServerTicketStore shares the credentials-wide memory budget for a listener
// accepting multiple peer assets. The caller still verifies the authenticated
// peer identity on every resumed connection. Its protocol and transport scope is
// distinct from pinned outbound/link stores and other protocol listeners.
func (c *Credentials) ServerTicketStore(protocolScope, transport string) *TicketStore {
	return &TicketStore{cache: c.ticketCache(), scope: fmt.Sprintf("server/%d:%s/%d:%s", len(protocolScope), protocolScope, len(transport), transport)}
}

// QUIC links share tickets across LAN and NAN; BLE has a separate TLS scope.
const LinkQUICSessionScope = "link-quic"
