package providers

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble"
)

func TestPhysicalLiteBLESelectorReadsOnlySelectedGATTInfo(t *testing.T) {
	oldRead, oldPermission := readSelectedLiteBLEInfo, preflightSelectedLiteBLE
	t.Cleanup(func() { readSelectedLiteBLEInfo, preflightSelectedLiteBLE = oldRead, oldPermission })
	const address = "1B2C3D4E-0000-0000-0000-000000000000"
	const tenant = "11111111-1111-4111-8111-111111111111"
	const device = "22222222-2222-4222-8222-222222222222"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	permissions, reads := 0, 0
	preflightSelectedLiteBLE = func(got context.Context) error {
		if got != ctx {
			t.Fatal("selection lost caller context")
		}
		permissions++
		return nil
	}
	readSelectedLiteBLEInfo = func(got string, budget time.Duration) (*ble.LiteInfo, error) {
		if got != address || budget <= 0 || budget > 3*time.Second {
			t.Fatalf("probe %q %v", got, budget)
		}
		reads++
		return &ble.LiteInfo{PSM: 128, MTLSEnabled: true, DeviceID: "board-c6", DisplayName: "C6", Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device}, nil
	}
	selector := "wendy-lite:ble:" + address
	selected, matched, err := ResolveLiteBLESelector(ctx, selector)
	if err != nil || !matched {
		t.Fatalf("selection %v %v", matched, err)
	}
	if selected.ID != selector || selected.ProviderKey != "wendy-lite" || selected.ConnectionType() != "BLE" || selected.ConnectionInfo["address"] != address || selected.ConnectionInfo["mtls"] != "true" || selected.ConnectionInfo["tenantId"] != tenant || selected.ConnectionInfo["enrolledDeviceId"] != device || selected.ConnectionInfo["psm"] != "128" {
		t.Fatalf("selection metadata changed: %+v", selected)
	}
	if permissions != 1 || reads != 1 {
		t.Fatalf("permission/probes %d/%d", permissions, reads)
	}
	// Existing other selector forms remain outside this direct BLE path.
	if _, matched, err = ResolveLiteBLESelector(ctx, "wendy-lite:board.local"); matched || err != nil || reads != 1 {
		t.Fatalf("unexpected unrelated selection %v %v", matched, err)
	}
}
