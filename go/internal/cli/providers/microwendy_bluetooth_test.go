package providers

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"testing"
	"time"
)

type powerFixture struct {
	gets, sets, closed int
	patch              *pb.WendyComBluetoothPowerConfig
}

func (f *powerFixture) GetBluetoothPower(time.Duration) (*pb.WendyComBluetoothPowerStatus, error) {
	f.gets++
	return &pb.WendyComBluetoothPowerStatus{}, nil
}
func (f *powerFixture) SetBluetoothPower(c *pb.WendyComBluetoothPowerConfig, _ time.Duration) (*pb.WendyComBluetoothPowerStatus, error) {
	f.sets++
	f.patch = c
	return &pb.WendyComBluetoothPowerStatus{}, nil
}
func (f *powerFixture) Close() error { f.closed++; return nil }
func TestLitePowerProviderSendsOnlyExplicitFieldsWithoutStaleRead(t *testing.T) {
	old := connectLitePowerClient
	t.Cleanup(func() { connectLitePowerClient = old })
	f := &powerFixture{}
	connectLitePowerClient = func(_ context.Context, _ *MicroWendyProvider, device models.ExternalDevice) (litePowerClient, error) {
		if device.ConnectionInfo["type"] != "USB" {
			t.Fatal("changed management transport", device)
		}
		return f, nil
	}
	p := &MicroWendyProvider{}
	device := models.ExternalDevice{ConnectionInfo: map[string]string{"type": "USB"}}
	_, err := p.BluetoothPower(context.Background(), device, &pb.WendyComBluetoothPowerConfig{Connections: &pb.WendyComBluetoothPowerSetting{Mode: pb.WendyComBluetoothPowerMode_WENDY_COM_BT_POWER_MAXIMUM}})
	if err != nil || f.gets != 0 || f.sets != 1 || f.patch.Advertising != nil || f.patch.Connections == nil || f.closed != 1 {
		t.Fatal(f, err)
	}
	_, err = p.BluetoothPower(context.Background(), device, nil)
	if err != nil || f.gets != 1 || f.sets != 1 || f.closed != 2 {
		t.Fatal(f, err)
	}
}
