package grpcclient

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy/go/internal/cli/tlscache"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/resolver"
)

// The CLI supplies credential lookup, routing, diagnostics and persistent session
// storage. WendyGo owns TLS verification, gRPC tuning and live peer evidence.
func sdkDeviceConnector(address string, certInfo *config.CertificateInfo, pins certs.PinChecker, options []grpc.DialOption) *wendy.GRPCDeviceConnector {
	connector := &wendy.GRPCDeviceConnector{Pins: pins, DialContext: linkLocalDialFn, DialOptions: options,
		SessionCache: func(_ string, leaf []byte) wendy.DeviceSessionCache {
			if cache := tlscache.ForTarget(address, leaf); cache != nil {
				return cache
			}
			return nil
		},
	}
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		connector.Debugf = func(format string, args ...any) {
			// Custom targets are normalized only for the SDK's endpoint input. Keep
			// the CLI's original target spelling in its existing handshake diagnostic.
			if len(args) > 0 {
				args = append([]any(nil), args...)
				args[0] = address
			}
			fmt.Fprintf(tlsDebugWriter, format, args...)
		}
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
	return connector
}

func connectSDKDevice(ctx context.Context, address string, certInfo *config.CertificateInfo, pins certs.PinChecker, expected *certs.WendyIdentity, extraOpts ...grpc.DialOption) (*AgentConnection, error) {
	sdkAddress := address
	options := append([]grpc.DialOption(nil), extraOpts...)
	if target := grpcTarget(address); strings.HasPrefix(target, "passthrough:///") {
		parsed, err := url.Parse(target)
		if err != nil {
			return nil, err
		}
		endpoint := strings.TrimPrefix(parsed.Path, "/")
		sdkAddress = endpoint
		if _, _, err := net.SplitHostPort(sdkAddress); err != nil {
			sdkAddress = net.JoinHostPort(endpoint, "443")
		}
		// SDK input is an endpoint. The host preserves explicit gRPC passthrough
		// targets (including synthetic tunnel labels without a port) with a static
		// resolver, so the caller's dialer receives exactly its original target.
		options = append(options, grpc.WithResolvers(staticDeviceResolver{address: endpoint}))
	}
	connector := sdkDeviceConnector(address, certInfo, pins, options)
	request := wendy.DeviceConnectRequest{Address: sdkAddress, TLS: certInfo != nil}
	if id := expected; id != nil {
		request.Expected = &wendy.DeviceIdentity{OrgID: id.OrgID, TenantUUID: id.TenantUUID, EntityType: id.EntityType, EntityID: id.EntityID, Principal: id.Principal}
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

// grpc.NewClient defaults to DNS for the SDK's plain host:port input. Only an
// explicitly supplied passthrough target selects this host-owned resolver.
type staticDeviceResolver struct{ address string }

func (staticDeviceResolver) Scheme() string { return "dns" }
func (r staticDeviceResolver) Build(_ resolver.Target, cc resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	if err := cc.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: r.address}}}); err != nil {
		return nil, err
	}
	return r, nil
}
func (staticDeviceResolver) ResolveNow(resolver.ResolveNowOptions) {}
func (staticDeviceResolver) Close()                                {}
