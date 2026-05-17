package services

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

type BluetoothService struct {
	agentpbv2.UnimplementedWendyBluetoothServiceServer
	logger           *zap.Logger
	bluetoothManager BluetoothManager
}

func NewBluetoothService(logger *zap.Logger, bm BluetoothManager) *BluetoothService {
	return &BluetoothService{logger: logger, bluetoothManager: bm}
}

func (s *BluetoothService) ScanBluetoothPeripherals(_ *agentpbv2.ScanBluetoothPeripheralsRequest, stream grpc.ServerStreamingServer[agentpbv2.ScanBluetoothPeripheralsResponse]) error {
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
	if err := s.bluetoothManager.Connect(ctx, req.GetAddress(), req.GetPair(), req.GetTrust()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to connect bluetooth peripheral: %v", err)
	}
	return &agentpbv2.ConnectBluetoothPeripheralResponse{}, nil
}

func (s *BluetoothService) DisconnectBluetoothPeripheral(ctx context.Context, req *agentpbv2.DisconnectBluetoothPeripheralRequest) (*agentpbv2.DisconnectBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Disconnect(ctx, req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to disconnect bluetooth peripheral: %v", err)
	}
	return &agentpbv2.DisconnectBluetoothPeripheralResponse{}, nil
}

func (s *BluetoothService) ForgetBluetoothPeripheral(ctx context.Context, req *agentpbv2.ForgetBluetoothPeripheralRequest) (*agentpbv2.ForgetBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Forget(ctx, req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to forget bluetooth peripheral: %v", err)
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
	}
}
