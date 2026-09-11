// Package ipcam manages network cameras as a camera transport alongside the USB
// and CSI cameras handled in internal/agent/camera.
//
// A network camera differs from a local one in three ways, and those three
// differences are what this package exists to absorb: it is identified by MAC
// address rather than a device node, it needs credentials, and it has to be
// discovered on the network. Everything specific to them lives here, so the
// video service only gains branches.
package ipcam

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Device IDs for network cameras come from a reserved high band. The kernel's
// VIDEO_NUM_DEVICES bound is 256, so physical /dev/videoN nodes never reach 200
// in practice, and the band leaves room for 56 cameras.
//
// The allocated number is also the v4l2loopback node number a camera will get
// when container parity lands, so the ID a user learns now does not change.
const (
	// LoopbackBandStart is the first device number reserved for Wendy-managed
	// virtual cameras. ROS 2 cameras use 128-199; network cameras retain the
	// original 200-255 stable-ID band.
	LoopbackBandStart = 128
	IDBandStart       = 200
	IDBandEnd         = 255
	// MCU / remote-source cameras get their own band above the IP band.
	MCUBandStart = 256
	MCUBandEnd   = 319
)

// ErrBandExhausted is returned when every ID in the reserved band is taken.
var ErrBandExhausted = errors.New("no free network camera device IDs")

// Camera is a known network camera. MAC is the identity: addresses move when a
// lease changes, so nothing else is stable enough to key on.
type Camera struct {
	MAC        string    `json:"mac"`
	ID         uint32    `json:"id"`
	Address    string    `json:"address"`
	Model      string    `json:"model"`
	ONVIFAddr  string    `json:"onvifAddr,omitempty"`
	StreamSub  string    `json:"streamSub,omitempty"`
	StreamMain string    `json:"streamMain,omitempty"`
	Link       string    `json:"link,omitempty"`
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`

	// Online reflects the most recent probe and is deliberately not persisted:
	// a camera reloaded from disk is offline until something reaches it.
	Online bool `json:"-"`
}

// Registry is the persistent set of known network cameras, keyed by MAC.
type Registry struct {
	path string

	mu sync.Mutex
	by map[string]Camera // MAC -> camera

	// now is an injection seam so tests get deterministic timestamps.
	now func() time.Time
}

// NewRegistry returns a registry backed by path. Call Load before use.
func NewRegistry(path string) *Registry {
	return &Registry{
		path: path,
		by:   make(map[string]Camera),
		now:  time.Now,
	}
}

// Load reads the registry from disk. A missing file is not an error: it is the
// normal state of a device that has never seen a network camera.
func (r *Registry) Load() error {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading camera registry: %w", err)
	}
	var cameras []Camera
	if err := json.Unmarshal(data, &cameras); err != nil {
		return fmt.Errorf("parsing camera registry: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range cameras {
		c.Online = false
		r.by[c.MAC] = c
	}
	return nil
}

// save writes the registry atomically. Callers hold r.mu.
func (r *Registry) save() error {
	cameras := make([]Camera, 0, len(r.by))
	for _, c := range r.by {
		cameras = append(cameras, c)
	}
	sort.Slice(cameras, func(i, j int) bool { return cameras[i].ID < cameras[j].ID })
	data, err := json.MarshalIndent(cameras, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding camera registry: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("creating camera registry directory: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing camera registry: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("replacing camera registry: %w", err)
	}
	return nil
}

// allocateID returns the lowest free ID in the band. Callers hold r.mu.
// Reusing freed IDs keeps numbers small and predictable across forget/re-add.
func (r *Registry) allocateID() (uint32, error) {
	taken := make(map[uint32]bool, len(r.by))
	for _, c := range r.by {
		taken[c.ID] = true
	}
	for id := uint32(IDBandStart); id <= IDBandEnd; id++ {
		if !taken[id] {
			return id, nil
		}
	}
	return 0, ErrBandExhausted
}

// Upsert records a camera, allocating an ID the first time its MAC is seen.
//
// Zero-valued fields on c do not erase what is already known: discovery probes
// return different subsets of a camera's details, and a later probe should not
// blank what an earlier one found.
func (r *Registry) Upsert(c Camera) (Camera, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	existing, found := r.by[c.MAC]
	if !found {
		id, err := r.allocateID()
		if err != nil {
			return Camera{}, err
		}
		c.ID = id
		c.FirstSeen = now
		c.LastSeen = now
		c.Online = true
		r.by[c.MAC] = c
		if err := r.save(); err != nil {
			delete(r.by, c.MAC)
			return Camera{}, err
		}
		return c, nil
	}

	merged := existing
	merged.LastSeen = now
	merged.Online = true
	if c.Address != "" {
		merged.Address = c.Address
	}
	if c.Model != "" {
		merged.Model = c.Model
	}
	if c.ONVIFAddr != "" {
		merged.ONVIFAddr = c.ONVIFAddr
	}
	if c.StreamSub != "" {
		merged.StreamSub = c.StreamSub
	}
	if c.StreamMain != "" {
		merged.StreamMain = c.StreamMain
	}
	if c.Link != "" {
		merged.Link = c.Link
	}
	r.by[c.MAC] = merged
	if err := r.save(); err != nil {
		r.by[c.MAC] = existing
		return Camera{}, err
	}
	return merged, nil
}

// List returns known cameras ordered by ID.
func (r *Registry) List() []Camera {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Camera, 0, len(r.by))
	for _, c := range r.by {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns the camera with the given device ID.
func (r *Registry) Get(id uint32) (Camera, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.by {
		if c.ID == id {
			return c, true
		}
	}
	return Camera{}, false
}

// Forget removes a camera. It reports whether the ID was known.
func (r *Registry) Forget(id uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for mac, c := range r.by {
		if c.ID != id {
			continue
		}
		delete(r.by, mac)
		if err := r.save(); err != nil {
			r.by[mac] = c
			return false
		}
		return true
	}
	return false
}

// MarkSeen updates liveness and address for a camera already in the registry.
// Unknown MACs are ignored: registration goes through Upsert, which allocates.
func (r *Registry) MarkSeen(mac, address string, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, found := r.by[mac]
	if !found {
		return
	}
	c.Online = online
	if address != "" {
		c.Address = address
	}
	if online {
		c.LastSeen = r.now()
	}
	r.by[mac] = c
	// Liveness is refreshed constantly, so a failed write is not worth
	// propagating: the next round retries.
	_ = r.save()
}
