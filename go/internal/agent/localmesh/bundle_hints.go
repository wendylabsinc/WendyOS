package localmesh

import (
	"sync"
	"time"
)

const (
	directoryBundleHintLifetime = 30 * time.Minute
	maxDirectoryHintPeers       = 128
	maxDirectoryHintsPerPeer    = 256
)

// bundleHintCache records only certificate bundles successfully written to an
// authenticated peer. It is scoped to one Node and therefore to its current
// credentials and trust. A hint is an optimization, never proof of identity.
type bundleHintCache struct {
	mu    sync.Mutex
	peers map[int32]map[string]time.Time
}

func newBundleHintCache() *bundleHintCache {
	return &bundleHintCache{peers: make(map[int32]map[string]time.Time)}
}

func (h *bundleHintCache) Known(asset int32, now time.Time) []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := h.peers[asset]
	known := make([]string, 0, len(entries))
	for fp, written := range entries {
		if now.Before(written) || now.Sub(written) >= directoryBundleHintLifetime {
			delete(entries, fp)
		} else {
			known = append(known, fp)
		}
	}
	if len(entries) == 0 {
		delete(h.peers, asset)
	}
	return known
}

func (h *bundleHintCache) Written(asset int32, chain [][]byte, now time.Time) {
	if h == nil || asset <= 0 {
		return
	}
	fp := Fingerprint(chain)
	if len(fp) != 64 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := h.peers[asset]
	if entries == nil {
		if len(h.peers) >= maxDirectoryHintPeers {
			var oldestAsset int32
			var oldest time.Time
			for id, values := range h.peers {
				for _, written := range values {
					if oldest.IsZero() || written.Before(oldest) {
						oldestAsset, oldest = id, written
					}
				}
			}
			delete(h.peers, oldestAsset)
		}
		entries = make(map[string]time.Time)
		h.peers[asset] = entries
	}
	if _, exists := entries[fp]; !exists && len(entries) >= maxDirectoryHintsPerPeer {
		var oldestFP string
		var oldest time.Time
		for key, written := range entries {
			if oldest.IsZero() || written.Before(oldest) {
				oldestFP, oldest = key, written
			}
		}
		delete(entries, oldestFP)
	}
	entries[fp] = now
}
