package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

type operatorRouterNodeLink struct{ *meshoperatorlink.RouterLink }

func (l *operatorRouterNodeLink) WriteControl(m ControlMessage) error {
	var data bytes.Buffer
	if err := WriteControl(&data, m); err != nil {
		return err
	}
	return l.WriteControlBytes(data.Bytes())
}
func (l *operatorRouterNodeLink) ReadControl() (ControlMessage, error) {
	data, err := l.ReadControlBytes()
	if err != nil {
		return ControlMessage{}, err
	}
	return ReadControl(bytes.NewReader(data))
}

// AttachOperatorRouterLink uses the ordinary shared Node, gossip and Babel
// forwarding lifecycle for a separately authenticated primary operator role.
// Management substreams remain on the actual carrier TLS session.
func (n *Node) AttachOperatorRouterLink(ctx context.Context, link *meshoperatorlink.RouterLink, cost uint16) error {
	if link == nil {
		return errors.New("missing operator router link")
	}
	defer link.Close()
	if err := n.verifyPeerCertificates(link.Peer(), link.ConnectionState().PeerCertificates); err != nil {
		return err
	}
	selected := link.DirectCapability()
	if selected != 0 {
		n.gate.RLock()
		capability, handler := n.directCapability, n.directHandler
		n.gate.RUnlock()
		physical := IsDirectBLECarrier(ctx)
		if !physical || handler == nil || directstream.NegotiatedCapability(capability, selected) != selected {
			return errors.New("operator direct lane has no eligible session owner")
		}
		state := link.ConnectionState()
		digest, role, err := n.Credentials.directIdentities(link.Peer(), state)
		if err != nil {
			return err
		}
		admission := DirectCarrierAdmission{Deferred: link.DirectDeferred(), IdentityDigest: digest, Role: role, Self: n.Credentials.Device, Peer: link.Peer(), Selected: selected, TLS: state, Records: link}
		return n.attachLink(WithCarrier(ctx, carrierName(ctx, "tls-stream")), link.Peer(), cost, &operatorRouterNodeLink{link}, func(ctx context.Context, id babel.LinkID) error {
			admission.Link = id
			admission.Cost = cost
			return serveDirectCarrier(ctx, admission, handler)
		})
	}
	return n.attachLink(WithCarrier(ctx, carrierName(ctx, "tls-stream")), link.Peer(), cost, &operatorRouterNodeLink{link})
}

func (n *Node) AttachOperatorRouterStream(ctx context.Context, c *tls.Conn, cost uint16) error {
	n.gate.RLock()
	agent := n.operatorAgent
	capability, handler, eligible := n.directCapability, n.directHandler, n.directEligible
	n.gate.RUnlock()
	physical := IsDirectBLECarrier(ctx)
	if !physical || handler == nil {
		capability = 0
	}
	var link *meshoperatorlink.RouterLink
	var err error
	if capability != 0 {
		credentials := n.Credentials
		link, err = meshoperatorlink.AcceptRouterWithDirect(ctx, c, credentials.Device.Tenant(), credentials.primaryRoots, credentials.CertificateTimePolicy(), operatorOwnCredentialExpiry(credentials), meshoperatorlink.RouterDirectConfig{PhysicalBLE: physical, Capability: capability, Eligible: func(peer meshid.Device, state tls.ConnectionState) bool {
			if eligible == nil {
				return true
			}
			digest, _, err := credentials.directIdentities(peer, state)
			return err == nil && eligible(peer, digest)
		}}, agent, credentials.TLSIssuerCandidates)
	} else if n.Credentials.HasCertificateTimePolicy() {
		credentials := n.Credentials
		own := operatorOwnCredentialExpiry(credentials)
		link, err = meshoperatorlink.AcceptRouterWithTimePolicyAndOwn(ctx, c, credentials.Device.Tenant(), credentials.primaryRoots, credentials.CertificateTimePolicy(), own, agent, credentials.TLSIssuerCandidates)
	} else {
		link, err = meshoperatorlink.AcceptRouter(ctx, c, n.Credentials.Device.Tenant(), n.Credentials.primaryRoots, agent, n.Credentials.TLSIssuerCandidates)
	}
	if err != nil {
		return err
	}
	defer func() { _ = link.Close(); link.Wait() }()
	return n.AttachOperatorRouterLink(ctx, link, cost)
}

// Freeze own material and expected role before handing the admitted link to
// management. The credential verifier retains current local policy and trust.
func operatorOwnCredentialExpiry(credentials *Credentials) func() (time.Time, error) {
	expected := credentials.Device
	ownChain := make([][]byte, len(credentials.Certificate.Certificate))
	for i, der := range credentials.Certificate.Certificate {
		ownChain[i] = bytes.Clone(der)
	}
	return func() (time.Time, error) {
		identity, err := credentials.Verify(ownChain, time.Now())
		if err != nil {
			return time.Time{}, err
		}
		if identity.Device != expected || identity.Device.IsOperator() {
			return time.Time{}, errors.New("operator receiver credential identity changed")
		}
		return identity.NotAfter, nil
	}
}

// Primary operator routers have their own bounded admission pool, independent
// of enrolled-device links. Every admitted operator still runs ordinary Babel.
const maxOperatorAttachments = 8

func (n *Node) SetOperatorAgent(serve func(context.Context, net.Conn) error) {
	n.gate.Lock()
	n.operatorAgent = serve
	n.gate.Unlock()
}

// AttachOperatorStream is the provider callback for the only operator role.
func (n *Node) AttachOperatorStream(ctx context.Context, c *tls.Conn, cost uint16) error {
	return n.AttachOperatorRouterStream(ctx, c, cost)
}
