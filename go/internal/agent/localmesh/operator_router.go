package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

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
	return n.attachLink(WithCarrier(ctx, carrierName(ctx, "tls-stream")), link.Peer(), cost, &operatorRouterNodeLink{link}, link.ConnectionState().PeerCertificates...)
}

func (n *Node) AttachOperatorRouterStream(ctx context.Context, c *tls.Conn, cost uint16) error {
	n.gate.RLock()
	agent := n.operatorAgent
	n.gate.RUnlock()
	var link *meshoperatorlink.RouterLink
	var err error
	if n.Credentials.HasCertificateTimePolicy() {
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
