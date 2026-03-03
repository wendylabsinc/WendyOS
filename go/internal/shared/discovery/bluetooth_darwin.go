//go:build darwin

package discovery

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework CoreBluetooth -framework Foundation
#include "bluetooth_darwin.h"
*/
import "C"

import (
	"context"
	"sort"
	"unsafe"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const (
	wendyBLEServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b631"
	wendyL2CAPPSM       = 128
)

// discoverBluetooth uses CoreBluetooth via CGo to scan for WendyOS BLE
// peripherals on macOS. It scans for devices advertising the Wendy service
// UUID and returns them sorted by RSSI (strongest first).
func discoverBluetooth(ctx context.Context, activeScan bool) ([]models.BluetoothDevice, error) {
	scanSeconds := 5
	if !activeScan {
		scanSeconds = 3
	}

	// Run the CoreBluetooth scan (blocks for scanSeconds).
	result := C.wendy_ble_scan(C.int(scanSeconds))
	defer C.wendy_ble_free_result(result)

	if result.count == 0 || result.devices == nil {
		return nil, nil
	}

	count := int(result.count)
	cDevices := unsafe.Slice(result.devices, count)

	devices := make([]models.BluetoothDevice, 0, count)
	for _, cd := range cDevices {
		psm := uint16(wendyL2CAPPSM)
		displayName := C.GoString(cd.name)
		if cd.is_lite != 0 {
			psm = 0
			if displayName == "" {
				displayName = "Wendy Lite"
			}
		}
		devices = append(devices, models.BluetoothDevice{
			ID:            C.GoString(cd.uuid),
			DisplayName:   displayName,
			Address:       C.GoString(cd.uuid),
			RSSI:          int(cd.rssi),
			IsWendyDevice: true,
			L2CAPPSM:      psm,
		})
	}

	// Sort by RSSI descending (strongest signal first).
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].RSSI > devices[j].RSSI
	})

	return devices, nil
}
