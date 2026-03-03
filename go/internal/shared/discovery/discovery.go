// Package discovery provides device discovery via mDNS and other transports.
package discovery

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const (
	// wendyServiceType is the mDNS service type advertised by WendyOS devices.
	wendyServiceType = "_wendyos._udp"

	// defaultTimeout is the default mDNS browse duration.
	defaultTimeout = 5 * time.Second
)

// DiscoveryOptions configures a device discovery scan.
type DiscoveryOptions struct {
	// Types limits discovery to the specified interface types.
	// An empty slice means discover all supported types.
	Types []models.InterfaceType

	// Timeout is the maximum duration for the discovery scan.
	// Zero uses the default timeout.
	Timeout time.Duration
}

// Discover runs device discovery across the requested interface types and returns
// all found devices.
func Discover(ctx context.Context, opts DiscoveryOptions) (*models.DevicesCollection, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	collection := &models.DevicesCollection{}

	shouldDiscover := func(t models.InterfaceType) bool {
		if len(opts.Types) == 0 {
			return true
		}
		for _, ot := range opts.Types {
			if ot == t {
				return true
			}
		}
		return false
	}

	if shouldDiscover(models.InterfaceUSB) {
		if devices, err := discoverUSB(ctx); err == nil {
			collection.USBDevices = devices
		}
	}

	if shouldDiscover(models.InterfaceEthernet) {
		if devices, err := discoverEthernet(ctx); err == nil {
			collection.EthernetInterfaces = devices
		}
	}

	if shouldDiscover(models.InterfaceLAN) {
		if devices, err := discoverLAN(ctx, timeout); err == nil {
			collection.LANDevices = devices
		}
	}

	if shouldDiscover(models.InterfaceBluetooth) {
		// Use active scanning when bluetooth is explicitly requested or
		// discovering all types. The scan takes ~5 seconds on Linux.
		activeScan := len(opts.Types) == 0 || len(opts.Types) == 1
		if devices, err := discoverBluetooth(ctx, activeScan); err == nil {
			collection.BluetoothDevices = devices
		}
	}

	return collection, nil
}
