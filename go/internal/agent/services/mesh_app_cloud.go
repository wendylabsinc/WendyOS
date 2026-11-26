package services

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type cloudAppClient struct {
	credentials *localmesh.Credentials
	conn        *grpc.ClientConn
	verifier    *cloudrelay.Verifier
	sign        func([]byte) ([]byte, error)
	lifetime    context.Context
	cancel      context.CancelFunc
}

func (c *cloudAppClient) Close() error {
	c.cancel()
	return c.conn.Close()
}

func (d *MeshDialer) newCloudAppClientLocked() (*cloudAppClient, error) {
	identity, endpoint := d.ident, d.cloudURL
	deviceEndpoint, err := cloudrelay.DeviceEndpoint(endpoint, os.Getenv("WENDY_DEVICE_CLOUD_URL"))
	if err != nil {
		return nil, err
	}
	issuer, err := cloudrelay.Issuer(endpoint, os.Getenv("WENDY_CLOUD_GRANT_ISSUER"))
	if err != nil {
		return nil, err
	}
	credentials, err := localmesh.NewCredentials(identity.certPEM, identity.chainPEM, identity.keyPEM)
	if err != nil {
		return nil, err
	}
	sign, err := cloudrelay.DevicePrincipalSigner(identity.certPEM, []byte(identity.keyPEM))
	if err != nil {
		return nil, err
	}
	conn, err := cloudrelay.DialCloud(deviceEndpoint, identity.certPEM, identity.chainPEM, []byte(identity.keyPEM))
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &cloudAppClient{
		credentials: credentials, conn: conn, verifier: &cloudrelay.Verifier{Issuer: issuer},
		sign: sign, lifetime: lifetime, cancel: cancel,
	}, nil
}

func (d *MeshDialer) cloudAppClientFor(peer meshid.Device) (*cloudAppClient, meshIdentity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	identity := d.ident
	if d.cloudURL == "" {
		return nil, identity, meshsession.ErrNoRoute
	}
	if !peer.IsValid() || peer.Tenant() != identity.device.Tenant() {
		return nil, identity, errors.New("application relay target is outside the enrolled tenant")
	}
	if d.appCloud == nil {
		client, err := d.newCloudAppClientLocked()
		if err != nil {
			return nil, identity, err
		}
		d.appCloud = client
	}
	return d.appCloud, identity, nil
}

func (d *MeshDialer) cloudAppTransport(peer meshid.Device) (*localmesh.Credentials, func(context.Context, string, string) (net.Conn, error), error) {
	client, identity, err := d.cloudAppClientFor(peer)
	if err != nil {
		return nil, nil, err
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		owner, cancel, established := dialBoundContext(ctx)
		stop := context.AfterFunc(client.lifetime, cancel)
		closeTransport := func() error { stop(); cancel(); return nil }
		if client.lifetime.Err() != nil {
			_ = closeTransport()
			return nil, net.ErrClosed
		}
		// RequestTunnel issues and verifies a fresh grant for each relay.
		// The shared client carries only this enrollment's transport and public keys.
		transport, err := cloudrelay.OpenTCP(owner, owner, client.conn, client.verifier, peer.UUID().String(), cloudrelay.ApplicationService, client.sign)
		if err != nil {
			_ = closeTransport()
			return nil, err
		}
		established()
		return d.ownApplicationConnection(transport, closeTransport, identity)
	}
	return client.credentials, dial, nil
}

func (d *MeshDialer) meshDialAppCloud(ctx context.Context, peer meshid.Device, port uint16) (net.Conn, error) {
	credentials, dial, err := d.cloudAppTransport(peer)
	if err != nil {
		return nil, err
	}
	address := netip.AddrPortFrom(peer.MeshAddress(), meshsession.TLSPort)
	return meshsession.DialTLSWithDialer(ctx, credentials, peer, address, port, dial)
}

// DialApplicationUDPViaCloud preserves the source flow guard while carrying
// authorized app datagrams inside the encrypted Cloud byte stream.
func (d *MeshDialer) DialApplicationUDPViaCloud(ctx context.Context, peer meshid.Device, port uint16, options meshsession.UDPOptions) (mesh.UDPFlow, error) {
	credentials, dial, err := d.cloudAppTransport(peer)
	if err != nil {
		return nil, err
	}
	address := netip.AddrPortFrom(peer.MeshAddress(), meshsession.TLSPort)
	return meshsession.DialUDPTLSWithDialer(ctx, credentials, peer, address, port, dial, options)
}

// ownedAppConnection closes the relay and its context on enrollment retirement.
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
