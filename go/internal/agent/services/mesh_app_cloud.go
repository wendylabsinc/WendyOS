package services

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func (d *MeshDialer) cloudAppTransport(peer meshid.Device) (*localmesh.Credentials, func(context.Context, string, string) (net.Conn, error), error) {
	d.mu.Lock()
	identity, endpoint := d.ident, d.cloudURL
	d.mu.Unlock()
	if endpoint == "" {
		return nil, nil, meshsession.ErrNoRoute
	}
	credentials, err := localmesh.NewCredentials(identity.certPEM, identity.chainPEM, identity.keyPEM)
	if err != nil {
		return nil, nil, err
	}
	if !peer.IsValid() || peer.Tenant() != credentials.Device.Tenant() {
		return nil, nil, errors.New("application relay target is outside the enrolled tenant")
	}
	deviceEndpoint, err := cloudrelay.DeviceEndpoint(endpoint, os.Getenv("WENDY_DEVICE_CLOUD_URL"))
	if err != nil {
		return nil, nil, err
	}
	issuer, err := cloudrelay.Issuer(endpoint, os.Getenv("WENDY_CLOUD_GRANT_ISSUER"))
	if err != nil {
		return nil, nil, err
	}
	sign, err := cloudrelay.PrincipalSigner(identity.certPEM, []byte(identity.keyPEM))
	if err != nil {
		return nil, nil, err
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		cloud, err := cloudrelay.DialCloud(deviceEndpoint, identity.certPEM, identity.chainPEM, []byte(identity.keyPEM))
		if err != nil {
			return nil, err
		}
		defer cloud.Close()
		owner, cancel, established := dialBoundContext(ctx)
		transport, err := cloudrelay.OpenTCP(owner, ctx, cloud, &cloudrelay.Verifier{Issuer: issuer}, peer.UUID().String(), cloudrelay.ApplicationService, sign)
		if err != nil {
			cancel()
			return nil, err
		}
		established()
		return d.ownApplicationConnection(transport, func() error { cancel(); return nil }, identity)
	}
	return credentials, dial, nil
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
