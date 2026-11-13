package services

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *VideoService) GetCameraDHCPStatus(_ context.Context, _ *agentpb.GetCameraDHCPStatusRequest) (*agentpb.CameraDHCPStatus, error) {
	if s.links == nil {
		return nil, status.Error(codes.Unavailable, "camera link management is unavailable")
	}
	return cameraDHCPStatusProto(s.links.DHCPStatus()), nil
}

func (s *VideoService) SetCameraDHCP(_ context.Context, req *agentpb.SetCameraDHCPRequest) (*agentpb.CameraDHCPStatus, error) {
	if s.links == nil {
		return nil, status.Error(codes.Unavailable, "camera link management is unavailable")
	}
	result, err := s.links.SetDHCPEnabled(req.GetInterfaceName(), req.GetEnabled())
	if err != nil {
		code := codes.FailedPrecondition
		if errors.Is(err, ipcam.ErrInvalidDHCPInterface) {
			code = codes.InvalidArgument
		} else if errors.Is(err, ipcam.ErrDHCPInterfaceMissing) {
			code = codes.NotFound
		}
		return nil, status.Error(code, err.Error())
	}
	return cameraDHCPStatusProto(result), nil
}

func cameraDHCPStatusProto(s ipcam.DHCPStatus) *agentpb.CameraDHCPStatus {
	out := &agentpb.CameraDHCPStatus{ConfigurationError: s.ConfigurationError, InterfaceError: s.InterfaceError}
	for _, iface := range s.Interfaces {
		out.Interfaces = append(out.Interfaces, &agentpb.CameraDHCPInterfaceStatus{
			InterfaceName: iface.Name, Enabled: iface.Enabled, Serving: iface.Serving,
			Present: iface.Present, Carrier: iface.Carrier, Up: iface.Up,
			Ipv4Addresses: iface.IPv4s, State: iface.State, Detail: iface.Detail,
		})
	}
	return out
}
