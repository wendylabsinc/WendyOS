package ble

import (
	"fmt"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// Wendy Lite (ESP32/uwasm) BLE provisioning UUIDs.
// Base: 4E57454E-4459-0001-xxxx-000000000000 ("NWENDY" + service 0001).
const (
	liteServiceUUID  = "4E57454E-4459-0001-0000-000000000000"
	liteSSIDCharUUID = "4E57454E-4459-0001-0001-000000000000"
	litePassCharUUID = "4E57454E-4459-0001-0002-000000000000"
	liteCmdCharUUID  = "4E57454E-4459-0001-0003-000000000000"
	liteStatusUUID   = "4E57454E-4459-0001-0004-000000000000"
	liteDevNameUUID  = "4E57454E-4459-0001-0005-000000000000"
)

// Wendy Lite command bytes
const (
	liteCmdConnect    = 0x01
	liteCmdClearCreds = 0x02
)

// Wendy Lite status values
const (
	liteStatusNoCreds    = 0x00
	liteStatusConnecting = 0x01
	liteStatusConnected  = 0x02
	liteStatusFailed     = 0x03
)

// LiteClient communicates with a Wendy Lite (ESP32) device over BLE GATT
// for WiFi provisioning.
type LiteClient struct {
	conn *Connection
}

// ConnectLite establishes a BLE connection to a Wendy Lite device and
// discovers its GATT services.
func ConnectLite(device *models.BluetoothDevice) (*LiteClient, error) {
	conn, err := Connect(device.Address, 10)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", device.DisplayName, err)
	}

	if err := conn.DiscoverServices(10); err != nil {
		conn.Close()
		return nil, fmt.Errorf("discovering services: %w", err)
	}

	// Validate that the Wendy Lite provisioning service is present.
	if !conn.HasService(liteServiceUUID) {
		services := conn.ListServices()
		conn.Close()
		return nil, fmt.Errorf("device %s does not expose the Wendy Lite provisioning service (expected %s); discovered services: [%s]",
			device.DisplayName, liteServiceUUID, services)
	}

	return &LiteClient{conn: conn}, nil
}

// Close disconnects from the Wendy Lite device.
func (c *LiteClient) Close() {
	c.conn.Close()
}

// DeviceName reads the device name characteristic.
func (c *LiteClient) DeviceName() (string, error) {
	data, err := c.conn.ReadCharacteristic(liteServiceUUID, liteDevNameUUID)
	if err != nil {
		return "", fmt.Errorf("reading device name: %w", err)
	}
	return string(data), nil
}

// WifiProvisionResult holds the result of a WiFi provisioning attempt.
type WifiProvisionResult struct {
	Connected bool
	IPAddress string // set when Connected is true
}

// WifiConnect provisions WiFi credentials on the Wendy Lite device and
// waits for it to connect. The protocol is:
//  1. Write SSID to SSID characteristic
//  2. Write password to password characteristic
//  3. Write 0x01 to command characteristic
//  4. Subscribe to status characteristic and wait for CONNECTED or FAILED
func (c *LiteClient) WifiConnect(ssid, password string) (*WifiProvisionResult, error) {
	// Step 1: Write SSID
	if err := c.conn.WriteCharacteristic(liteServiceUUID, liteSSIDCharUUID, []byte(ssid)); err != nil {
		return nil, fmt.Errorf("writing SSID: %w", err)
	}

	// Step 2: Write password
	if err := c.conn.WriteCharacteristic(liteServiceUUID, litePassCharUUID, []byte(password)); err != nil {
		return nil, fmt.Errorf("writing password: %w", err)
	}

	// Step 3: Subscribe to status notifications before sending command
	if err := c.conn.Subscribe(liteServiceUUID, liteStatusUUID); err != nil {
		return nil, fmt.Errorf("subscribing to status: %w", err)
	}

	// Step 4: Send connect command
	if err := c.conn.WriteCharacteristic(liteServiceUUID, liteCmdCharUUID, []byte{liteCmdConnect}); err != nil {
		return nil, fmt.Errorf("writing connect command: %w", err)
	}

	// Step 5: Wait for status updates (timeout 30 seconds total)
	for i := 0; i < 6; i++ {
		data, err := c.conn.WaitNotification(liteServiceUUID, liteStatusUUID, 5)
		if err != nil {
			// On timeout, read the status characteristic directly
			data, err = c.conn.ReadCharacteristic(liteServiceUUID, liteStatusUUID)
			if err != nil {
				continue
			}
		}

		if len(data) == 0 {
			continue
		}

		status := data[0]
		switch status {
		case liteStatusConnected:
			result := &WifiProvisionResult{Connected: true}
			if len(data) > 1 {
				result.IPAddress = string(data[1:])
			}
			return result, nil

		case liteStatusFailed:
			return &WifiProvisionResult{Connected: false}, nil

		case liteStatusConnecting:
			// Still connecting, keep waiting
			continue

		case liteStatusNoCreds:
			return nil, fmt.Errorf("device reports no credentials stored")
		}
	}

	return nil, fmt.Errorf("timed out waiting for WiFi connection status")
}

// WifiStatus reads the current WiFi status from the device.
func (c *LiteClient) WifiStatus() (*WifiProvisionResult, error) {
	data, err := c.conn.ReadCharacteristic(liteServiceUUID, liteStatusUUID)
	if err != nil {
		return nil, fmt.Errorf("reading status: %w", err)
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("empty status response")
	}

	result := &WifiProvisionResult{}
	switch data[0] {
	case liteStatusConnected:
		result.Connected = true
		if len(data) > 1 {
			result.IPAddress = string(data[1:])
		}
	case liteStatusConnecting:
		// Still connecting
	case liteStatusFailed:
		// Failed
	case liteStatusNoCreds:
		// No credentials
	}
	return result, nil
}

// WifiClearCredentials clears stored WiFi credentials on the device.
func (c *LiteClient) WifiClearCredentials() error {
	return c.conn.WriteCharacteristic(liteServiceUUID, liteCmdCharUUID, []byte{liteCmdClearCreds})
}
