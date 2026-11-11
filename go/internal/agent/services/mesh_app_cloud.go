package services

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"

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

// DialApplicationUDPWithoutMesh preserves the source flow guard while reaching
// an authenticated LAN or app-only Cloud TLS endpoint without a Babel route.
func (d *MeshDialer) DialApplicationUDPWithoutMesh(ctx context.Context, peer meshid.Device, port uint16, options meshsession.UDPOptions) (mesh.UDPFlow, error) {
	identity := d.identity()
	credentials, err := localmesh.NewCredentials(identity.certPEM, identity.chainPEM, identity.keyPEM)
	if err != nil {
		return nil, err
	}
	if !peer.IsValid() || peer.Tenant() != credentials.Device.Tenant() {
		return nil, errors.New("application UDP target is outside the enrolled tenant")
	}
	flow, found, err := dialApplicationLAN(ctx, d, peer, true, func(attempt context.Context, candidate applicationLANAddress) (mesh.UDPFlow, error) {
		flow, err := meshsession.DialUDPTLSWithDialer(attempt, credentials, peer, candidate.address, port, d.applicationLANDial(identity), options)
		if err != nil {
			return nil, err
		}
		return flow, nil
	})
	if found || err != nil {
		return flow, err
	}
	credentials, dial, err := d.cloudAppTransport(peer)
	if err != nil {
		return nil, err
	}
	address := netip.AddrPortFrom(peer.MeshAddress(), meshsession.TLSPort)
	return meshsession.DialUDPTLSWithDialer(ctx, credentials, peer, address, port, dial, options)
}
