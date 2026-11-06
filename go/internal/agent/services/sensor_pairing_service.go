package services

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StartPairingFunc launches (or restarts) a supervisor goroutine for a pairing.
type StartPairingFunc func(p mcusource.SensorPairing, addr string)

// StopPairingFunc cancels a running supervisor.
type StopPairingFunc func(mcusource.SensorPairing)

// IsConnectedFunc reports whether a pairing is delivering sensor frames.
type IsConnectedFunc func(mcusource.SensorPairing) bool

// SensorSourceBrowseFunc browses this device's network for at most timeout
// and returns the sensor sources it saw, leaving out the sightings exclude
// matches. discovery.DiscoverSensorSources is the production implementation.
type SensorSourceBrowseFunc func(ctx context.Context, timeout time.Duration, exclude func(models.LANDevice) bool) ([]models.LANDevice, error)

// SensorSourceSelf identifies this device's own mDNS advertisement, so
// DiscoverSensorSources never offers the device as a source for itself.
type SensorSourceSelf struct {
	// AssetID is this device's legacy cloud asset ID; 0 before enrollment or
	// with a PKI-only identity.
	AssetID int32
	// LocalIPs are this device's interface addresses. A sighting whose primary
	// address is one of them, or a loopback address, is this device.
	LocalIPs []net.IP
}

// LocalSensorSourceSelf pairs assetID with this host's current interface
// addresses.
func LocalSensorSourceSelf(assetID int32) SensorSourceSelf {
	self := SensorSourceSelf{AssetID: assetID}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return self
	}
	for _, addr := range addrs {
		switch a := addr.(type) {
		case *net.IPNet:
			self.LocalIPs = append(self.LocalIPs, a.IP)
		case *net.IPAddr:
			self.LocalIPs = append(self.LocalIPs, a.IP)
		}
	}
	return self
}

// matches reports whether d is this device's own advertisement. Hostnames are
// not compared: avahi renames a host on a name conflict, so another device
// can be published under this device's own hostname.
func (own SensorSourceSelf) matches(d models.LANDevice) bool {
	// This device advertises itself on _wendyos._udp, never as a Wendy Lite
	// board, so an asset ID only identifies it among WendyOS agents.
	if !d.WendyLite && own.AssetID > 0 && d.AssetID == own.AssetID {
		return true
	}
	ip, ok := sensorSourceIP(d.IPAddress)
	if !ok {
		return false
	}
	if ip == nil || ip.IsLoopback() {
		return true
	}
	for _, local := range own.LocalIPs {
		if local.Equal(ip) {
			return true
		}
	}
	return false
}

// sensorSourceIP parses an mDNS address, dropping any IPv6 zone. ok is false
// when the address is empty or not an IP literal. A nil IP with ok true is the
// "localhost" name some avahi configurations publish, i.e. loopback.
func sensorSourceIP(addr string) (ip net.IP, ok bool) {
	if strings.EqualFold(addr, "localhost") {
		return nil, true
	}
	if i := strings.IndexByte(addr, '%'); i >= 0 {
		addr = addr[:i]
	}
	ip = net.ParseIP(addr)
	return ip, ip != nil
}

const (
	// defaultSensorSourceScan and maxSensorSourceScan bound how long one
	// DiscoverSensorSources call browses the network.
	defaultSensorSourceScan = 5 * time.Second
	maxSensorSourceScan     = 10 * time.Second
	// sensorSourceReplyMargin is the part of the call's deadline the scan
	// leaves for the reply to reach the client, including through the cloud
	// tunnel.
	sensorSourceReplyMargin = time.Second
	// maxConcurrentSensorSourceScans bounds the mDNS browses concurrent
	// callers can run; further callers wait for a slot.
	maxConcurrentSensorSourceScans = 2
)

type SensorPairingService struct {
	agentpbv2.UnimplementedWendySensorPairingServiceServer
	logger      *zap.Logger
	store       *mcusource.PairingStore
	agentScope  func() certs.Scope
	start       StartPairingFunc
	stop        StopPairingFunc
	isConnected IsConnectedFunc
	browse      SensorSourceBrowseFunc
	self        func() SensorSourceSelf
	scans       chan struct{}
}

// agentScope reads this agent's current identity on every Add so pairing
// follows provisioning changes without a restart. The dialer independently
// verifies the source certificate against the current consumer identity.
//
// Source discovery browses with discovery.DiscoverSensorSources and recognizes
// this device by its interface addresses alone until SetSourceDiscovery
// supplies its asset ID.
func NewSensorPairingService(logger *zap.Logger, store *mcusource.PairingStore, agentScope func() certs.Scope, start StartPairingFunc, stop StopPairingFunc, isConnected IsConnectedFunc) *SensorPairingService {
	return &SensorPairingService{
		logger: logger, store: store, agentScope: agentScope, start: start, stop: stop, isConnected: isConnected,
		browse: discovery.DiscoverSensorSources,
		self:   func() SensorSourceSelf { return LocalSensorSourceSelf(0) },
		scans:  make(chan struct{}, maxConcurrentSensorSourceScans),
	}
}

// SetSourceDiscovery replaces how DiscoverSensorSources browses the network
// and recognizes this device. A nil argument keeps the current function. self
// is called on every request, so it follows enrollment and address changes.
// Call it before the service starts serving.
func (s *SensorPairingService) SetSourceDiscovery(browse SensorSourceBrowseFunc, self func() SensorSourceSelf) {
	if browse != nil {
		s.browse = browse
	}
	if self != nil {
		s.self = self
	}
}

