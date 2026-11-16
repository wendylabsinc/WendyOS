package localmesh

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type directBLECarrierKey struct{}

// WithDirectBLECarrier is used by the physical BLE provider, never by discovery
// or route metadata. A diagnostic WithCarrier("ble") label alone cannot opt in.
// TLS peer authentication and the negotiated promotion capability remain required.
func WithDirectBLECarrier(ctx context.Context) context.Context {
	return context.WithValue(WithCarrier(ctx, "ble"), directBLECarrierKey{}, true)
}

// IsDirectBLECarrier reports physical-provider provenance, not a diagnostic
// carrier label or route hint. It does not replace TLS or capability admission.
func IsDirectBLECarrier(ctx context.Context) bool {
	physical, _ := ctx.Value(directBLECarrierKey{}).(bool)
	return physical
}

type DirectRecordCarrier interface {
	TrySendDirectRecord([]byte) (bool, error)
	ReceiveDirectRecord(context.Context) ([]byte, error)
}

// DirectCarrierAdmission is one admitted TLS incarnation, including resumed
// handshakes. The session owner assigns fresh local tokens/aliases and derives
// its identity/security binding from local authenticated state. The TLS state
// exposes this connection's exporter; tickets never authorize logical recovery.
type DirectCarrierAdmission struct {
	// AttachmentAlias is assigned by the carrier owner. Zero preserves the
	// initial alias (1); a reused live lane supplies its next unused alias.
	AttachmentAlias uint32
	// Deferred starts a parked lane at alias zero, after a busy hello.
	Deferred bool

	// PromotionInitiator is set by the local upper transport owner, never by
	// peer data. It is ignored for physical BLE admissions.
	PromotionInitiator bool

	IdentityDigest [32]byte
	Role           byte // logical coordinator0 / participant1, distinct from certificate role

	Self, Peer meshid.Device
	Link       babel.LinkID
	Cost       uint16 // Local physical carrier policy, never peer-advertised.
	Selected   uint32
	TLS        tls.ConnectionState
	Records    DirectRecordCarrier
}
type DirectCarrierHandler func(context.Context, DirectCarrierAdmission) error

// ErrDirectLaneRetired is returned only after the owner has relinquished old
// byte ownership and stopped/joined every lane reader and writer. It retires
// this application lane for the remaining physical carrier incarnation. A
// negotiated reusable lane stays inside its handler while parked; returning
// this error instead keeps ordinary routing alive and drains obsolete records.
var ErrDirectLaneRetired = errors.New("direct application lane retired")

func serveDirectCarrier(ctx context.Context, a DirectCarrierAdmission, handler DirectCarrierHandler) error {
	if err := handler(ctx, a); !errors.Is(err, ErrDirectLaneRetired) {
		return err
	}
	// In-flight old records can arrive after the owner's generation fence. The
	// carrier demux has a bounded reliable queue; abandoning it would block IP
	// and routing traffic too. Drain without granting any new app authority.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := a.Records.ReceiveDirectRecord(ctx); err != nil {
			return err
		}
	}
}

// SetDirectCarrierHandler installs the node's sole session owner once, before
// links are admitted. The handler lives for the entire carrier admission and
// must stop on context cancellation/record EOF. Ordinary return tears down the
// carrier. ErrDirectLaneRetired hands its obsolete direct records back to Node
// for draining while routing and upper application traffic keep using the link.
// It owns no routing authority and must separately authorize every app OPEN.
// eligible optionally declines direct application framing for a device whose
// retained epoch already belongs to an upper transport. The physical carrier
// still negotiates ordinary routing. Its digest is derived from the current
// authenticated TLS identities. It is sampled before the hello, outside
// the node lock; the session owner remains responsible for admission races.
func (n *Node) SetDirectCarrierHandler(capability uint32, handler DirectCarrierHandler, eligible ...func(meshid.Device, [32]byte) bool) error {
	if capability == 0 || !directstream.ValidCapability(capability) || handler == nil || len(eligible) > 1 {
		return errors.New("invalid direct session owner")
	}
	n.gate.Lock()
	defer n.gate.Unlock()
	if n.directHandler != nil || len(n.peers) != 0 || n.stopped {
		return errors.New("direct session owner must be installed before carrier admission")
	}
	n.directCapability = capability
	n.directHandler = handler
	if len(eligible) == 1 {
		n.directEligible = eligible[0]
	}
	return nil
}

func directEndpointIdentity(peer meshid.Device, cert *x509.Certificate) (directstream.EndpointIdentity, error) {
	var out directstream.EndpointIdentity
	if !peer.IsValid() || cert == nil || len(cert.RawSubjectPublicKeyInfo) == 0 {
		return out, errors.New("missing authenticated direct identity")
	}
	named, err := meshid.PeerFromCert(cert)
	if err != nil || named.Principal() != peer.Principal() {
		return out, errors.New("direct identity/certificate mismatch")
	}
	out.Principal = sha256.Sum256([]byte(peer.Principal()))
	out.Instance = peer.UUID()
	out.Key = sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if peer.IsOperator() {
		out.Role = 1
	}
	return out, nil
}

// Both identities are derived here, after TLS/hello validation. No capability or
// OFFER field can choose the digest or logical role handed to the session owner.
func (c *Credentials) directIdentities(peer meshid.Device, state tls.ConnectionState) ([32]byte, byte, error) {
	if c == nil || len(c.Certificate.Certificate) == 0 || !state.HandshakeComplete || len(state.PeerCertificates) == 0 {
		return [32]byte{}, 0, errors.New("missing direct TLS identity")
	}
	leaf := c.Certificate.Leaf
	if leaf == nil {
		var err error
		leaf, err = x509.ParseCertificate(c.Certificate.Certificate[0])
		if err != nil {
			return [32]byte{}, 0, err
		}
	}
	own, err := directEndpointIdentity(c.Device, leaf)
	if err != nil {
		return [32]byte{}, 0, err
	}
	other, err := directEndpointIdentity(peer, state.PeerCertificates[0])
	if err != nil {
		return [32]byte{}, 0, err
	}
	role := byte(0)
	if meshid.Compare(c.Device, peer) > 0 {
		own, other = other, own
		role = 1
	}
	digest, err := directstream.IdentityDigest(own, other)
	return digest, role, err
}

// DirectSessionIdentity derives the same identity binding used by BLE admission
// for an independently authenticated upper connection. Caller must first verify
// TLS trust/policy and match peer's installation to an existing retained epoch.
// No digest, role or capability from the network is accepted here.
func (c *Credentials) DirectSessionIdentity(peer meshid.Device, state tls.ConnectionState) ([32]byte, byte, error) {
	return c.directIdentities(peer, state)
}
