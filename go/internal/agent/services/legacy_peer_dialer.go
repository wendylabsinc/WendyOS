package services

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

// Match the normal discovery window on a cold Avahi cache. Confirmed peers
// settle sooner; results are cached so registry connections do not re-browse.
const legacyLANScanTimeout = 5 * time.Second

type legacyLANKey struct {
	binding CloudEnrollment
	asset   int32
}

// LegacyPeerDialer preserves v1's numeric app and administrative transports.
// It never enters the UUID/Babel mesh. Administrative callers pin endpoint
// mTLS themselves; app streams pin the discovered agent before MeshDial.
type LegacyPeerDialer struct {
	mu           sync.Mutex
	cache        map[legacyLANKey]lanCacheEntry
	metrics      *MeshMetrics
	logger       *zap.Logger
	provisioning *ProvisioningService
	browse       func(context.Context) ([]models.LANDevice, error)
	dialTCP      func(context.Context, string) (net.Conn, error)
}

func NewLegacyPeerDialer(logger *zap.Logger, provisioning *ProvisioningService) *LegacyPeerDialer {
	return &LegacyPeerDialer{logger: logger, provisioning: provisioning, dialTCP: func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}, browse: func(ctx context.Context) ([]models.LANDevice, error) {
		// Let CollectLAN's scan timer conclude normally so confirmed answers
		// survive a full scan window. The caller's context bounds cancellation.
		enrollment, err := provisioning.CloudEnrollment()
		if err != nil {
			return nil, err
		}
		if enrollment.Mode != CloudLegacy {
			return nil, fmt.Errorf("legacy discovery requires a v1 enrollment")
		}
		// Foreign and self sightings must not settle the scan before a usable
		// peer resolves. This matters on hosts advertising many app bridges.
		filter := legacyPeerFilter{org: enrollment.OrganizationID, self: enrollment.AssetID}
		return discovery.CollectLAN(ctx, discovery.StreamOptions{Exclude: filter}, legacyLANScanTimeout)
	}}
}
func (d *LegacyPeerDialer) Resolve(org, asset int32) (DeliveryTarget, error) {
	e, err := d.provisioning.CloudEnrollment()
	if err != nil {
		return DeliveryTarget{}, err
	}
	return legacyDeliveryTarget(e, org, asset)
}

