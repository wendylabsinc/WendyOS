package backend

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

// Each flow owns its transport. Closing it also closes the cloud broker and
// agent connection. The CLI enforces the selected device's TLS identity.
func desktopDial(ctx context.Context, selector string, port uint16) (net.Conn, error) {
	if !strings.HasPrefix(selector, "mesh:") && !strings.HasPrefix(selector, "cloud://") {
		return nil, errors.New("VPN routes require a mesh or cloud device")
	}
	agent, err := commands.DesktopConnectTunnel(ctx, selector)
	if err != nil {
		return nil, err
	}
	if agent.RegistryDialer == nil {
		agent.Close()
		return nil, errors.New("device has no TCP tunnel transport")
	}
	conn, err := agent.RegistryDialer(ctx, int(port))
	if err != nil {
		agent.Close()
		return nil, err
	}
	return withConnRelease(conn, func() { agent.Close() }), nil
}

// Report missing Cloud port support once per VPN session. Other dial failures
// retain their normal behavior, including authorization refusals.
func reportVPNDialFailures(dial vpn.DialFunc, report func(error)) vpn.DialFunc {
	var reported sync.Once
	return func(ctx context.Context, selector string, port uint16) (net.Conn, error) {
		conn, err := dial(ctx, selector, port)
		var unsupported *cloudrelay.UnsupportedServiceError
		if ctx.Err() == nil && errors.As(err, &unsupported) {
			reported.Do(func() { report(unsupported) })
		}
		return conn, err
	}
}

// withConnRelease releases ownership on full close while preserving a
// transport's optional write half-close.
func withConnRelease(conn net.Conn, release func()) net.Conn {
	owned := &ownedConn{Conn: conn, release: release}
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		return &ownedHalfCloseConn{ownedConn: owned, closeWrite: half.CloseWrite}
	}
	return owned
}

type ownedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *ownedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.release()
	})
	return err
}

type ownedHalfCloseConn struct {
	*ownedConn
	closeWrite func() error
}

func (c *ownedHalfCloseConn) CloseWrite() error { return c.closeWrite() }
