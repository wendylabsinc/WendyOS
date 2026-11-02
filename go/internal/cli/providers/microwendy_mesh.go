package providers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// MeshLiteDevice projects a fresh signed native WendyCom announcement into the
// existing provider model. It carries no live connection or inventory guess.
func MeshLiteDevice(info sessionbroker.OperatorTargetInfo) (models.ExternalDevice, error) {
	target, matched, err := sessionbroker.ParseMeshSelector(info.Selector)
	if err != nil || !matched || info.Platform != "Lite" || info.Transport != "mtls-v1" ||
		info.Tenant != target.Tenant().String() || info.Device != target.UUID().String() || info.Principal != target.Principal() {
		return models.ExternalDevice{}, fmt.Errorf("mesh target has no signed Lite WendyCom endpoint")
	}
	name := info.Name
	if name == "" {
		name = info.Device
	}
	return models.ExternalDevice{ID: info.Selector, DisplayName: name, ProviderKey: "wendy-lite", IsWendyDevice: true,
		ConnectionInfo: map[string]string{"type": "Mesh", "selector": info.Selector, "deviceId": info.Device,
			"enrolledDeviceId": info.Device, "tenantId": info.Tenant, "mtls": "true", "caps": strings.Join(info.Caps, ",")}}, nil
}

func connectMeshLite(ctx context.Context, client *liteclient.WendyLiteClient, device models.ExternalDevice) error {
	selector := device.ConnectionInfo["selector"]
	// Refresh before each connection: route availability, port, platform and
	// capabilities come from the authenticated router's current signed view.
	info, err := sessionbroker.DescribeOperatorTarget(ctx, selector)
	if err != nil {
		return err
	}
	current, err := MeshLiteDevice(info)
	if err != nil {
		return err
	}
	if current.ConnectionInfo["enrolledDeviceId"] != device.ConnectionInfo["enrolledDeviceId"] || current.ConnectionInfo["tenantId"] != device.ConnectionInfo["tenantId"] {
		return fmt.Errorf("mesh Lite selection identity changed")
	}
	directory, err := config.ConfigDir()
	if err != nil {
		return err
	}
	return connectWithScopedCLIPrincipal(ctx, info.Tenant, 0, info.OperatorPrincipal, func(cert tls.Certificate, chain string, lite liteclient.LiteDeviceCAChain) error {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return err
		}
		identity, ok, err := certs.IdentityFromCert(leaf)
		if err != nil || !ok || identity.Principal != info.OperatorPrincipal {
			return fmt.Errorf("Lite credential must match the attached primary operator")
		}
		return client.ConnectWithPKIDialerContext(ctx, info.Address, cert, chain, info.Device, lite, meshLitePins{directory},
			func(dial context.Context, _, _ string) (net.Conn, error) {
				return sessionbroker.DialOperatorLite(dial, selector)
			})
	})
}

// Reload at the actual verified handshake, so another invocation's pin change
// is observed even if resolving credentials or dialing the mesh took time.
type meshLitePins struct{ directory string }

func (p meshLitePins) CheckAndUpdate(leaf *x509.Certificate, name string) error {
	store, err := devicepin.Open(p.directory)
	if err != nil {
		return err
	}
	return store.CheckAndUpdate(leaf, name)
}
