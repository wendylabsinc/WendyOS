//go:build windows

package discovery

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

func discoverUSB(_ context.Context) ([]models.USBDevice, error) {
	return nil, nil
}

func discoverEthernet(_ context.Context) ([]models.EthernetInterface, error) {
	return nil, nil
}

func discoverLAN(_ context.Context, _ time.Duration) ([]models.LANDevice, error) {
	return nil, nil
}

func discoverBluetooth(_ context.Context, _ bool) ([]models.BluetoothDevice, error) {
	return nil, nil
}

// BrowseMDNSServices is not yet implemented on Windows.
func BrowseMDNSServices(_ context.Context, _ string, _ time.Duration) ([]MDNSService, error) {
	return nil, nil
}
