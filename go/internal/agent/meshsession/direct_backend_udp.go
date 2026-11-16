package meshsession

import (
	"context"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// The retained backend reuses the ordinary UDP endpoint's bounded scheduler,
// socket accounting, and incarnation guard. Close/revocation releases that
// endpoint; promotion never opens another local socket.
type directUDPBackend struct {
	*net.UDPConn
	local         *LocalDatagramFlow
	maximum       int
	writeMu       sync.Mutex
	writeDeadline time.Time
}

func (s *Server) directUDPBackend(ctx context.Context, peer meshid.Device, port, maximum uint16) (net.Conn, error) {
	auth, ok := s.authorizer.(UDPAuthorizer)
	if !ok || maximum == 0 || int(maximum) > MaxUDPMessageSize {
		return nil, ErrDenied
	}
	token := auth.UDPToken(port)
	guard := udpmessage.Guard(func(send func() error) error {
		return auth.WithAuthorizedUDPToken(port, token, func(netip.AddrPort) error { return send() })
	})
	endpoint, err := s.udpManager.Open(peer, guard)
	if err != nil {
		return nil, err
	}
	local, err := DialLocalUDP(ctx, auth, port, UDPOptions{Endpoint: endpoint})
	if err != nil {
		endpoint.Close()
		return nil, err
	}
	if err := endpoint.WithGuard(func() error { return nil }); err != nil {
		local.Close()
		return nil, err
	}
	return &directUDPBackend{UDPConn: local.conn, local: local, maximum: int(maximum)}, nil
}
func (c *directUDPBackend) Done() <-chan struct{} { return c.local.endpoint.Done() }
func (c *directUDPBackend) Close() error          { return c.local.Close() }
func (c *directUDPBackend) Write(p []byte) (int, error) {
	if len(p) > c.maximum {
		return 0, syscall.EMSGSIZE
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if err := c.local.Send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// The session relay supplies maximum+1 bytes so oversized local messages are
// detected before delivery. A truncated kernel read is always discarded.
func (c *directUDPBackend) Read(p []byte) (int, error) {
	for {
		if err := c.local.endpoint.WithGuard(func() error { return nil }); err != nil {
			return 0, localUDPError(err)
		}
		n, _, flags, _, err := c.UDPConn.ReadMsgUDP(p, nil)
		if err != nil {
			return 0, err
		}
		if udpReadTruncated(flags) || n > c.maximum {
			continue
		}
		if err := c.local.endpoint.WithGuard(func() error { return nil }); err != nil {
			return 0, localUDPError(err)
		}
		return n, nil
	}
}
func (c *directUDPBackend) SetWriteDeadline(t time.Time) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.writeDeadline = t
	return nil
}
func (c *directUDPBackend) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
