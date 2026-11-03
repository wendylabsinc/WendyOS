package services

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

// LegacyPeerDialer is v1's org-scoped administrative transport. It never
// enters the UUID/Babel mesh. The caller still pins endpoint mTLS to the exact
// numeric org and asset before sending image data over either raw transport.
type LegacyPeerDialer struct {
	logger       *zap.Logger
	provisioning *ProvisioningService
	browse       func(context.Context) ([]models.LANDevice, error)
}

func NewLegacyPeerDialer(logger *zap.Logger, provisioning *ProvisioningService) *LegacyPeerDialer {
	return &LegacyPeerDialer{logger: logger, provisioning: provisioning, browse: func(ctx context.Context) ([]models.LANDevice, error) {
		found, err := discovery.Discover(ctx, discovery.DiscoveryOptions{Types: []models.InterfaceType{models.InterfaceLAN}, Timeout: lanBudget})
		if err != nil {
			return nil, err
		}
		return found.LANDevices, nil
	}}
}
func (d *LegacyPeerDialer) Resolve(org, asset int32) (DeliveryTarget, error) {
	e, err := d.provisioning.CloudEnrollment()
	if err != nil {
		return DeliveryTarget{}, err
	}
	if org == 0 && e.Mode == CloudLegacy {
		org = e.OrganizationID
	}
	if e.Mode != CloudLegacy || org <= 0 || org != e.OrganizationID || asset <= 0 {
		return DeliveryTarget{}, fmt.Errorf("legacy delivery target is outside this device's enrolled organization")
	}
	return DeliveryTarget{LegacyOrgID: org, LegacyAssetID: asset}, nil
}
func (d *LegacyPeerDialer) DialTarget(ctx context.Context, target DeliveryTarget, port uint16) (net.Conn, string, error) {
	checked, err := d.Resolve(target.LegacyOrgID, target.LegacyAssetID)
	if err != nil || target.Device.IsValid() || target.AssetUUID != "" || port == 0 {
		if err == nil {
			err = fmt.Errorf("invalid or mixed legacy transport target")
		}
		return nil, "", err
	}
	enrollment, _ := d.provisioning.CloudEnrollment()
	if target.LegacyAssetID == enrollment.AssetID {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		return conn, "local-service", err
	}
	bounded, cancel := context.WithTimeout(ctx, lanBudget)
	devices, browseErr := d.browse(bounded)
	if browseErr == nil {
		for _, device := range devices {
			if device.AssetID != checked.LegacyAssetID || device.OrgID != checked.LegacyOrgID || !device.IsMTLS || device.IPAddress == "" || strings.EqualFold(device.IPAddress, "localhost") {
				continue
			}
			if ip := net.ParseIP(device.IPAddress); ip != nil && ip.IsLoopback() {
				continue
			}
			conn, err := (&net.Dialer{}).DialContext(bounded, "tcp", net.JoinHostPort(device.IPAddress, strconv.Itoa(int(port))))
			if err == nil {
				cancel()
				return conn, "lan-direct", nil
			}
		}
	}
	cancel()
	return d.DialRoutedTarget(ctx, target, port)
}
func (d *LegacyPeerDialer) DialRoutedTarget(ctx context.Context, target DeliveryTarget, port uint16) (net.Conn, string, error) {
	if _, err := d.Resolve(target.LegacyOrgID, target.LegacyAssetID); err != nil {
		return nil, "", err
	}
	if target.Device.IsValid() || target.AssetUUID != "" || port == 0 {
		return nil, "", fmt.Errorf("invalid or mixed legacy relay target")
	}
	enrollment, cert, chain, key, err := d.provisioning.LegacyCloudCredentials()
	if err != nil || enrollment.Mode != CloudLegacy {
		if err == nil {
			err = fmt.Errorf("legacy enrollment no longer active")
		}
		return nil, "", err
	}
	if brokerIdentityFromCert(d.logger, cert) != certs.AssetURN(enrollment.OrganizationID, enrollment.AssetID) {
		return nil, "", fmt.Errorf("legacy certificate does not match enrollment")
	}
	conn, proof, err := dialLegacyCloud(NewTunnelBrokerClient(d.logger, enrollment.Host, cert, key, chain, 0))
	if err != nil {
		return nil, "", err
	}
	sctx, cancel, established := dialBoundContext(ctx)
	md, err := proof(cloudpb.TunnelBrokerService_ClientTunnel_FullMethodName)
	if err != nil {
		cancel()
		conn.Close()
		return nil, "", err
	}
	stream, err := cloudpb.NewTunnelBrokerServiceClient(conn).ClientTunnel(metadata.NewOutgoingContext(sctx, md))
	if err == nil {
		err = stream.Send(&cloudpb.ClientTunnelMessage{Content: &cloudpb.ClientTunnelMessage_Open{Open: &cloudpb.ClientTunnelOpen{AssetId: target.LegacyAssetID, Host: "localhost", Port: uint32(port)}}})
	}
	if err != nil {
		cancel()
		conn.Close()
		return nil, "", err
	}
	established()
	return streamNetConn(&legacyClientTunnelAdapter{stream: stream}, func() { cancel(); conn.Close() }), "relay", nil
}

type legacyClientTunnelAdapter struct {
	stream cloudpb.TunnelBrokerService_ClientTunnelClient
}

func (a *legacyClientTunnelAdapter) send(p []byte, half bool) error {
	return a.stream.Send(&cloudpb.ClientTunnelMessage{Content: &cloudpb.ClientTunnelMessage_Data{Data: &cloudpb.TunnelData{Payload: p, HalfClose: half}}})
}
func (a *legacyClientTunnelAdapter) recv() ([]byte, bool, error) {
	m, err := a.stream.Recv()
	if err != nil {
		return nil, false, err
	}
	return m.GetPayload(), m.GetHalfClose(), nil
}
func (a *legacyClientTunnelAdapter) closeSend() error { return a.stream.CloseSend() }
