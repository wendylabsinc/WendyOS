package meshsession

import (
	"context"
	"net"
	"net/netip"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type udpSession struct {
	peer    meshid.Device
	address netip.AddrPort
	cancel  context.CancelFunc
	flow    interface{ Close() error }
	retired bool
	tls     bool
}

// DialUDP opens one authorized datagram flow on the peer's shared session. Like
// TCP sessions, pending setup and active UDP flows are revoked by Invalidate,
// PruneRoutes and Close. The caller must resolve the current route before dialing.
// A cold session always uses conservative QUIC startup; agent app dials use
// DialUDPWithRouteSnapshot instead.
func (c *Client) DialUDP(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, options ...UDPOptions) (*DatagramFlow, error) {
	return c.dialUDPWithConfig(ctx, peer, address, port, nil, options...)
}

// DialUDPWithRouteSnapshot applies the same current direct-LAN startup proof
// as DialWithRouteSnapshot when a datagram flow opens the shared session first.
// The caller resolves address from a current snapshot immediately before
// calling; the route is checked again before a cold handshake.
func (c *Client) DialUDPWithRouteSnapshot(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, snapshot func() localmesh.NodeSnapshot, options ...UDPOptions) (*DatagramFlow, error) {
	if snapshot == nil {
		return nil, ErrNoRoute
	}
	return c.dialUDPWithConfig(ctx, peer, address, port, routeQUICConfig(peer, address, snapshot), options...)
}

func (c *Client) dialUDPWithConfig(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, config func() (*quic.Config, error), options ...UDPOptions) (*DatagramFlow, error) {
	if c == nil {
		return nil, net.ErrClosed
	}
	if peer == c.credentials.Device {
		return nil, ErrNoRoute
	}
	setupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	entry := &udpSession{peer: peer, address: address, cancel: cancel}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	c.udpSessions[entry] = struct{}{}
	c.mu.Unlock()
	opts := udpOptions(options)
	if opts.Manager == nil {
		if opts.Endpoint != nil {
			opts.Manager = opts.Endpoint.Manager()
		} else {
			opts.Manager = c.udpManager
		}
	}
	flow, err := c.dialMultiplexedUDP(setupCtx, peer, address, port, opts, config)
	c.mu.Lock()
	valid := err == nil && !c.closed && !entry.retired
	if !valid {
		delete(c.udpSessions, entry)
	} else {
		entry.flow, entry.cancel = flow, nil
	}
	c.mu.Unlock()
	if !valid {
		if flow != nil {
			_ = flow.Close()
		}
		if err != nil {
			return nil, err
		}
		return nil, net.ErrClosed
	}
	go func() {
		<-flow.Done()
		c.mu.Lock()
		delete(c.udpSessions, entry)
		c.mu.Unlock()
	}()
	return flow, nil
}
