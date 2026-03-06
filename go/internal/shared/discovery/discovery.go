// Package discovery provides device discovery via mDNS and other transports.
package discovery

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const (
	// wendyServiceType is the mDNS service type advertised by WendyOS devices.
	wendyServiceType = "_wendyos._udp"

	// defaultTimeout is the default mDNS browse duration.
	defaultTimeout = 5 * time.Second
)

// silentLogger is a no-op logger used to suppress hashicorp/mdns log output.
var silentLogger = log.New(io.Discard, "", 0)

// DiscoveryOptions configures a device discovery scan.
type DiscoveryOptions struct {
	// Types limits discovery to the specified interface types.
	// An empty slice means discover all supported types.
	Types []models.InterfaceType

	// Timeout is the maximum duration for the discovery scan.
	// Zero uses the default timeout.
	Timeout time.Duration
}

// Discover runs device discovery across the requested interface types concurrently
// and returns all found devices.
func Discover(ctx context.Context, opts DiscoveryOptions) (*models.DevicesCollection, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	collection := &models.DevicesCollection{}
	var mu sync.Mutex
	var wg sync.WaitGroup

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
		wg.Add(1)
		go func() {
			defer wg.Done()
			if devices, err := discoverUSB(ctx); err == nil {
				mu.Lock()
				collection.USBDevices = devices
				mu.Unlock()
			}
		}()
	}

	if shouldDiscover(models.InterfaceEthernet) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if devices, err := discoverEthernet(ctx); err == nil {
				mu.Lock()
				collection.EthernetInterfaces = devices
				mu.Unlock()
			}
		}()
	}

	if shouldDiscover(models.InterfaceLAN) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if devices, err := discoverLAN(ctx, timeout); err == nil {
				mu.Lock()
				collection.LANDevices = devices
				mu.Unlock()
			}
		}()
	}

	if shouldDiscover(models.InterfaceBluetooth) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			activeScan := len(opts.Types) == 0 || len(opts.Types) == 1
			if devices, err := discoverBluetooth(ctx, activeScan); err == nil {
				mu.Lock()
				collection.BluetoothDevices = devices
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return collection, nil
}

// DiscoverUSB discovers USB-connected Wendy devices.
func DiscoverUSB(ctx context.Context) ([]models.USBDevice, error) {
	return discoverUSB(ctx)
}

// DiscoverEthernet discovers Ethernet interfaces connected to Wendy devices.
func DiscoverEthernet(ctx context.Context) ([]models.EthernetInterface, error) {
	return discoverEthernet(ctx)
}

// DiscoverLAN discovers Wendy devices via mDNS on the local network.
func DiscoverLAN(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}
	return discoverLAN(ctx, timeout)
}

// DiscoverBluetooth discovers Wendy devices via Bluetooth.
func DiscoverBluetooth(ctx context.Context, activeScan bool) ([]models.BluetoothDevice, error) {
	return discoverBluetooth(ctx, activeScan)
}

// DiscoverLANContinuous discovers LAN devices via mDNS continuously,
// sending each new device to ch as it's found. The scan runs until ctx
// is cancelled. The channel is closed when discovery stops.
func DiscoverLANContinuous(ctx context.Context, ch chan<- models.LANDevice) {
	discoverLANContinuous(ctx, ch)
}
