package providers

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

type litePowerClient interface {
	GetBluetoothPower(time.Duration) (*pb.WendyComBluetoothPowerStatus, error)
	SetBluetoothPower(*pb.WendyComBluetoothPowerConfig, time.Duration) (*pb.WendyComBluetoothPowerStatus, error)
	Close() error
}

var connectLitePowerClient = func(ctx context.Context, p *MicroWendyProvider, device models.ExternalDevice) (litePowerClient, error) {
	return p.connectClient(ctx, device)
}

func (p *MicroWendyProvider) BluetoothPower(ctx context.Context, device models.ExternalDevice, update *pb.WendyComBluetoothPowerConfig) (*pb.WendyComBluetoothPowerStatus, error) {
	client, err := connectLitePowerClient(ctx, p, device)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	if update == nil {
		return client.GetBluetoothPower(10 * time.Second)
	}
	return client.SetBluetoothPower(update, 10*time.Second)
}
