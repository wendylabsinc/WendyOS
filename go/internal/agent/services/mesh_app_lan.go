package services

import (
	"context"
	"net"
	"net/netip"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshappstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// meshDialAppLAN connects to the discovered agent, not to an unverified app
// socket. Discovery only selects an address: TLS pins the full device principal,
// then MeshDial authorizes the requested port against the live app registry.
func (d *MeshDialer) meshDialAppLAN(ctx context.Context, hostport string, device meshid.Device, port uint16) (net.Conn, error) {
	identity := d.identity()
	config, err := mtls.NewClientTLSConfigExpectingDevice(identity.certPEM, identity.chainPEM, identity.keyPEM, d.logger, device)
	if err != nil {
		return nil, err
	}
	attempt, stop := context.WithCancel(ctx)
	defer stop()
	causes := &lanConnectionErrors{stop: stop}
	cc, err := grpc.NewClient(meshDialTarget,
		grpc.WithTransportCredentials(lanRecordingCredentials{credentials.NewTLS(config), causes}),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			conn, err := d.dialApplicationSocket(ctx, "tcp", hostport)
			causes.recordDial(err)
			return conn, err
		}))
	if err != nil {
		return nil, err
	}
	conn, err := meshappstream.Dial(attempt, cc, port)
	if err != nil {
		_ = cc.Close()
		return nil, causes.cause(err)
	}
	return d.ownApplicationConnection(conn, cc.Close, identity)
}

// Each LAN dial owns its agent connection. The wrapped application stream keeps
// the same half-close behavior while Close also releases the HTTP/2 transport.
type ownedAppConnection struct {
	net.Conn
	closeTransport func() error
	once           sync.Once
	closeErr       error
	onClose        func()
}

func (c *ownedAppConnection) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		if err := c.closeTransport(); c.closeErr == nil {
			c.closeErr = err
		}
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.closeErr
}

func (d *MeshDialer) ownApplicationConnection(conn net.Conn, closeTransport func() error, identity meshIdentity) (net.Conn, error) {
	owned := &ownedAppConnection{Conn: conn, closeTransport: closeTransport}
	d.mu.Lock()
	if d.ident != identity {
		d.mu.Unlock()
		_ = owned.Close()
		return nil, net.ErrClosed
	}
	if d.appFlows == nil {
		d.appFlows = make(map[*ownedAppConnection]struct{})
	}
	d.appFlows[owned] = struct{}{}
	owned.onClose = func() {
		d.mu.Lock()
		delete(d.appFlows, owned)
		d.mu.Unlock()
	}
	d.mu.Unlock()
	return owned, nil
}

func (c *ownedAppConnection) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return c.Close()
}

func (d *MeshDialer) applicationLANDial(identity meshIdentity) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := d.dialApplicationSocket(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return d.ownApplicationConnection(conn, func() error { return nil }, identity)
	}
}

func (d *MeshDialer) meshDialApplicationTLS(ctx context.Context, address netip.AddrPort, peer meshid.Device, port uint16) (net.Conn, error) {
	identity := d.identity()
	credentials, err := localmesh.NewCredentials(identity.certPEM, identity.chainPEM, identity.keyPEM)
	if err != nil {
		return nil, err
	}
	return meshsession.DialTLSWithDialer(ctx, credentials, peer, address, port, d.applicationLANDial(identity))
}

func (d *MeshDialer) dialApplicationSocket(ctx context.Context, network, address string) (net.Conn, error) {
	bounded, cancel := context.WithTimeout(ctx, lanBudget)
	defer cancel()
	if d.appSocketDial != nil {
		return d.appSocketDial(bounded, network, address)
	}
	return (&net.Dialer{}).DialContext(bounded, network, address)
}
