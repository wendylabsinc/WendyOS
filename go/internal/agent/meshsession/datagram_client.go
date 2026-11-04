package meshsession

import (
	"context"
	"net"
	"net/netip"

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

// DialUDP opens a dedicated authenticated session for one datagram flow. Like
// TCP sessions, pending setup and active UDP flows are revoked by Invalidate,
// PruneRoutes and Close. The caller must resolve the current route before dialing.
func (c *Client) DialUDP(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, options ...UDPOptions) (*DatagramFlow, error) {
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
	flow, err := DialUDP(setupCtx, c.credentials, peer, address, port, opts)
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
		<-flow.conn.Context().Done()
		c.mu.Lock()
		delete(c.udpSessions, entry)
		c.mu.Unlock()
	}()
	return flow, nil
}
