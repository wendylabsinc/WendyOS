package livefusion

import (
	"bytes"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

const (
	maxNativeMediaEntries = 16
	maxNativeMediaBytes   = 16 << 20
)

type mediaCacheKey struct {
	source serviceaccess.Source
	ref    sensing.EvidenceRef
}

type cachedMedia struct {
	key      mediaCacheKey
	data     []byte
	retained int64
}

// The enclosing native Session fixes tenant/site and serializes all access.
// Entries own immutable bytes, never capture proofs, authority or inference.
// The oldest unused entry is evicted within both fixed bounds.
type nativeMediaCache struct {
	entries []cachedMedia
	bytes   int
}

func (c *nativeMediaCache) get(key mediaCacheKey) ([]byte, int64, bool) {
	if c != nil {
		for i, entry := range c.entries {
			if entry.key == key {
				copy(c.entries[i:], c.entries[i+1:])
				c.entries[len(c.entries)-1] = entry
				return bytes.Clone(entry.data), entry.retained, true
			}
		}
	}
	return nil, 0, false
}

func (c *nativeMediaCache) remove(key mediaCacheKey) {
	if c != nil {
		for i, entry := range c.entries {
			if entry.key == key {
				c.bytes -= len(entry.data)
				copy(c.entries[i:], c.entries[i+1:])
				c.entries[len(c.entries)-1] = cachedMedia{}
				c.entries = c.entries[:len(c.entries)-1]
				return
			}
		}
	}
}

// Called only after complete chunk, digest and authenticated retention checks.
func (c *nativeMediaCache) put(key mediaCacheKey, data []byte, retained int64) {
	if c == nil || len(data) == 0 || len(data) > maxNativeMediaBytes {
		return
	}
	c.remove(key)
	for len(c.entries) >= maxNativeMediaEntries || c.bytes+len(data) > maxNativeMediaBytes {
		c.remove(c.entries[0].key)
	}
	c.entries = append(c.entries, cachedMedia{key: key, data: bytes.Clone(data), retained: retained})
	c.bytes += len(data)
}
