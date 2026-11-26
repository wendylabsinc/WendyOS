package twinrobot

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

// One immutable geometry model per robot avoids rebuilding height-layer grids
// during repeated matches. The independent mutex never blocks sensor ingress
// on the robot lock. Geometry content, not a room name or caller revision, is
// the key; an edited mesh must be revalidated and rebuilt.
type layeredGeometryCache struct {
	mu    sync.Mutex
	key   [sha256.Size]byte
	model *sitenav.LayeredRoom
}

func geometryDigest(architecture, furniture []sitenav.Triangle) [sha256.Size]byte {
	hash := sha256.New()
	var data [72]byte
	for _, mesh := range [][]sitenav.Triangle{architecture, furniture} {
		binary.LittleEndian.PutUint64(data[:8], uint64(len(mesh)))
		hash.Write(data[:8])
		for _, triangle := range mesh {
			for i, p := range triangle {
				binary.LittleEndian.PutUint64(data[i*24:], math.Float64bits(p.X))
				binary.LittleEndian.PutUint64(data[i*24+8:], math.Float64bits(p.Y))
				binary.LittleEndian.PutUint64(data[i*24+16:], math.Float64bits(p.Z))
			}
			hash.Write(data[:])
		}
	}
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	return digest
}

func (c *layeredGeometryCache) load(architecture, furniture []sitenav.Triangle) (sitenav.LayeredRoom, error) {
	key := geometryDigest(architecture, furniture)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.model != nil && c.key == key {
		return *c.model, nil
	}
	model, err := sitenav.NewLayeredRoom(architecture, furniture)
	if err != nil {
		return sitenav.LayeredRoom{}, err
	}
	c.key, c.model = key, &model
	return model, nil
}
