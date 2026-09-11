package services

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

// StartPairingFunc launches (or restarts) a supervisor goroutine for a pairing.
type StartPairingFunc func(p mcusource.SensorPairing, addr string)

// StopPairingFunc cancels a running supervisor.
type StopPairingFunc func(sourceAssetID int32)

// IsConnectedFunc reports whether a pairing is delivering sensor frames.
type IsConnectedFunc func(sourceAssetID int32) bool

type SensorPairingService struct {
	agentpbv2.UnimplementedWendySensorPairingServiceServer
	logger      *zap.Logger
	store       *mcusource.PairingStore
	agentOrgID  func() int32
	start       StartPairingFunc
	stop        StopPairingFunc
	isConnected IsConnectedFunc
}

// agentOrgID returns this agent's own current provisioning org id, read
// fresh on every Add (not captured once at construction) so a device
// provisioned or re-provisioned while the agent runs gets the right org
// without a restart. Sensor pairing is same-org by design (no org_id field
// on the request), so every pairing's SensorPairing.OrgID is set to the
// agent's own org — that's the identity the per-pairing mTLS dialer pins
// against on the handshake, so a wrong org here means no real source can
// ever connect.
func NewSensorPairingService(logger *zap.Logger, store *mcusource.PairingStore, agentOrgID func() int32, start StartPairingFunc, stop StopPairingFunc, isConnected IsConnectedFunc) *SensorPairingService {
	return &SensorPairingService{logger: logger, store: store, agentOrgID: agentOrgID, start: start, stop: stop, isConnected: isConnected}
}

func (s *SensorPairingService) AddSensorPairing(_ context.Context, req *agentpbv2.AddSensorPairingRequest) (*agentpbv2.AddSensorPairingResponse, error) {
	p := mcusource.SensorPairing{
		SourceAssetID:   req.SourceAssetId,
		OrgID:           s.agentOrgID(),
		Name:            req.Name,
		SensorAllowlist: req.SensorAllowlist,
		Transport:       req.Transport,
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
	if s.stop != nil {
		s.stop(req.SourceAssetId)
	}
	if err := s.store.Remove(req.SourceAssetId); err != nil {
		return nil, err
	}
	return &agentpbv2.RemoveSensorPairingResponse{}, nil
}

func (s *SensorPairingService) ListSensorPairings(_ context.Context, _ *agentpbv2.ListSensorPairingsRequest) (*agentpbv2.ListSensorPairingsResponse, error) {
	list := s.store.List()
	out := make([]*agentpbv2.SensorPairing, 0, len(list))
	for _, p := range list {
		connected := s.isConnected != nil && s.isConnected(p.SourceAssetID)
		out = append(out, toProto(p, connected))
	}
	return &agentpbv2.ListSensorPairingsResponse{Pairings: out}, nil
}

func toProto(p mcusource.SensorPairing, connected bool) *agentpbv2.SensorPairing {
	return &agentpbv2.SensorPairing{
		SourceAssetId:   p.SourceAssetID,
		OrgId:           p.OrgID,
		Name:            p.Name,
		SensorAllowlist: p.SensorAllowlist,
		Connected:       connected,
		Transport:       p.Transport,
	}
}
