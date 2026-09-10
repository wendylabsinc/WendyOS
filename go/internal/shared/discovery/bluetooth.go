package discovery

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

const (
	// wendyBLEServiceUUID is the 128-bit service UUID advertised by WendyOS
	// agent BLE devices. Currently unreferenced — kept as the protocol fact a
	// reimplementation on internal/shared/ble would need.
	wendyBLEServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b631"

	// wendyL2CAPPSM is the L2CAP PSM used for gRPC-over-BLE. Currently
	// unreferenced — kept as the protocol fact a reimplementation on
	// internal/shared/ble would need.
	wendyL2CAPPSM = 128
)

// errBluetoothDiscoveryDisabled is returned by discoverBluetooth: the legacy
// one-shot Bluetooth scanner is disabled — to be removed or reimplemented
// with the reference BLE API (internal/shared/ble/scan +
// internal/shared/ble/central), the stack BLELiteDeviceDiscoverContinuous
// (ble_lite_discovery.go) already uses.
var errBluetoothDiscoveryDisabled = errors.New("Bluetooth discovery is currently disabled")

// discoverBluetooth is disabled — to be removed or reimplemented with the
// reference BLE API (internal/shared/ble/scan + internal/shared/ble/central).
// See BLELiteDeviceDiscoverContinuous (ble_lite_discovery.go), which already
// scans through that stack.
func discoverBluetooth(_ context.Context, _ bool) ([]models.BluetoothDevice, error) {
	return nil, errBluetoothDiscoveryDisabled
}
