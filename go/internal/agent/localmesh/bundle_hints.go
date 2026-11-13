package localmesh

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
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
	peers map[meshid.Device]map[string]time.Time
}

func newBundleHintCache() *bundleHintCache {
	return &bundleHintCache{peers: make(map[meshid.Device]map[string]time.Time)}
}

func (h *bundleHintCache) Known(device meshid.Device, now time.Time) []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := h.peers[device]
	known := make([]string, 0, len(entries))
	for fp, written := range entries {
		if now.Before(written) || now.Sub(written) >= directoryBundleHintLifetime {
			delete(entries, fp)
		} else {
			known = append(known, fp)
		}
	}
	if len(entries) == 0 {
		delete(h.peers, device)
	}
	return known
}

func (h *bundleHintCache) Written(device meshid.Device, chain [][]byte, now time.Time) {
	if h == nil || device == (meshid.Device{}) {
		return
	}
	fp := Fingerprint(chain)
	if len(fp) != 64 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := h.peers[device]
	if entries == nil {
		if len(h.peers) >= maxDirectoryHintPeers {
			var oldestDevice meshid.Device
			var oldest time.Time
			for id, values := range h.peers {
				for _, written := range values {
					if oldest.IsZero() || written.Before(oldest) {
						oldestDevice, oldest = id, written
					}
				}
			}
			delete(h.peers, oldestDevice)
		}
		entries = make(map[string]time.Time)
		h.peers[device] = entries
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

// Fingerprint is the SHA-256 hex of a bundle's leaf certificate. It names a
// bundle inside one peer's hint set; it is never an identity or a proof.
func Fingerprint(chain [][]byte) string {
	if len(chain) == 0 {
		return ""
	}
	sum := sha256.Sum256(chain[0])
	return hex.EncodeToString(sum[:])
}
