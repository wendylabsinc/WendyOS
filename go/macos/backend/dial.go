package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshappstream"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Each flow owns its transport. Closing it also closes the cloud broker and
// agent connection. The CLI enforces the selected device's TLS identity.
func desktopDial(ctx context.Context, selector string, port uint16) (net.Conn, error) {
	return desktopDialExpected(ctx, selector, port, "")
}

func desktopDialExpected(ctx context.Context, selector string, port uint16, expected string) (net.Conn, error) {
	return desktopDialWithConnect(ctx, selector, port, expected, commands.DesktopConnect)
}

func desktopDialWithConnect(ctx context.Context, selector string, port uint16, expected string, connect func(context.Context, string) (*grpcclient.AgentConnection, error)) (net.Conn, error) {
	if !strings.HasPrefix(selector, "mesh:") && !strings.HasPrefix(selector, "cloud://") {
		return nil, errors.New("VPN routes require a mesh or cloud device")
	}
	agent, err := connect(ctx, selector)
	if err != nil {
		return nil, err
	}
	if expected != "" {
		// Cloud inventory can change after DNS was answered. Force TLS to
		// complete and pin this flow to the identity behind that answer.
		if agent.AgentService == nil {
			agent.Close()
			return nil, errors.New("device cannot prove its route identity")
		}
		if _, err := agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
			agent.Close()
			return nil, err
		}
		actual, ok := agent.ObservedServerIdentity()
		if !ok || actual.Principal != expected {
			agent.Close()
			return nil, fmt.Errorf("VPN route identity changed: expected %s", expected)
		}
	}
	var conn net.Conn
	if strings.HasPrefix(selector, "cloud://") && strings.Contains(selector, "/tenant/") && port != 50052 && port != 22 {
		// Published apps use destination admission on the authenticated agent
		// stream. A raw Cloud tcp-N tunnel is not an app authorization grant.
		if !agent.IsMTLS || agent.Conn == nil {
			agent.Close()
			return nil, errors.New("Cloud app streams require an authenticated agent connection")
		}
		conn, err = meshappstream.Dial(ctx, agent.Conn, port)
	} else {
		if agent.RegistryDialer == nil {
			agent.Close()
			return nil, errors.New("device has no TCP tunnel transport")
		}
		conn, err = agent.RegistryDialer(ctx, int(port))
	}
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
