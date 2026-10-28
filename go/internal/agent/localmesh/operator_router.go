package localmesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"

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
	return n.attachLink(WithCarrier(ctx, carrierName(ctx, "tls-stream")), link.Peer(), cost, &operatorRouterNodeLink{link})
}

func (n *Node) AttachOperatorRouterStream(ctx context.Context, c *tls.Conn, cost uint16) error {
	n.gate.RLock()
	agent := n.operatorAgent
	n.gate.RUnlock()
	link, err := meshoperatorlink.AcceptRouter(ctx, c, n.Credentials.Device.Tenant(), n.Credentials.primaryRoots, agent)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close(); link.Wait() }()
	return n.AttachOperatorRouterLink(ctx, link, cost)
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
