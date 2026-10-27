package main

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sync"
)

// provisioningIdentityTracker detects live certificate replacement without
// retaining a second copy of the private key. The first observed identity is
// enrollment; a changed identity requires a whole-agent restart because the
// local mesh, app listener, carrier links, and TLS tickets share its lifetime.
type provisioningIdentityTracker struct {
	mu      sync.Mutex
	digest  [sha256.Size]byte
	started bool
}

func writeIdentityField(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}

func (t *provisioningIdentityTracker) Changed(cert, chain, key string, org, asset int32) bool {
	h := sha256.New()
	writeIdentityField(h, cert)
	writeIdentityField(h, chain)
	writeIdentityField(h, key)
	var ids [8]byte
	binary.BigEndian.PutUint32(ids[:4], uint32(org))
	binary.BigEndian.PutUint32(ids[4:], uint32(asset))
	_, _ = h.Write(ids[:])
	var next [sha256.Size]byte
	copy(next[:], h.Sum(nil))
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		t.digest, t.started = next, true
		return false
	}
	return t.digest != next
}
