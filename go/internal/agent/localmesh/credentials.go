package localmesh

import (
	"bytes"
	"container/list"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Credentials reuses the agent's provisioned signer and ML-DSA-aware trust
// verifier. Reconstruct this object (and the session/cache) when provisioning or
// trust changes. No mesh-specific private key or trust-on-first-use is created.
//
// The mesh identity is the leaf's device principal
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
	issuerDigest   string
}

// NewCredentials loads the provisioned identity and derives the local-mesh
// identity from its leaf. Certificates without a device principal — asset, user,
// service, or legacy numeric principals — are refused outright; there is no
// fallback identity.
func NewCredentials(certPEM, chainPEM, keyPEM string) (*Credentials, error) {
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
	self, err := meshid.DeviceFromCert(leaf)
	if err != nil {
		return nil, err
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
	if _, err := verifyChain(cert.Certificate, time.Now()); err != nil {
		return nil, err
	}
	anchors, err := mtls.PeerTrustAnchors(chainPEM)
	if err != nil {
		return nil, err
	}
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
		expires, err := verifyChain(chain, now)
		if err != nil {
			return Identity{}, err
		}
		peerLeaf, err := x509.ParseCertificate(chain[0])
		if err != nil {
			return Identity{}, err
		}
		device, err := meshid.DeviceFromCert(peerLeaf)
		if err != nil {
			return Identity{}, err
		}
		if device.Tenant() != self.Tenant() {
			return Identity{}, errors.New("wendy net peer is outside the tenant")
		}
		if now.Before(peerLeaf.NotBefore) || !now.Before(peerLeaf.NotAfter) {
			return Identity{}, errors.New("wendy net identity is not currently valid")
		}
		return Identity{Device: device, NotAfter: expires}, nil
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

	primaryRoots := make([]*x509.Certificate, 0, len(anchors))
	for _, der := range anchors {
		root, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		primaryRoots = append(primaryRoots, root)
	}
	credentials := &Credentials{primaryRoots: primaryRoots, Device: self, Certificate: cert, Signer: signer, Verify: verify, issuerDigest: anchoredIssuerDigest(cert.Certificate[1:], anchors), peerTLS: func(peer meshid.Device) (*tls.Config, error) {
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
		cfg, err := credentials.PeerTLS(peer)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s/%d:%s/%d:%s", peer.Principal(), len(alpn), alpn, len(transport), transport)
		cfg.NextProtos = []string{alpn}
		cfg.ServerName = fmt.Sprintf("device-%s.mesh.wendy.invalid", peer.UUID())
		cfg.ClientSessionCache = scopedClientSessionCache{cache: credentials.ticketCache(), scope: key}
		(&TicketStore{cache: credentials.ticketCache(), scope: key}).Configure(cfg)

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
		device, err := meshid.DeviceFromCert(leaf)
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
	if peer.Principal() == c.Device.Principal() {
		return nil, errors.New("self link")
	}
	if peer.Tenant() != c.Device.Tenant() {
		return nil, errors.New("wendy net peer is outside the tenant")
	}
	return c.peerTLS(peer)
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

// IssuerDigest names the CA certificates this identity presents after its
// leaf. It is empty when there are none, or when any of them is not also one of
// this device's own trust anchors. Two devices with the same digest present
// the same issuers and each already trusts them, so either may omit them when
// it authenticates to the other without changing what the receiver verifies.
func (c *Credentials) IssuerDigest() string {
	if c == nil {
		return ""
	}
	return c.issuerDigest
}

// LeafCertificate is this identity without its issuers, for a peer that
// already trusts them (see IssuerDigest).
func (c *Credentials) LeafCertificate() tls.Certificate {
	leaf := c.Certificate
	leaf.Certificate = leaf.Certificate[:min(len(leaf.Certificate), 1)]
	return leaf
}

func anchoredIssuerDigest(issuers, anchors [][]byte) string {
	if len(issuers) == 0 {
		return ""
	}
	trusted := make(map[string]bool, len(anchors))
	for _, anchor := range anchors {
		trusted[string(anchor)] = true
	}
	sums := make([][sha256.Size]byte, 0, len(issuers))
	for _, issuer := range issuers {
		if !trusted[string(issuer)] {
			return ""
		}
		sums = append(sums, sha256.Sum256(issuer))
	}
	slices.SortFunc(sums, func(a, b [sha256.Size]byte) int { return bytes.Compare(a[:], b[:]) })
	digest := sha256.New()
	for _, sum := range sums {
		digest.Write(sum[:])
	}
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil)[:16])
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
