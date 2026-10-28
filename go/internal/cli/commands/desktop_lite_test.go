package commands

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestDesktopCloudLiteUsesWendyComInsteadOfAgent(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldSelect, oldConnect := connectCloudDeviceSelectorFn, desktopConnectLite
	t.Cleanup(func() { connectCloudDeviceSelectorFn, desktopConnectLite = oldSelect, oldConnect })
	const asset = "7791d76e-a942-4a8e-b582-063d063b5623"
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	device := models.ExternalDevice{ID: "wendy-lite:cloud:" + asset, ProviderKey: "wendy-lite", DisplayName: "esp-camera-01", ConnectionInfo: map[string]string{"type": "Cloud", "assetId": asset, "tenantId": tenant}}
	connectCloudDeviceSelectorFn = func(_ context.Context, selector cloudDeviceSelector) (*SelectedDevice, error) {
		if selector.TenantUUID != tenant {
			t.Fatal("lost selected tenant")
		}
		return &SelectedDevice{External: &device, Provider: &providers.MicroWendyProvider{}}, nil
	}
	expected := liteclient.NewWendyLiteClient()
	desktopConnectLite = func(ctx context.Context, provider *providers.MicroWendyProvider, got models.ExternalDevice) (*liteclient.WendyLiteClient, error) {
		if provider.Key() != "wendy-lite" || got.ID != device.ID || got.ConnectionInfo["assetId"] != asset {
			t.Fatal("changed selected Lite target")
		}
		return expected, nil
	}
	agent, lite, err := DesktopConnectTarget(context.Background(), "cloud://cloud.example:443/tenant/"+tenant+"/asset/"+asset)
	if err != nil || agent != nil || lite != expected {
		t.Fatalf("connection = %v, %v, %v", agent, lite, err)
	}
	// The separate VPN/HTTP adapter must retain its agent-only restriction.
	if _, err := DesktopConnect(context.Background(), "cloud://cloud.example:443/tenant/"+tenant+"/asset/"+asset); err == nil {
		t.Fatal("Lite accepted as an agent tunnel")
	}
}
