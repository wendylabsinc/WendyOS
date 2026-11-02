package commands

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestExplicitPhysicalLiteBLESelectionBypassesProviderDiscovery(t *testing.T) {
	oldResolve, oldFind := resolveLiteBLESelector, findDeviceByIDFn
	t.Cleanup(func() { resolveLiteBLESelector, findDeviceByIDFn = oldResolve, oldFind })
	const selector = "wendy-lite:ble:1B2C3D4E-0000-0000-0000-000000000000"
	resolveLiteBLESelector = func(_ context.Context, got string) (models.ExternalDevice, bool, error) {
		if got != selector {
			t.Fatalf("selector changed: %q", got)
		}
		return models.ExternalDevice{ID: selector, ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "BLE", "address": "1B2C3D4E-0000-0000-0000-000000000000"}}, true, nil
	}
	findDeviceByIDFn = func(context.Context, string) *SelectedDevice {
		t.Fatal("opened provider/USB discovery for direct BLE selection")
		return nil
	}
	selected, err := resolveTarget(context.Background(), SelectDevice(selector), SuppressUpdateCheck())
	if err != nil || selected == nil || selected.Agent != nil || selected.External == nil || selected.External.ID != selector || selected.DefaultSelector != selector {
		t.Fatalf("direct selection %+v %v", selected, err)
	}
	if _, ok := selected.Provider.(*providers.MicroWendyProvider); !ok {
		t.Fatalf("provider %T", selected.Provider)
	}
	selected.Close()
}
