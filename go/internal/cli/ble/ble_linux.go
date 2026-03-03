//go:build linux

package ble

import "fmt"

// Connection wraps a BLE connection to a peripheral.
// On Linux, BLE client operations are not yet implemented.
type Connection struct{}

// Connect establishes a BLE connection to the peripheral identified by its address.
func Connect(peripheralAddress string, timeoutSeconds int) (*Connection, error) {
	return nil, fmt.Errorf("BLE client not yet implemented on Linux; use a LAN connection instead")
}

// DiscoverServices discovers all services and characteristics on the peripheral.
func (c *Connection) DiscoverServices(timeoutSeconds int) error {
	return fmt.Errorf("not implemented")
}

// WriteCharacteristic writes data to a GATT characteristic with response.
func (c *Connection) WriteCharacteristic(serviceUUID, charUUID string, data []byte) error {
	return fmt.Errorf("not implemented")
}

// WriteCharacteristicNoResponse writes data to a GATT characteristic without response.
func (c *Connection) WriteCharacteristicNoResponse(serviceUUID, charUUID string, data []byte) error {
	return fmt.Errorf("not implemented")
}

// ReadCharacteristic reads data from a GATT characteristic.
func (c *Connection) ReadCharacteristic(serviceUUID, charUUID string) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

// Subscribe enables notifications for a GATT characteristic.
func (c *Connection) Subscribe(serviceUUID, charUUID string) error {
	return fmt.Errorf("not implemented")
}

// WaitNotification waits for a notification on a subscribed characteristic.
func (c *Connection) WaitNotification(serviceUUID, charUUID string, timeoutSeconds int) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

// OpenL2CAP opens an L2CAP channel on the given PSM.
func (c *Connection) OpenL2CAP(psm uint16, timeoutSeconds int) error {
	return fmt.Errorf("not implemented")
}

// L2CAPSend sends data over the L2CAP channel.
func (c *Connection) L2CAPSend(data []byte) error {
	return fmt.Errorf("not implemented")
}

// L2CAPRecv receives data from the L2CAP channel.
func (c *Connection) L2CAPRecv(timeoutSeconds int) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

// HasService checks whether a specific service UUID was discovered.
func (c *Connection) HasService(serviceUUID string) bool { return false }

// ListServices returns a comma-separated string of discovered service UUIDs.
func (c *Connection) ListServices() string { return "" }

// Close disconnects and frees all BLE resources.
func (c *Connection) Close() {}
