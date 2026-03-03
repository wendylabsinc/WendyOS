//go:build darwin

package ble

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework CoreBluetooth -framework Foundation
#include <stdlib.h>
#include "ble_darwin.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Connection wraps a CoreBluetooth connection to a BLE peripheral.
type Connection struct {
	handle C.WendyBLEConn
}

// Connect establishes a BLE connection to the peripheral identified by its UUID.
func Connect(peripheralUUID string, timeoutSeconds int) (*Connection, error) {
	cUUID := C.CString(peripheralUUID)
	defer C.free(unsafe.Pointer(cUUID))

	var errCode C.WendyBLEError
	handle := C.wendy_ble_connect(cUUID, C.int(timeoutSeconds), &errCode)
	if handle == nil {
		return nil, bleError(errCode, "connecting to peripheral")
	}
	return &Connection{handle: handle}, nil
}

// DiscoverServices discovers all services and characteristics on the peripheral.
func (c *Connection) DiscoverServices(timeoutSeconds int) error {
	err := C.wendy_ble_discover_services(c.handle, C.int(timeoutSeconds))
	if err != C.WENDY_BLE_OK {
		return bleError(err, "discovering services")
	}
	return nil
}

// WriteCharacteristic writes data to a GATT characteristic with response.
func (c *Connection) WriteCharacteristic(serviceUUID, charUUID string, data []byte) error {
	cSvc := C.CString(serviceUUID)
	defer C.free(unsafe.Pointer(cSvc))
	cChr := C.CString(charUUID)
	defer C.free(unsafe.Pointer(cChr))

	var cData *C.uint8_t
	if len(data) > 0 {
		cData = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}
	err := C.wendy_ble_write_characteristic(c.handle, cSvc, cChr, cData, C.int(len(data)))
	if err != C.WENDY_BLE_OK {
		return bleError(err, "writing characteristic")
	}
	return nil
}

// WriteCharacteristicNoResponse writes data to a GATT characteristic without response.
func (c *Connection) WriteCharacteristicNoResponse(serviceUUID, charUUID string, data []byte) error {
	cSvc := C.CString(serviceUUID)
	defer C.free(unsafe.Pointer(cSvc))
	cChr := C.CString(charUUID)
	defer C.free(unsafe.Pointer(cChr))

	var cData *C.uint8_t
	if len(data) > 0 {
		cData = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}
	err := C.wendy_ble_write_characteristic_no_response(c.handle, cSvc, cChr, cData, C.int(len(data)))
	if err != C.WENDY_BLE_OK {
		return bleError(err, "writing characteristic (no response)")
	}
	return nil
}

// ReadCharacteristic reads data from a GATT characteristic.
func (c *Connection) ReadCharacteristic(serviceUUID, charUUID string) ([]byte, error) {
	cSvc := C.CString(serviceUUID)
	defer C.free(unsafe.Pointer(cSvc))
	cChr := C.CString(charUUID)
	defer C.free(unsafe.Pointer(cChr))

	result := C.wendy_ble_read_characteristic(c.handle, cSvc, cChr)
	if result.error != C.WENDY_BLE_OK {
		return nil, bleError(result.error, "reading characteristic")
	}
	defer C.wendy_ble_free_data(result.data)

	if result.length == 0 || result.data == nil {
		return nil, nil
	}
	return C.GoBytes(unsafe.Pointer(result.data), result.length), nil
}

// Subscribe enables notifications for a GATT characteristic.
func (c *Connection) Subscribe(serviceUUID, charUUID string) error {
	cSvc := C.CString(serviceUUID)
	defer C.free(unsafe.Pointer(cSvc))
	cChr := C.CString(charUUID)
	defer C.free(unsafe.Pointer(cChr))

	err := C.wendy_ble_subscribe(c.handle, cSvc, cChr)
	if err != C.WENDY_BLE_OK {
		return bleError(err, "subscribing to characteristic")
	}
	return nil
}

