package grpcclient

import (
	"context"
	"fmt"
	"os"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy/go/internal/cli/tlscache"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Keep only CLI-owned credential lookup, routing and persistent session storage
// here. WendyGo owns the TLS verifier, tuning, transport and live evidence.
func connectSDKDevice(ctx context.Context, address string, certInfo *config.CertificateInfo, pins certs.PinChecker, expected *certs.WendyIdentity) (*AgentConnection, error) {
	connector := &wendy.GRPCDeviceConnector{Pins: pins, DialContext: linkLocalDialFn,
		SessionCache: func(address string, leaf []byte) wendy.DeviceSessionCache {
			if cache := tlscache.ForTarget(address, leaf); cache != nil {
				return cache
			}
			return nil
		},
	}
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		connector.Debugf = func(format string, args ...any) { fmt.Fprintf(tlsDebugWriter, format, args...) }
	}
	request := wendy.DeviceConnectRequest{Address: address, TLS: certInfo != nil}
	if id := expected; id != nil {
		request.Expected = &wendy.DeviceIdentity{OrgID: id.OrgID, TenantUUID: id.TenantUUID, EntityType: id.EntityType, EntityID: id.EntityID, Principal: id.Principal}
	}
	if certInfo != nil {
		connector.Credential = func(context.Context, string) (wendy.DeviceTLSCredential, error) {
			key, err := certInfo.PrivateKeyPEM()
			if err != nil {
				return wendy.DeviceTLSCredential{}, fmt.Errorf("loading client key: %w", err)
			}
			return wendy.LoadDeviceTLSCredential(certInfo.PemCertificate, certInfo.PemCertificateChain, key, int32(certInfo.OrganizationID))
		}
	}
	connection, err := connector.ConnectDevice(ctx, request)
	if err != nil {
		return nil, err
	}
	device := connection.(*wendy.GRPCDeviceConnection)
	conn := newAgentConnection(device.Conn())
	conn.device = device
	conn.Host = hostFromAddress(address)
	conn.Addr = address
	conn.IsMTLS = certInfo != nil
	conn.CertInfo = certInfo
	return conn, nil
}
