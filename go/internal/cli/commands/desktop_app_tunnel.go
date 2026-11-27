package commands

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type appTunnelIdentityKey struct{}

type appTunnelIdentity struct {
	operator string
	device   *certs.WendyIdentity
}

// DesktopAppTunnelConnector retains the identities from the original connection
// for later app tunnel renewal, even if another login replaces its saved session.
func DesktopAppTunnelConnector(original *grpcclient.AgentConnection) func(context.Context, string) (*grpcclient.AgentConnection, error) {
	operator := appTunnelOperator(original.CertInfo)
	pin := appTunnelIdentity{operator: operator}
	if observed, ok := original.ObservedServerIdentity(); ok {
		pin.device = &observed
	}
	return func(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
		if pin.operator == "" {
			return nil, errors.New("cannot identify the original Cloud app tunnel operator")
		}
		return DesktopConnectTunnel(context.WithValue(ctx, appTunnelIdentityKey{}, pin), selector)
	}
}

func appTunnelOperator(cert *config.CertificateInfo) string {
	if cert == nil {
		return ""
	}
	leaf, err := certs.ParseStoredLeaf(cert.PemCertificate)
	if err != nil {
		return ""
	}
	id, known, err := certs.IdentityFromCert(leaf)
	if err != nil || !known || id.EntityType != certs.EntityUser {
		return ""
	}
	return id.IdentityKey()
}

func requireAppTunnelOperator(ctx context.Context, auth *config.AuthConfig) error {
	pin, ok := ctx.Value(appTunnelIdentityKey{}).(appTunnelIdentity)
	if !ok {
		return nil
	}
	if auth == nil || len(auth.Certificates) == 0 || appTunnelOperator(&auth.Certificates[0]) != pin.operator {
		return errors.New("Cloud app tunnel operator changed; reopen the app with the intended sign-in")
	}
	return nil
}

func requireAppTunnelDevice(ctx context.Context, auth *config.AuthConfig, asset cloudDiscoveryDevice) error {
	pin, ok := ctx.Value(appTunnelIdentityKey{}).(appTunnelIdentity)
	if ok && pin.device != nil && !pin.device.SameEntity(cloudDiscoveryIdentity(auth, asset)) {
		return errors.New("Cloud app tunnel device identity changed; reselect the device")
	}
	return nil
}