func (s *SensorPairingService) AddSensorPairing(_ context.Context, req *agentpbv2.AddSensorPairingRequest) (*agentpbv2.AddSensorPairingResponse, error) {
	scope := s.agentScope()
	p := mcusource.SensorPairing{
		SourceAssetID:      req.SourceAssetId,
		SourcePrincipalURI: req.SourcePrincipalUri,
		OrgID:              scope.OrgID,
		Name:               req.Name,
		SensorAllowlist:    req.SensorAllowlist,
		Transport:          req.Transport,
	}
	if err := p.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if p.SourcePrincipalURI != "" {
		identity, _ := certs.ParsePrincipal(p.SourcePrincipalURI)
		if !identity.Scope().Matches(scope) {
			return nil, status.Error(codes.PermissionDenied, "sensor source must belong to this device's tenant")
		}
	}
	if err := s.store.Add(p); err != nil {
		return nil, err
	}
	if s.start != nil {
		s.start(p, req.SourceAddress)
	}
	// The supervisor has not delivered a frame yet.
	return &agentpbv2.AddSensorPairingResponse{Pairing: toProto(p, false)}, nil
}

func (s *SensorPairingService) RemoveSensorPairing(_ context.Context, req *agentpbv2.RemoveSensorPairingRequest) (*agentpbv2.RemoveSensorPairingResponse, error) {
	p := mcusource.SensorPairing{SourceAssetID: req.SourceAssetId, SourcePrincipalURI: req.SourcePrincipalUri, Transport: "wendycom"}
	if err := p.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.stop != nil {
		s.stop(p)
	}
	if err := s.store.RemovePairing(p); err != nil {
		return nil, err
	}
	return &agentpbv2.RemoveSensorPairingResponse{}, nil
}

func (s *SensorPairingService) ListSensorPairings(_ context.Context, _ *agentpbv2.ListSensorPairingsRequest) (*agentpbv2.ListSensorPairingsResponse, error) {
	list := s.store.List()
	out := make([]*agentpbv2.SensorPairing, 0, len(list))
	for _, p := range list {
		connected := s.isConnected != nil && s.isConnected(p)
		out = append(out, toProto(p, connected))
	}
	return &agentpbv2.ListSensorPairingsResponse{Pairings: out, SupportsPrincipalPairing: true}, nil
}

// DiscoverSensorSources browses this device's own network for sensor sources,
// so a client anywhere sees what this device can reach rather than what is on
// the client's network. Results carry addresses as seen from here. Their
// identity fields are TXT hints only: AddSensorPairing's transports verify the
// source certificate when they connect.
func (s *SensorPairingService) DiscoverSensorSources(ctx context.Context, req *agentpbv2.DiscoverSensorSourcesRequest) (*agentpbv2.DiscoverSensorSourcesResponse, error) {
	select {
	case s.scans <- struct{}{}:
		defer func() { <-s.scans }()
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	budget := sensorSourceScanBudget(ctx, req.GetTimeoutMs())
	if budget <= 0 {
		return nil, status.Error(codes.DeadlineExceeded, "not enough time left to scan for sensor sources")
	}
	self := s.self()
	// The browse drops this device as it goes: its own advertisement is the
	// first answer every browse here gets, and counting it would end the scan
	// before other agents reply. The loop below still filters it out.
	found, err := s.browse(ctx, budget, self.matches)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, status.FromContextError(ctxErr).Err()
	}
	if err != nil {
		s.logger.Warn("sensor source discovery failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "sensor source discovery failed on this device")
	}
	sources := make([]*agentpbv2.DiscoveredSensorSource, 0, len(found))
	for _, d := range found {
		if !d.Sensorlink || self.matches(d) {
			continue
		}
		sources = append(sources, sensorSourceToProto(d))
	}
	sort.SliceStable(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		if a.DisplayName != b.DisplayName {
			return a.DisplayName < b.DisplayName
		}
		if a.Id != b.Id {
			return a.Id < b.Id
		}
		return a.IpAddress < b.IpAddress
	})
	return &agentpbv2.DiscoverSensorSourcesResponse{Sources: sources}, nil
}

// sensorSourceScanBudget is how long to browse: the requested time (or the
// default), capped, and short enough to answer before ctx's deadline.
func sensorSourceScanBudget(ctx context.Context, timeoutMS uint32) time.Duration {
	budget := defaultSensorSourceScan
	if timeoutMS > 0 {
		budget = min(time.Duration(timeoutMS)*time.Millisecond, maxSensorSourceScan)
	}
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)-sensorSourceReplyMargin)
	}
	return budget
}

func sensorSourceToProto(d models.LANDevice) *agentpbv2.DiscoveredSensorSource {
	var addresses []string
	for _, addr := range d.Addresses {
		// A loopback address reaches this device, never the source.
		if ip, ok := sensorSourceIP(addr); ok && (ip == nil || ip.IsLoopback()) {
			continue
		}
		addresses = append(addresses, addr)
	}
	return &agentpbv2.DiscoveredSensorSource{
		Id:           d.ID,
		DisplayName:  d.DisplayName,
		Hostname:     d.Hostname,
		IpAddress:    d.IPAddress,
		Addresses:    addresses,
		Port:         int32(d.Port),
		IsMtls:       d.IsMTLS,
		AssetId:      d.AssetID,
		OrgId:        d.OrgID,
		TenantUuid:   d.TenantUUID,
		PrincipalUri: d.PrincipalURI,
		Caps:         d.Caps,
		WendyLite:    d.WendyLite,
	}
}

func toProto(p mcusource.SensorPairing, connected bool) *agentpbv2.SensorPairing {
	return &agentpbv2.SensorPairing{
		SourceAssetId:      p.SourceAssetID,
		SourcePrincipalUri: p.SourcePrincipalURI,
		OrgId:              p.OrgID,
		Name:               p.Name,
		SensorAllowlist:    p.SensorAllowlist,
		Connected:          connected,
		Transport:          p.Transport,
	}
}