func legacyDeliveryTarget(e CloudEnrollment, org, asset int32) (DeliveryTarget, error) {
	if org == 0 && e.Mode == CloudLegacy {
		org = e.OrganizationID
	}
	if e.Mode != CloudLegacy || org <= 0 || org != e.OrganizationID || asset <= 0 {
		return DeliveryTarget{}, fmt.Errorf("legacy delivery target is outside this device's enrolled organization")
	}
	return DeliveryTarget{LegacyOrgID: org, LegacyAssetID: asset}, nil
}
func (d *LegacyPeerDialer) DialTarget(ctx context.Context, target DeliveryTarget, port uint16) (net.Conn, string, error) {
	enrollment, err := d.provisioning.CloudEnrollment()
	if err != nil {
		return nil, "", err
	}
	checked, err := legacyDeliveryTarget(enrollment, target.LegacyOrgID, target.LegacyAssetID)
	if err != nil || target.Device.IsValid() || target.AssetUUID != "" || port == 0 {
		if err == nil {
			err = fmt.Errorf("invalid or mixed legacy transport target")
		}
		return nil, "", err
	}
	if target.LegacyAssetID == enrollment.AssetID {
		conn, err := d.dialTCP(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		return conn, "local-service", err
	}

	if hostport, ok := d.legacyAppLANAddr(ctx, enrollment, checked.LegacyAssetID); ok {
		host, _, err := net.SplitHostPort(hostport)
		if err != nil {
			return nil, "", err
		}
		bounded, cancel := legacyLANContext(ctx)
		conn, err := d.dialTCP(bounded, net.JoinHostPort(host, strconv.Itoa(int(port))))
		cancel()
		if err == nil {
			return conn, "lan-direct", nil
		}
	}
	return d.DialRoutedTarget(ctx, target, port)
}
func (d *LegacyPeerDialer) DialRoutedTarget(ctx context.Context, target DeliveryTarget, port uint16) (net.Conn, string, error) {
	if target.Device.IsValid() || target.AssetUUID != "" || port == 0 {
		return nil, "", fmt.Errorf("invalid or mixed legacy relay target")
	}
	enrollment, cert, chain, key, err := d.provisioning.LegacyCloudCredentials()
	defer clear(key)
	if err != nil || enrollment.Mode != CloudLegacy {
		if err == nil {
			err = fmt.Errorf("legacy enrollment no longer active")
		}
		return nil, "", err
	}
	if _, err := legacyDeliveryTarget(enrollment, target.LegacyOrgID, target.LegacyAssetID); err != nil {
		return nil, "", err
	}
	if brokerIdentityFromCert(d.logger, cert) != certs.AssetURN(enrollment.OrganizationID, enrollment.AssetID) {
		return nil, "", fmt.Errorf("legacy certificate does not match enrollment")
	}
	conn, proof, err := dialLegacyCloud(NewTunnelBrokerClient(d.logger, LegacyBrokerURL(enrollment.Host), cert, key, chain, 0))
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

// SetMetrics must be called before serving connections.
func (d *LegacyPeerDialer) SetMetrics(m *MeshMetrics) { d.metrics = m }

// LegacyOrgID reads the saved protocol rather than inferring it from a SAN.
func (d *LegacyPeerDialer) LegacyOrgID() int32 {
	e, err := d.provisioning.CloudEnrollment()
	if err != nil || e.Mode != CloudLegacy {
		return 0
	}
	return e.OrganizationID
}

// DialLegacyApp restores numeric application dialing. LAN uses an authenticated
// MeshDial stream to the discovered agent port, not raw TCP to the app port.
func (d *LegacyPeerDialer) DialLegacyApp(ctx context.Context, org, asset int32, port uint16) (net.Conn, string, error) {
	target, err := d.Resolve(org, asset)
	if err != nil {
		return nil, "", err
	}
	if asset > 65534 || port == 0 {
		return nil, "", fmt.Errorf("invalid legacy application destination")
	}
	e, cert, chain, key, err := d.provisioning.LegacyCloudCredentials()
	defer clear(key)
	if err != nil || e.Mode != CloudLegacy || e.OrganizationID != target.LegacyOrgID {
		return nil, "", fmt.Errorf("legacy enrollment changed")
	}
	record := func(mode string, start time.Time, err error) {
		result := "ok"
		if err != nil {
			result = "error"
		}
		d.metrics.RecordAssetDial(asset, mode, result, float64(time.Since(start))/float64(time.Millisecond))
	}
	if asset == e.AssetID {
		start := time.Now()
		conn, err := d.dialTCP(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		record("local-app", start, err)
		return conn, "local-app", err
	}

	start := time.Now()
	if hostport, ok := d.legacyAppLANAddr(ctx, e, asset); ok {
		bounded, cancel := legacyLANContext(ctx)
		conn, err := d.dialLegacyLAN(bounded, hostport, cert, chain, key, e.OrganizationID, asset, port)
		cancel()
		record("lan-direct", start, err)
		if err == nil {
			return conn, "lan-direct", nil
		}
	}
	start = time.Now()
	conn, mode, err := d.DialRoutedTarget(ctx, target, port)
	record("relay", start, err)
	return conn, mode, err
}

func (d *LegacyPeerDialer) dialLegacyLAN(ctx context.Context, hostport, cert, chain string, key []byte, org, asset int32, port uint16) (net.Conn, error) {
	tlsConfig, err := mtls.NewClientTLSConfigExpectingIdentityWithKeyBytes(cert, chain, key, d.logger, certs.WendyIdentity{OrgID: org, EntityType: certs.EntityAsset, EntityID: strconv.Itoa(int(asset))})
	if err != nil {
		return nil, err
	}
	cc, err := grpc.NewClient(meshDialTarget, grpc.WithContextDialer(meshDialContextDialer(hostport)), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, err
	}
	sctx, cancel, established := dialBoundContext(ctx)
	stream, err := agentpbv2.NewWendyMeshServiceClient(cc).MeshDial(sctx)
	if err == nil {
		err = stream.Send(&agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Open{Open: &agentpbv2.MeshDialOpen{Port: uint32(port)}}})
	}
	if err != nil {
		cancel()
		cc.Close()
		return nil, err
	}
	// Older v1 agents have no ready frame: the first response may require an
	// application request. Empty ready frames from newer agents are harmless.
	established()
	return streamNetConn(&legacyMeshDialAdapter{stream}, func() { cancel(); cc.Close() }), nil
}

type legacyMeshDialAdapter struct {
	stream agentpbv2.WendyMeshService_MeshDialClient
}

func (a *legacyMeshDialAdapter) send(p []byte, half bool) error {
	return a.stream.Send(&agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Data{Data: &agentpbv2.MeshDialData{Payload: p, HalfClose: half}}})
}
func (a *legacyMeshDialAdapter) recv() ([]byte, bool, error) {
	m, err := a.stream.Recv()
	if err != nil {
		return nil, false, err
	}
	return m.Payload, m.HalfClose, nil
}
func (a *legacyMeshDialAdapter) closeSend() error { return a.stream.CloseSend() }

func (d *LegacyPeerDialer) legacyAppLANAddr(ctx context.Context, e CloudEnrollment, asset int32) (string, bool) {
	key := legacyLANKey{e, asset}
	d.mu.Lock()
	cached, ok := d.cache[key]
	d.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.hostport, cached.hostport != ""
	}
	devices, err := d.browse(ctx)
	if err != nil {
		return "", false
	}
	hostport := ""
	for _, device := range devices {
		if device.OrgID != e.OrganizationID || device.AssetID != asset || !device.IsMTLS || device.IPAddress == "" || device.Port <= 0 || strings.EqualFold(device.IPAddress, "localhost") {
			continue
		}
		if ip := net.ParseIP(device.IPAddress); ip != nil && ip.IsLoopback() {
			continue
		}
		hostport = net.JoinHostPort(device.IPAddress, strconv.Itoa(device.Port))
		break
	}
	d.mu.Lock()
	if d.cache == nil {
		d.cache = make(map[legacyLANKey]lanCacheEntry)
	}
	d.cache[key] = lanCacheEntry{hostport: hostport, expires: time.Now().Add(lanCacheTTL)}
	d.mu.Unlock()
	return hostport, hostport != ""
}

// Reserve time for Cloud fallback even when a discovered LAN endpoint stalls.
// Discovery has its own scan timer; this budget covers only connection setup.
func legacyLANContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := 3 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	return context.WithTimeout(ctx, budget)
}

// A batch scan must settle on a usable peer, not the dialing host or another
// enrollment's advertisements. TLS still authenticates every dial afterward.
type legacyPeerFilter struct{ org, self int32 }

func (f legacyPeerFilter) Exclude(device models.LANDevice) bool {
	return device.OrgID != f.org || device.AssetID <= 0 || device.AssetID == f.self || !device.IsMTLS
}
func (legacyPeerFilter) Changed() <-chan struct{} { return nil }
