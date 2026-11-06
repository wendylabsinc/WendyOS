package mtls

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"
)

const (
	peerPathCacheEntries = 256
	peerPathCacheBytes   = 8 << 20
	peerPathCacheAge     = time.Minute
)

// peerPathCache reuses only the cryptographic path proof for an exact chain.
// It belongs to one immutable roots/usage/floor verifier incarnation. Callers
// must still check current identity, time and revocation policy on every call.
// Neither a failed verification nor a device authorization is retained here.
type peerPathCache struct {
	mu         sync.Mutex
	entries    map[[sha256.Size]byte]*list.Element
	recency    list.List
	bytes      int
	maxEntries int
	maxBytes   int
	clock      func() peerPathClockSample
	highWater  time.Time // Actual wall clock, never a caller snapshot timestamp.
	rollback   bool
	generation uint64
}

type peerPathEntry struct {
	key                         [sha256.Size]byte
	chain                       [][]byte
	bytes                       int
	verifiedWall, verifiedClock time.Time
	verifiedEffective, expires  time.Time
}

// Separate fields let tests move wall time backwards while monotonic time
// continues forward, matching a real system-clock adjustment.
type peerPathClockSample struct {
	wall, monotonic time.Time
}

func newPeerPathCache() *peerPathCache {
	return &peerPathCache{entries: make(map[[sha256.Size]byte]*list.Element), maxEntries: peerPathCacheEntries, maxBytes: peerPathCacheBytes, clock: func() peerPathClockSample {
		now := time.Now()
		return peerPathClockSample{wall: now.Round(0), monotonic: now}
	}}
}

func samePeerChain(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func peerChainKey(chain [][]byte, limit int) (key [sha256.Size]byte, size int, ok bool) {
	if len(chain) == 0 || len(chain) > 32 {
		return key, 0, false
	}
	for _, der := range chain {
		// Include framing in the retained-byte budget and avoid integer overflow.
		if len(der) > limit-size-8 {
			return key, 0, false
		}
		size += len(der) + 8
	}
	h := sha256.New()
	var length [8]byte
	for _, der := range chain {
		binary.BigEndian.PutUint64(length[:], uint64(len(der)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(der)
	}
	h.Sum(key[:0])
	return key, size, true
}

func (c *peerPathCache) remove(element *list.Element) {
	entry := element.Value.(peerPathEntry)
	delete(c.entries, entry.key)
	c.recency.Remove(element)
	c.bytes -= entry.bytes
}

// sampleLocked observes the actual clock under the same lock as highWater.
// Sampling outside the lock would mistake scheduler reordering for rollback.
// A generation prevents a verification that spans an observed rollback from
// inserting its result even if wall time catches up before it finishes.
func (c *peerPathCache) sampleLocked() (peerPathClockSample, bool) {
	now := c.clock()
	now.wall = now.wall.Round(0)
	if now.wall.Before(c.highWater) {
		if !c.rollback {
			c.generation++
			clear(c.entries)
			c.recency.Init()
			c.bytes = 0
		}
		c.rollback = true
		return now, false
	}
	c.highWater = now.wall
	c.rollback = false
	return now, true
}

// observe includes calls rejected before path verification. Caller-provided
// snapshot times are authorization inputs, not observations of the system clock.
func (c *peerPathCache) observe() {
	c.mu.Lock()
	c.sampleLocked()
	c.mu.Unlock()
}

func (entry peerPathEntry) current(now peerPathClockSample) bool {
	age := now.monotonic.Sub(entry.verifiedClock)
	return age >= 0 && age < peerPathCacheAge && now.wall.Before(entry.expires)
}

// verify retains the caller's wall/effective bounds for the selected path.
// Older snapshots may need their own verification, but cannot evict a current
// proof or extend its fixed monotonic lifetime. Actual clock rollback disables
// reuse independently of snapshot ordering.
func (c *peerPathCache) verify(chain [][]byte, wall, effective time.Time, verify func([][]byte, time.Time) (time.Time, error)) (time.Time, error) {
	wall, effective = wall.Round(0), effective.Round(0)
	key, size, cacheable := peerChainKey(chain, c.maxBytes)
	cacheable = cacheable && c.maxEntries > 0
	c.mu.Lock()
	started, canReuse := c.sampleLocked()
	generation := c.generation
	if element := c.entries[key]; cacheable && element != nil {
		entry := element.Value.(peerPathEntry)
		if !entry.current(started) {
			c.remove(element)
		} else if canReuse && !wall.Before(entry.verifiedWall) && !effective.Before(entry.verifiedEffective) &&
			wall.Before(entry.expires) && effective.Before(entry.expires) && samePeerChain(chain, entry.chain) {
			c.recency.MoveToFront(element)
			c.mu.Unlock()
			return entry.expires, nil
		}
	}
	c.mu.Unlock()
	if !cacheable {
		return verify(chain, wall)
	}
	// Verification and the stored proof use the same owned bytes. A caller's
	// later reuse of its input buffers cannot mutate an accepted cache entry.
	copyChain := make([][]byte, len(chain))
	for i, der := range chain {
		copyChain[i] = bytes.Clone(der)
	}
	expires, err := verify(copyChain, wall)
	if err != nil {
		return time.Time{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	finished, stillCurrent := c.sampleLocked()
	age := finished.monotonic.Sub(started.monotonic)
	if !canReuse || !stillCurrent || generation != c.generation || !finished.wall.Before(expires) ||
		!wall.Before(expires) || !effective.Before(expires) || age < 0 || age >= peerPathCacheAge {
		return expires, nil
	}
	if previous := c.entries[key]; previous != nil {
		if previous.Value.(peerPathEntry).current(finished) {
			// A concurrent verification already installed a current proof. Keep
			// its time bounds and deadline, regardless of completion ordering.
			return expires, nil
		}
		c.remove(previous)
	}
	for c.recency.Len() >= c.maxEntries || c.bytes+size > c.maxBytes {
		c.remove(c.recency.Back())
	}
	entry := peerPathEntry{key: key, chain: copyChain, bytes: size, verifiedWall: wall, verifiedClock: started.monotonic, verifiedEffective: effective, expires: expires}
	c.entries[key] = c.recency.PushFront(entry)
	c.bytes += size
	return expires, nil
}
