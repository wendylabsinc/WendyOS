package services

import (
	"context"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type bluetoothRadioManager interface {
	BluetoothRadio(context.Context) (*pb.BluetoothRadioStatus, error)
	ConfigureBluetoothRadio(context.Context, *pb.ConfigureBluetoothRadioRequest) (*pb.BluetoothRadioStatus, error)
}

func (s *BluetoothService) GetBluetoothRadio(ctx context.Context, _ *pb.GetBluetoothRadioRequest) (*pb.BluetoothRadioStatus, error) {
	m, ok := s.bluetoothManager.(bluetoothRadioManager)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Bluetooth radio controls are unavailable on this platform")
	}
	return m.BluetoothRadio(ctx)
}
func (s *BluetoothService) ConfigureBluetoothRadio(ctx context.Context, r *pb.ConfigureBluetoothRadioRequest) (*pb.BluetoothRadioStatus, error) {
	m, ok := s.bluetoothManager.(bluetoothRadioManager)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Bluetooth radio controls are unavailable on this platform")
	}
	return m.ConfigureBluetoothRadio(ctx, r)
}
