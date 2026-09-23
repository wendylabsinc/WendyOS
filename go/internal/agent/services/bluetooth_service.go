package services

import (
	"context"
	"errors"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/bluetooth"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// btStatusError maps a BluetoothManager failure to a gRPC status, shared by
// the v1 and v2 bluetooth handlers. The manager already produces user-readable
// text (the CLI shows only the status message), so the error text passes
// through; a missing device maps to NotFound so clients can tell "rescan"
// apart from a genuine connection failure.
func btStatusError(op string, err error) error {
	if errors.Is(err, bluetooth.ErrDeviceNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Errorf(codes.Internal, "failed to %s: %v", op, err)
}

type BluetoothService struct {
	agentpbv2.UnimplementedWendyBluetoothServiceServer
	logger           *zap.Logger
	bluetoothManager BluetoothManager
}

func NewBluetoothService(logger *zap.Logger, bm BluetoothManager) *BluetoothService {
	return &BluetoothService{logger: logger, bluetoothManager: bm}
}

func (s *BluetoothService) ScanBluetoothPeripherals(stream grpc.BidiStreamingServer[agentpbv2.ScanBluetoothPeripheralsRequest, agentpbv2.ScanBluetoothPeripheralsResponse]) error {
	ctx := stream.Context()
	ch, err := s.bluetoothManager.Scan(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start bluetooth scan: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case peripherals, ok := <-ch:
			if !ok {
				return nil
			}
			v2devs := make([]*agentpbv2.DiscoveredBluetoothPeripheral, len(peripherals))
			for i, p := range peripherals {
				v2devs[i] = mapBluetoothPeripheralToV2(p)
			}
			if err := stream.Send(&agentpbv2.ScanBluetoothPeripheralsResponse{DiscoveredDevices: v2devs}); err != nil {
				return err
			}
		}
	}
}

func (s *BluetoothService) ConnectBluetoothPeripheral(ctx context.Context, req *agentpbv2.ConnectBluetoothPeripheralRequest) (*agentpbv2.ConnectBluetoothPeripheralResponse, error) {
	paired, err := s.bluetoothManager.Connect(ctx, req.GetAddress(), req.GetPair(), req.GetTrust())
	if err != nil {
		return nil, btStatusError("connect bluetooth peripheral", err)
	}
	return &agentpbv2.ConnectBluetoothPeripheralResponse{Paired: &paired}, nil
}

func (s *BluetoothService) DisconnectBluetoothPeripheral(ctx context.Context, req *agentpbv2.DisconnectBluetoothPeripheralRequest) (*agentpbv2.DisconnectBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Disconnect(ctx, req.GetAddress()); err != nil {
		return nil, btStatusError("disconnect bluetooth peripheral", err)
	}
	return &agentpbv2.DisconnectBluetoothPeripheralResponse{}, nil
}

func (s *BluetoothService) ForgetBluetoothPeripheral(ctx context.Context, req *agentpbv2.ForgetBluetoothPeripheralRequest) (*agentpbv2.ForgetBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Forget(ctx, req.GetAddress()); err != nil {
		return nil, btStatusError("forget bluetooth peripheral", err)
	}
	return &agentpbv2.ForgetBluetoothPeripheralResponse{}, nil
}

func mapBluetoothPeripheralToV2(p *agentpb.DiscoveredBluetoothPeripheral) *agentpbv2.DiscoveredBluetoothPeripheral {
	return &agentpbv2.DiscoveredBluetoothPeripheral{
		Name:       p.Name,
		Address:    p.Address,
		Rssi:       p.Rssi,
		DeviceType: p.DeviceType,
		Paired:     p.Paired,
		Connected:  p.Connected,
		Trusted:    p.Trusted,

		SupervisionTimeoutMs:          p.SupervisionTimeoutMs,
		RequestedSupervisionTimeoutMs: p.RequestedSupervisionTimeoutMs,
	}
}
