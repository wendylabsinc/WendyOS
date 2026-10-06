package commands

import (
	"fmt"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// lanBrowseErrors keeps the first mDNS backend error a discovery session
// reports through discovery.StreamOptions.OnBackendError. Safe for
// concurrent use: the discovery engine calls record from its own goroutine.
type lanBrowseErrors struct {
	mu  sync.Mutex
	err error
}

func (b *lanBrowseErrors) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = err
	}
}

func (b *lanBrowseErrors) first() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// lanBrowseOutcome decides what a failed mDNS browse means for a scan.
//   - No browse error: nothing to say.
//   - Devices were still found (USB-direct probe, providers): a one-line
//     stderr warning; the result stands but may be incomplete.
//   - Nothing found: an error. An empty list would read as "no devices on
//     this network" when the scan never actually listened — the usual cause
//     is a sandbox, or a missing macOS Local Network permission.
func lanBrowseOutcome(c *models.DevicesCollection, browseErr error) (warning string, err error) {
	if browseErr == nil {
		return "", nil
	}
	if foundWendyTargets(c) {
		return fmt.Sprintf("Warning: local network (mDNS) discovery failed: %v; the device list may be incomplete.", browseErr), nil
	}
	return "", fmt.Errorf("local network (mDNS) discovery failed (%v) and no devices were found. "+
		"If wendy is running inside a sandbox or an app without Local Network permission, allow local network access "+
		"(macOS: System Settings > Privacy & Security > Local Network) or run it outside the sandbox; "+
		"or connect directly with --device <address>", browseErr)
}

// foundWendyTargets reports whether c holds anything besides the local run
// targets (this machine, Docker, Apple Container) that JSON output always
// lists, whether or not the network scan saw anything.
func foundWendyTargets(c *models.DevicesCollection) bool {
	if c == nil {
		return false
	}
	if len(c.USBDevices)+len(c.LANDevices)+len(c.BluetoothDevices)+len(c.EthernetInterfaces)+len(c.Simulators) > 0 {
		return true
	}
	for _, d := range c.ExternalDevices {
		if !providers.IsLocalProviderKey(d.ProviderKey) {
			return true
		}
	}
	return false
}
