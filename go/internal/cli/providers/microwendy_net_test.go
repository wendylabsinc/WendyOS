package providers

import (
	"context"
	"strings"
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
	var desired *pb.WendyComNetConfig
	if f.config != nil {
		desired = proto.Clone(f.config).(*pb.WendyComNetConfig)
	}
	return &pb.WendyComNetStatus{Desired: desired, BleSupported: true}, nil
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

func TestLiteNetProviderPreservesExistingTransport(t *testing.T) {
	old := connectLiteNetClient
	t.Cleanup(func() { connectLiteNetClient = old })
	for _, transport := range []string{"USB", "LAN", "BLE", "Cloud"} {
		t.Run(transport, func(t *testing.T) {
			device := models.ExternalDevice{ID: "selected-device", ConnectionInfo: map[string]string{"type": transport, "address": "retained", "assetId": "retained"}}
			fixture := &netSettingsFixture{config: &pb.WendyComNetConfig{MaxLinks: 2, BleLinks: 2}}
			connectLiteNetClient = func(_ context.Context, _ *MicroWendyProvider, got models.ExternalDevice) (liteNetClient, error) {
				if got.ID != device.ID || got.ConnectionType() != transport || got.ConnectionInfo["address"] != "retained" || got.ConnectionInfo["assetId"] != "retained" {
					t.Fatalf("transport replaced: %+v", got)
				}
				return fixture, nil
			}
			provider := &MicroWendyProvider{}
			if _, err := provider.NetStatus(context.Background(), device); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.ConfigureNet(context.Background(), device, func(config *pb.WendyComNetConfig) error { config.Enabled = true; return nil }); err != nil {
				t.Fatal(err)
			}
			if fixture.closed != 2 || len(fixture.calls) != 3 || !fixture.config.Enabled || fixture.config.BleLinks != 2 {
				t.Fatalf("unexpected calls/config: %+v", fixture)
			}
		})
	}
}

func TestLiteNetProviderRefusesUpdateWithoutSavedSettings(t *testing.T) {
	old := connectLiteNetClient
	t.Cleanup(func() { connectLiteNetClient = old })
	fixture := &netSettingsFixture{}
	connectLiteNetClient = func(context.Context, *MicroWendyProvider, models.ExternalDevice) (liteNetClient, error) {
		return fixture, nil
	}
	_, err := (&MicroWendyProvider{}).ConfigureNet(context.Background(), models.ExternalDevice{}, func(*pb.WendyComNetConfig) error { t.Fatal("invoked update without saved settings"); return nil })
	if err == nil || !strings.Contains(err.Error(), "no saved") || len(fixture.calls) != 1 || fixture.closed != 1 {
		t.Fatalf("missing policy update = %v, %+v", err, fixture)
	}
}
