package backend

import (
	"context"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

// Discovery never owns the device-operation lock while scanning the network.
// A separate flight coalesces scans and is retired on account/lifecycle changes.
type nearbyDiscovery struct {
	generation uint64
	mu         sync.Mutex
	blocked    bool
	flight     *nearbyDiscoveryFlight
}
type nearbyDiscoveryFlight struct {
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	devices    []commands.DesktopDevice
	err        error
}

func (b *Backend) Nearby() ([]commands.DesktopDevice, error) {
	d := &b.discovery
	d.mu.Lock()
	if d.blocked {
		d.mu.Unlock()
		return nil, context.Canceled
	}
	f := d.flight
	if f == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
		f = &nearbyDiscoveryFlight{ctx: ctx, cancel: cancel, done: make(chan struct{}), generation: d.generation}
		d.flight = f
		fetch := b.discoverNearby
		if fetch == nil {
			fetch = commands.DesktopNearby
		}
		go func() {
			devices, err := fetch(ctx)
			d.mu.Lock()
			defer d.mu.Unlock()
			if ctx.Err() != nil || d.flight != f {
				devices = nil
				err = context.Canceled
			}
			f.devices, f.err = devices, err
			if d.flight == f {
				d.flight = nil
			}
			close(f.done)
			cancel()
		}()
	}
	d.mu.Unlock()
	select {
	case <-f.done:
	case <-f.ctx.Done():
		// A successful fetch also cancels its timer. Prefer its published result.
		select {
		case <-f.done:
		default:
			return nil, f.ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.blocked || f.generation != d.generation {
		return nil, context.Canceled
	}
	return append([]commands.DesktopDevice(nil), f.devices...), f.err
}

func (b *Backend) resetNearbyDiscovery(blocked bool) {
	d := &b.discovery
	d.mu.Lock()
	defer d.mu.Unlock()
	d.blocked = blocked
	d.generation++
	if d.flight != nil {
		d.flight.cancel()
		d.flight = nil
	}
}
func (b *Backend) allowNearbyDiscovery() {
	b.discovery.mu.Lock()
	b.discovery.blocked = false
	b.discovery.mu.Unlock()
}
