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
// all found devices. Currently only LAN (mDNS) discovery is implemented.
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

	if shouldDiscover(models.InterfaceLAN) {
		devices, err := discoverLAN(ctx, timeout)
		if err != nil {
			return collection, nil // return empty rather than failing
		}
		collection.LANDevices = devices
	}

	return collection, nil
}
