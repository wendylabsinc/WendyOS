package services

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

type WiFiService struct {
	agentpbv2.UnimplementedWendyWiFiServiceServer
	logger         *zap.Logger
	networkManager NetworkManager
}

func NewWiFiService(logger *zap.Logger, nm NetworkManager) *WiFiService {
	return &WiFiService{logger: logger, networkManager: nm}
}

func (s *WiFiService) ListWiFiNetworks(ctx context.Context, _ *agentpbv2.ListWiFiNetworksRequest) (*agentpbv2.ListWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	networks, err := s.networkManager.ListWiFiNetworks(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list WiFi networks: %v", err)
	}
	v2nets := make([]*agentpbv2.ListWiFiNetworksResponse_WiFiNetwork, len(networks))
	for i, n := range networks {
		v2nets[i] = mapWiFiNetworkToV2(n)
	}
	return &agentpbv2.ListWiFiNetworksResponse{Networks: v2nets}, nil
}

func (s *WiFiService) ConnectToWiFi(ctx context.Context, req *agentpbv2.ConnectToWiFiRequest) (*agentpbv2.ConnectToWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	v1req := &agentpb.ConnectToWiFiRequest{Ssid: req.Ssid}
	if req.Password != nil {
		v1req.Password = *req.Password
	}
	if req.Security != nil {
		sec := agentpb.WiFiSecurityType(*req.Security)
		v1req.Security = &sec
	}
	if req.Hidden != nil {
		v1req.Hidden = req.Hidden
	}
	if err := s.networkManager.ConnectToWiFi(ctx, v1req); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to connect to WiFi: %v", err)
	}
	return &agentpbv2.ConnectToWiFiResponse{}, nil
}

func (s *WiFiService) GetWiFiStatus(ctx context.Context, _ *agentpbv2.GetWiFiStatusRequest) (*agentpbv2.GetWiFiStatusResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	connected, ssid, err := s.networkManager.GetWiFiStatus(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get WiFi status: %v", err)
	}
	resp := &agentpbv2.GetWiFiStatusResponse{Connected: connected}
	if connected && ssid != "" {
		resp.Ssid = &ssid
	}
	return resp, nil
}

func (s *WiFiService) DisconnectWiFi(ctx context.Context, _ *agentpbv2.DisconnectWiFiRequest) (*agentpbv2.DisconnectWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.DisconnectWiFi(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to disconnect WiFi: %v", err)
	}
	return &agentpbv2.DisconnectWiFiResponse{}, nil
}

func (s *WiFiService) ListKnownWiFiNetworks(ctx context.Context, _ *agentpbv2.ListKnownWiFiNetworksRequest) (*agentpbv2.ListKnownWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	known, err := s.networkManager.ListKnownWiFiNetworks(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list known WiFi networks: %v", err)
	}
	v2known := make([]*agentpbv2.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, len(known))
	for i, k := range known {
		v2known[i] = &agentpbv2.ListKnownWiFiNetworksResponse_KnownWiFiNetwork{
			Ssid:     k.Ssid,
			Uuid:     k.Uuid,
			Priority: k.Priority,
			Security: agentpbv2.WiFiSecurityType(k.Security),
		}
	}
	return &agentpbv2.ListKnownWiFiNetworksResponse{Networks: v2known}, nil
}

func (s *WiFiService) SetWiFiNetworkPriority(ctx context.Context, req *agentpbv2.SetWiFiNetworkPriorityRequest) (*agentpbv2.SetWiFiNetworkPriorityResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.SetWiFiNetworkPriorityByUUID(ctx, req.GetUuid(), req.GetPriority()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to set WiFi network priority: %v", err)
	}
	return &agentpbv2.SetWiFiNetworkPriorityResponse{}, nil
}

func (s *WiFiService) ReorderKnownWiFiNetworks(ctx context.Context, req *agentpbv2.ReorderKnownWiFiNetworksRequest) (*agentpbv2.ReorderKnownWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ReorderKnownWiFiNetworksByUUID(ctx, req.GetOrderUuids()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to reorder WiFi networks: %v", err)
	}
	return &agentpbv2.ReorderKnownWiFiNetworksResponse{}, nil
}

func (s *WiFiService) ForgetWiFiNetwork(ctx context.Context, req *agentpbv2.ForgetWiFiNetworkRequest) (*agentpbv2.ForgetWiFiNetworkResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ForgetWiFiNetworkByUUID(ctx, req.GetUuid()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to forget WiFi network: %v", err)
	}
	return &agentpbv2.ForgetWiFiNetworkResponse{}, nil
}

func mapWiFiNetworkToV2(n *agentpb.ListWiFiNetworksResponse_WiFiNetwork) *agentpbv2.ListWiFiNetworksResponse_WiFiNetwork {
	return &agentpbv2.ListWiFiNetworksResponse_WiFiNetwork{
		Ssid:           n.Ssid,
		SignalStrength: n.SignalStrength,
		Security:       agentpbv2.WiFiSecurityType(n.Security),
		IsKnown:        n.IsKnown,
		IsConnected:    n.IsConnected,
		Priority:       n.Priority,
		RssiDbm:        n.RssiDbm,
	}
}
