package providers

import (
	"context"
	"sync"
)

var (
	allProviders       []DeviceProvider
	availableProviders []DeviceProvider
	mu                 sync.RWMutex
)

func init() {
	allProviders = []DeviceProvider{
		&LocalProvider{},
		&DockerProvider{},
		&AndroidProvider{},
		&MicroWendyProvider{},
	}
}

// Initialize probes each registered provider and filters to those that are
// currently available. Call this once at CLI startup.
func Initialize(ctx context.Context) {
	mu.Lock()
	defer mu.Unlock()

	availableProviders = nil
	for _, p := range allProviders {
		if p.IsAvailable(ctx) {
			availableProviders = append(availableProviders, p)
		}
	}
}

// AvailableProviders returns the providers whose toolchains are present.
func AvailableProviders() []DeviceProvider {
	mu.RLock()
	defer mu.RUnlock()
	return availableProviders
}

// AllProviders returns all registered providers regardless of toolchain availability.
// Use this for device discovery where you want to find devices even if you can't
// build for them.
func AllProviders() []DeviceProvider {
	mu.RLock()
	defer mu.RUnlock()
	return allProviders
}

// ProviderForKey returns the available provider with the given key, or nil.
func ProviderForKey(key string) DeviceProvider {
	mu.RLock()
	defer mu.RUnlock()
	for _, p := range availableProviders {
		if p.Key() == key {
			return p
		}
	}
	return nil
}
