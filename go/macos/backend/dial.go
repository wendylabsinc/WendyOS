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
	agent, err := commands.DesktopConnect(ctx, selector)
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
	return &ownedConn{Conn: conn, closeAgent: func() { agent.Close() }}, nil
}

// Report missing Cloud port support once per VPN session. Other dial failures
// retain their normal behavior, including authorization refusals.
func reportVPNDialFailures(dial vpn.DialFunc, report func(string)) vpn.DialFunc {
	var reported sync.Once
	return func(ctx context.Context, selector string, port uint16) (net.Conn, error) {
		conn, err := dial(ctx, selector, port)
		var unsupported *cloudrelay.UnsupportedServiceError
		if ctx.Err() == nil && errors.As(err, &unsupported) {
			reported.Do(func() { report(unsupported.Error()) })
		}
		return conn, err
	}
}

type ownedConn struct {
	net.Conn
	closeAgent func()
	once       sync.Once
}

func (c *ownedConn) Close() error {
	var err error
	c.once.Do(func() { err = c.Conn.Close(); c.closeAgent() })
	return err
}
func (c *ownedConn) CloseWrite() error {
	if h, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return h.CloseWrite()
	}
	return c.Close()
}