// WaitNotification waits for a notification on a subscribed characteristic.
func (c *Connection) WaitNotification(serviceUUID, charUUID string, timeoutSeconds int) ([]byte, error) {
	cSvc := C.CString(serviceUUID)
	defer C.free(unsafe.Pointer(cSvc))
	cChr := C.CString(charUUID)
	defer C.free(unsafe.Pointer(cChr))

	result := C.wendy_ble_wait_notification(c.handle, cSvc, cChr, C.int(timeoutSeconds))
	if result.error != C.WENDY_BLE_OK {
		return nil, bleError(result.error, "waiting for notification")
	}
	defer C.wendy_ble_free_data(result.data)

	if result.length == 0 || result.data == nil {
		return nil, nil
	}
	return C.GoBytes(unsafe.Pointer(result.data), result.length), nil
}

// OpenL2CAP opens an L2CAP channel on the given PSM.
func (c *Connection) OpenL2CAP(psm uint16, timeoutSeconds int) error {
	err := C.wendy_ble_open_l2cap(c.handle, C.uint16_t(psm), C.int(timeoutSeconds))
	if err != C.WENDY_BLE_OK {
		return bleError(err, "opening L2CAP channel")
	}
	return nil
}

// L2CAPSend sends data over the L2CAP channel.
func (c *Connection) L2CAPSend(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	cData := (*C.uint8_t)(unsafe.Pointer(&data[0]))
	err := C.wendy_ble_l2cap_send(c.handle, cData, C.int(len(data)))
	if err != C.WENDY_BLE_OK {
		return bleError(err, "sending L2CAP data")
	}
	return nil
}

// L2CAPRecv receives data from the L2CAP channel.
func (c *Connection) L2CAPRecv(timeoutSeconds int) ([]byte, error) {
	result := C.wendy_ble_l2cap_recv(c.handle, C.int(timeoutSeconds))
	if result.error != C.WENDY_BLE_OK {
		return nil, bleError(result.error, "receiving L2CAP data")
	}
	defer C.wendy_ble_free_data(result.data)

	if result.length == 0 || result.data == nil {
		return nil, nil
	}
	return C.GoBytes(unsafe.Pointer(result.data), result.length), nil
}

// HasService checks whether a specific service UUID was discovered.
func (c *Connection) HasService(serviceUUID string) bool {
	cSvc := C.CString(serviceUUID)
	defer C.free(unsafe.Pointer(cSvc))
	return C.wendy_ble_has_service(c.handle, cSvc) == 1
}

// ListServices returns a comma-separated string of discovered service UUIDs.
func (c *Connection) ListServices() string {
	cStr := C.wendy_ble_list_services(c.handle)
	if cStr == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cStr))
	return C.GoString(cStr)
}

// Close disconnects and frees all BLE resources.
func (c *Connection) Close() {
	if c.handle != nil {
		C.wendy_ble_disconnect(c.handle)
		c.handle = nil
	}
}

func bleError(code C.WendyBLEError, context string) error {
	var msg string
	switch code {
	case C.WENDY_BLE_ERR_TIMEOUT:
		msg = "timeout"
	case C.WENDY_BLE_ERR_NOT_FOUND:
		msg = "not found"
	case C.WENDY_BLE_ERR_CONNECT_FAILED:
		msg = "connection failed"
	case C.WENDY_BLE_ERR_DISCOVER_FAILED:
		msg = "service discovery failed"
	case C.WENDY_BLE_ERR_WRITE_FAILED:
		msg = "write failed"
	case C.WENDY_BLE_ERR_READ_FAILED:
		msg = "read failed"
	case C.WENDY_BLE_ERR_L2CAP_FAILED:
		msg = "L2CAP channel failed"
	case C.WENDY_BLE_ERR_DISCONNECTED:
		msg = "disconnected"
	default:
		msg = "unknown error"
	}
	return fmt.Errorf("BLE %s: %s", context, msg)
}
