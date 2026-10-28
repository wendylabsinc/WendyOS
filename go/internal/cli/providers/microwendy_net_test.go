package providers

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

type netSettingsFixture struct {
	calls  []string
	config *pb.WendyComNetConfig
	closed int
}

func (f *netSettingsFixture) GetNetStatus(timeout time.Duration) (*pb.WendyComNetStatus, error) {
	f.calls = append(f.calls, "get")
	return &pb.WendyComNetStatus{Desired: proto.Clone(f.config).(*pb.WendyComNetConfig), BleSupported: true}, nil
}
func (f *netSettingsFixture) SetNetConfig(config *pb.WendyComNetConfig, timeout time.Duration) (*pb.WendyComNetStatus, error) {
	f.calls = append(f.calls, "set")
	f.config = proto.Clone(config).(*pb.WendyComNetConfig)
	return &pb.WendyComNetStatus{Desired: f.config, BleSupported: true}, nil
}
func (f *netSettingsFixture) Close() error { f.closed++; return nil }
func TestPhysicalLiteBLEProviderConfiguresTwoLinksExplicitly(t *testing.T) {
	old := connectLiteNetClient
	t.Cleanup(func() { connectLiteNetClient = old })
	fixture := &netSettingsFixture{config: &pb.WendyComNetConfig{MaxLinks: 1, BleLinks: 1, WifiLinks: 1, NanLinks: 1}}
	const selector = "wendy-lite:ble:1B2C3D4E-0000-0000-0000-000000000000"
	device := models.ExternalDevice{ID: selector, ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "BLE", "address": "1B2C3D4E-0000-0000-0000-000000000000"}}
	ctx := context.Background()
	connectLiteNetClient = func(got context.Context, _ *MicroWendyProvider, d models.ExternalDevice) (liteNetClient, error) {
		if got != ctx || d.ID != selector || d.ConnectionType() != "BLE" {
			t.Fatalf("transport changed: %+v", d)
		}
		return fixture, nil
	}
	provider := &MicroWendyProvider{}
	initial, err := provider.NetStatus(ctx, device)
	if err != nil || initial.Desired.Enabled || initial.WifiSupported || fixture.closed != 1 || len(fixture.calls) != 1 {
		t.Fatalf("read-only status mutated policy: %v %v", initial, err)
	}
	status, err := provider.ConfigureNet(ctx, device, func(config *pb.WendyComNetConfig) error {
		config.Enabled = true
		config.Ble = true
		config.MaxLinks = 2
		config.BleLinks = 2
		return nil
	})
	if err != nil || !status.Desired.Enabled || !status.Desired.Ble || status.Desired.InfrastructureWifi || status.Desired.Nan || status.Desired.MaxLinks != 2 || status.Desired.BleLinks != 2 || status.Desired.WifiLinks != 1 || status.Desired.NanLinks != 1 || status.WifiSupported || status.NanSupported {
		t.Fatalf("typed two-BLE update changed other settings: %v %v", status, err)
	}
	if fixture.closed != 2 || len(fixture.calls) != 3 || fixture.calls[1] != "get" || fixture.calls[2] != "set" {
		t.Fatalf("management sequence %+v closes%d", fixture.calls, fixture.closed)
	}
}
