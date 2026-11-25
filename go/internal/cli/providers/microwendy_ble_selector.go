package providers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/permission"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

const liteBLESelectorPrefix = "wendy-lite:ble:"

var readSelectedLiteBLEInfo = ble.ReadLiteInfoAt
var preflightSelectedLiteBLE = permission.Preflight

// ResolveLiteBLESelector selects one physical BLE address without opening serial
// discovery or browsing other providers. GATT supplies the ordinary untrusted
// selection metadata; the existing provider still authenticates the TLS peer.
// On macOS the address is a CoreBluetooth UUID; elsewhere it is a BLE MAC.
func ResolveLiteBLESelector(ctx context.Context, selector string) (models.ExternalDevice, bool, error) {
	if !strings.HasPrefix(selector, liteBLESelectorPrefix) {
		return models.ExternalDevice{}, false, nil
	}
	address := strings.TrimPrefix(selector, liteBLESelectorPrefix)
	if address == "" {
		return models.ExternalDevice{}, true, fmt.Errorf("physical BLE selector requires an address after %s", liteBLESelectorPrefix)
	}
	if err := ctx.Err(); err != nil {
		return models.ExternalDevice{}, true, err
	}
	if err := preflightSelectedLiteBLE(ctx); err != nil {
		return models.ExternalDevice{}, true, err
	}
	timeout := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if err := ctx.Err(); err != nil {
		return models.ExternalDevice{}, true, err
	}
	info, err := readSelectedLiteBLEInfo(address, timeout)
	if err != nil {
		return models.ExternalDevice{}, true, fmt.Errorf("reading selected Wendy Lite BLE info: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return models.ExternalDevice{}, true, err
	}
	device := (&MicroWendyProvider{}).bleExternalDevice(discovery.BLELiteDevice{Address: address, Info: *info})
	device.ID = selector
	return device, true, nil
}
