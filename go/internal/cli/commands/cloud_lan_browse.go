package commands

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

const cloudLANBrowseTimeout = 750 * time.Millisecond

var cloudLANStream = discovery.StreamLAN

type cloudLANBrowseKey struct{}

type cloudLANBrowse struct {
	done    chan struct{}
	updated chan struct{}
	mu      sync.Mutex
	devices []models.LANDevice
}

// Browse while Cloud resolves the selection. These are only routing hints;
// localCloudCandidates filters them after Cloud supplies the expected identity.
// Keep the browse deadline separate from the connection's lifetime.
func startCloudLANBrowse(ctx context.Context) (context.Context, context.CancelFunc) {
	if cloudTunnelOnly(ctx) {
		// A tunnel-only connection never uses LAN candidates.
		return ctx, func() {}
	}
	browseCtx, cancel := context.WithTimeout(ctx, cloudLANBrowseTimeout)
	browse := &cloudLANBrowse{done: make(chan struct{}), updated: make(chan struct{}, 1)}
	stream := cloudLANStream(browseCtx, discovery.StreamOptions{UseCache: true})
	go func() {
		defer close(browse.done)
		defer cancel()
		for event := range stream {
			if event.Kind == discovery.LANFound || event.Kind == discovery.LANUpdated {
				browse.mu.Lock()
				browse.devices = append(browse.devices, event.Device)
				browse.mu.Unlock()
				select {
				case browse.updated <- struct{}{}:
				default:
				}
			}
		}
	}()
	return context.WithValue(ctx, cloudLANBrowseKey{}, browse), func() {
		cancel()
		<-browse.done
	}
}

// seen offers each sighting collected so far, without waiting for more.
func (b *cloudLANBrowse) seen(accept func(models.LANDevice)) {
	b.mu.Lock()
	devices := slices.Clone(b.devices)
	b.mu.Unlock()
	for _, device := range devices {
		accept(device)
	}
}

// Reuse already collected results, or wait only until a matching sighting or
// the original browse deadline. A fast roster need not wait for a full scan.
func (b *cloudLANBrowse) visit(ctx context.Context, accept func(models.LANDevice) bool) {
	seen, finished := 0, false
	for {
		b.mu.Lock()
		devices := b.devices[seen:]
		seen = len(b.devices)
		b.mu.Unlock()
		for _, device := range devices {
			if accept(device) {
				return
			}
		}
		if finished {
			return
		}
		select {
		case <-b.done:
			finished = true // Read any final sightings before returning.
		case <-b.updated:
		case <-ctx.Done():
			return
		}
	}
}
